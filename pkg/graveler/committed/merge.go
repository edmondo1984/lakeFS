package committed

import (
	"bytes"
	"context"
	"fmt"

	"github.com/treeverse/lakefs/pkg/graveler"
	"github.com/treeverse/lakefs/pkg/logging"
)

type merger struct {
	ctx    context.Context
	logger logging.Logger

	writer               MetaRangeWriter
	base                 Iterator
	source               Iterator
	dest                 Iterator
	haveSource, haveDest bool
}

// moveBaseToGERange moves base iterator (from current point) to range which is greater or equal than the given key
func (m *merger) moveBaseToGERange(key graveler.Key) (*Range, error) {
	for {
		_, baseRange := m.base.Value()
		if baseRange != nil && bytes.Compare(baseRange.MaxKey, key) >= 0 {
			return baseRange, nil
		}
		if !m.base.NextRange() {
			break
		}
	}
	return nil, m.base.Err()
}

// moveBaseToGEKey moves base iterator (from current point) to key from is greater or equal than the given key
func (m *merger) moveBaseToGEKey(key graveler.Key) (*graveler.ValueRecord, error) {
	baseValue, _ := m.base.Value()
	if baseValue != nil && bytes.Compare(key, baseValue.Key) <= 0 {
		return baseValue, nil
	}
	baseRange, err := m.moveBaseToGERange(key)
	if err != nil {
		return nil, err
	}
	for {
		baseValue, innerRange := m.base.Value()
		if baseValue != nil && bytes.Compare(key, baseValue.Key) <= 0 {
			return baseValue, nil
		}
		if !m.base.Next() || innerRange.ID != baseRange.ID {
			break
		}
	}
	return nil, m.base.Err()
}

// writeRange writes Range using writer
func (m *merger) writeRange(writeRange *Range) error {
	if m.logger.IsTracing() {
		m.logger.WithFields(logging.Fields{
			"from": string(writeRange.MinKey),
			"to":   string(writeRange.MaxKey),
			"ID":   writeRange.ID,
		}).Trace("copy entire range")
	}
	if err := m.writer.WriteRange(*writeRange); err != nil {
		return fmt.Errorf("copy range %s: %w", writeRange.ID, err)
	}
	return nil
}

// writeRecord writes graveler.ValueRecord using writer
func (m *merger) writeRecord(writeValue *graveler.ValueRecord) error {
	if m.logger.IsTracing() {
		m.logger.WithFields(logging.Fields{
			"key": string(writeValue.Key),
			"ID":  string(writeValue.Identity),
		}).Trace("write record")
	}
	if err := m.writer.WriteRecord(*writeValue); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// handleAll handles the case where only one Iterator from source or dest remains
func (m *merger) handleAll(iter Iterator) error {
	for {
		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		default:
		}
		iterValue, iterRange := iter.Value()
		if iterValue == nil {
			baseRange, err := m.moveBaseToGERange(graveler.Key(iterRange.MinKey))
			if err != nil {
				return fmt.Errorf("base range GE: %w", err)
			}
			if baseRange == nil || baseRange.ID != iterRange.ID {
				if err := m.writeRange(iterRange); err != nil {
					return err
				}
			}
			if !iter.NextRange() {
				break
			}
		} else {
			baseValue, err := m.moveBaseToGEKey(iterValue.Key)
			if err != nil {
				return fmt.Errorf("base value GE: %w", err)
			}
			if baseValue == nil || !bytes.Equal(baseValue.Identity, iterValue.Identity) {
				if err := m.writeRecord(iterValue); err != nil {
					return err
				}
			}
			if !iter.Next() {
				break
			}
		}
	}
	return iter.Err()
}

// handleBothRanges handles the case where both source and dest iterators are at the header of a range
func (m *merger) handleBothRanges(sourceRange *Range, destRange *Range) error {
	switch {
	case sourceRange.ID == destRange.ID: // range hasn't changed or both added the same range
		err := m.writeRange(sourceRange)
		if err != nil {
			return err
		}
		m.haveSource = m.source.NextRange()
		m.haveDest = m.dest.NextRange()

	case bytes.Compare(sourceRange.MaxKey, destRange.MinKey) < 0: // source before dest
		baseRange, err := m.moveBaseToGERange(graveler.Key(sourceRange.MinKey))
		if err != nil {
			return fmt.Errorf("base range GE: %w", err)
		}
		if baseRange != nil && sourceRange.ID == baseRange.ID { // dest deleted this range
			m.haveSource = m.source.NextRange()
			return nil
		}
		if baseRange == nil || destRange.ID == baseRange.ID { // source added this range
			err = m.writeRange(sourceRange)
			if err != nil {
				return err
			}
			m.haveSource = m.source.NextRange()
			return nil
		}
		// both changed this range
		m.haveSource = m.source.Next()
		m.haveDest = m.dest.Next()

	case bytes.Compare(destRange.MaxKey, sourceRange.MinKey) < 0: // dest before source
		baseRange, err := m.moveBaseToGERange(graveler.Key(destRange.MinKey))
		if err != nil {
			return fmt.Errorf("base range GE: %w", err)
		}
		if baseRange != nil && sourceRange.ID == baseRange.ID { // dest added this range
			err = m.writeRange(destRange)
			if err != nil {
				return err
			}
			m.haveDest = m.dest.NextRange()
			return nil
		}
		if baseRange == nil || destRange.ID == baseRange.ID { // source deleted this range
			m.haveDest = m.dest.NextRange()
			return nil
		}
		// both changed this range
		m.haveSource = m.source.Next()
		m.haveDest = m.dest.Next()

	case bytes.Equal(sourceRange.MinKey, destRange.MinKey) && bytes.Equal(sourceRange.MaxKey, destRange.MaxKey): // same bounds
		baseRange, err := m.moveBaseToGERange(graveler.Key(sourceRange.MinKey))
		if err != nil {
			return err
		}
		if baseRange != nil && (sourceRange.ID == baseRange.ID || destRange.ID == baseRange.ID) {
			if sourceRange.ID == baseRange.ID { // dest added changes
				err = m.writeRange(destRange)
			} else {
				err = m.writeRange(sourceRange) // source added changes
			}
			if err != nil {
				return err
			}
			m.haveSource = m.source.NextRange()
			m.haveDest = m.dest.NextRange()
		} else { // enter both ranges
			m.haveSource = m.source.Next()
			m.haveDest = m.dest.Next()
		}

	default: // ranges overlapping
		m.haveSource = m.source.Next()
		m.haveDest = m.dest.Next()
	}
	return nil
}

// handleBothKeys handles the case where both source and dest iterators are inside range
func (m *merger) handleBothKeys(sourceValue *graveler.ValueRecord, destValue *graveler.ValueRecord) error {
	c := bytes.Compare(sourceValue.Key, destValue.Key)
	switch {
	case c < 0: // source before dest
		baseValue, err := m.moveBaseToGEKey(sourceValue.Key)
		if err != nil {
			return err
		}
		if baseValue != nil && bytes.Equal(sourceValue.Identity, baseValue.Identity) { // dest deleted this record
			m.haveSource = m.source.Next()
		} else {
			if baseValue != nil && bytes.Equal(sourceValue.Key, baseValue.Key) { // deleted by dest and changed by source
				return graveler.ErrConflictFound
			}
			// source added this record
			err := m.writeRecord(sourceValue)
			if err != nil {
				return fmt.Errorf("write source record: %w", err)
			}
			m.haveSource = m.source.Next()
		}
	case c > 0: // dest before source
		baseValue, err := m.moveBaseToGEKey(destValue.Key)
		if err != nil {
			return err
		}
		if baseValue != nil && bytes.Equal(destValue.Identity, baseValue.Identity) { // source deleted this record
			m.haveDest = m.dest.Next()
		} else {
			if baseValue != nil && bytes.Equal(destValue.Key, baseValue.Key) { // deleted by source added by dest
				return graveler.ErrConflictFound
			}
			// dest added this record
			err := m.writeRecord(destValue)
			if err != nil {
				return fmt.Errorf("write dest record: %w", err)
			}
			m.haveDest = m.dest.Next()
		}
	default: // identical keys
		baseValue, err := m.moveBaseToGEKey(destValue.Key)
		if err != nil {
			return err
		}
		if !bytes.Equal(sourceValue.Identity, destValue.Identity) {
			if baseValue != nil {
				switch {
				case bytes.Equal(sourceValue.Identity, baseValue.Identity):
					err = m.writeRecord(destValue)
				case bytes.Equal(destValue.Identity, baseValue.Identity):
					err = m.writeRecord(sourceValue)
				default: // both changed the same key
					return graveler.ErrConflictFound
				}
				if err != nil {
					return fmt.Errorf("write record: %w", err)
				}
				m.haveSource = m.source.Next()
				m.haveDest = m.dest.Next()
				return nil
			} else {
				return graveler.ErrConflictFound
			}
		}
		// record hasn't changed or both added the same record
		err = m.writeRecord(sourceValue)
		if err != nil {
			return fmt.Errorf("write record: %w", err)
		}
		m.haveSource = m.source.Next()
		m.haveDest = m.dest.Next()
	}
	return nil
}

// handleDestRangeSourceKey handles the case where source Iterator inside range and dest Iterator at the header of a range
func (m *merger) handleDestRangeSourceKey(destRange *Range, sourceValue *graveler.ValueRecord) error {
	if bytes.Compare(destRange.MinKey, sourceValue.Key) > 0 { // source before dest range
		baseValue, err := m.moveBaseToGEKey(sourceValue.Key)
		if err != nil {
			return fmt.Errorf("base key GE: %w", err)
		}
		if baseValue != nil && bytes.Equal(sourceValue.Identity, baseValue.Identity) { // dest deleted this record
			m.haveSource = m.source.Next()
		} else {
			if baseValue != nil && bytes.Equal(sourceValue.Key, baseValue.Key) { // deleted by dest and changed by source
				return graveler.ErrConflictFound
			}
			// source added this record
			err := m.writeRecord(sourceValue)
			if err != nil {
				return fmt.Errorf("write source record: %w", err)
			}
			m.haveSource = m.source.Next()
		}
		return nil
	}

	if bytes.Compare(destRange.MaxKey, sourceValue.Key) < 0 { // dest range before source
		baseRange, err := m.moveBaseToGERange(graveler.Key(destRange.MinKey))
		if err != nil {
			return fmt.Errorf("base range GE: %w", err)
		}
		if baseRange != nil && destRange.ID == baseRange.ID { // source deleted this range
			m.haveDest = m.dest.NextRange()
			return nil
		}
	}
	// dest is at start of range which we need to scan, enter it
	m.haveDest = m.dest.Next()
	return nil
}

// handleSourceRangeDestKey handles the case where dest Iterator inside range and source Iterator at the header of a range
func (m *merger) handleSourceRangeDestKey(sourceRange *Range, destValue *graveler.ValueRecord) error {
	if bytes.Compare(sourceRange.MinKey, destValue.Key) > 0 { // dest before source range
		baseValue, err := m.moveBaseToGEKey(destValue.Key)
		if err != nil {
			return fmt.Errorf("base key GE: %w", err)
		}
		if baseValue != nil && bytes.Equal(destValue.Identity, baseValue.Identity) { // source deleted this record
			m.haveSource = m.source.Next()
		} else {
			if baseValue != nil && bytes.Equal(destValue.Key, baseValue.Key) { // deleted by source and changed by dest
				return graveler.ErrConflictFound
			}
			// dest added this record
			err := m.writeRecord(destValue)
			if err != nil {
				return fmt.Errorf("write dest record: %w", err)
			}
			m.haveDest = m.dest.Next()
		}
		return nil
	}

	if bytes.Compare(sourceRange.MaxKey, destValue.Key) < 0 { // source range before dest
		baseRange, err := m.moveBaseToGERange(graveler.Key(sourceRange.MinKey))
		if err != nil {
			return fmt.Errorf("base range GE: %w", err)
		}
		if baseRange != nil && sourceRange.ID == baseRange.ID { // dest deleted this range
			m.haveSource = m.source.NextRange()
			return nil
		}
	}
	// source is at start of range which we need to scan, enter it
	m.haveSource = m.source.Next()
	return nil
}

func (m *merger) merge() error {
	m.haveSource, m.haveDest, _ = m.source.Next(), m.dest.Next(), m.base.Next()
	for m.haveSource && m.haveDest {
		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		default:
		}
		sourceValue, sourceRange := m.source.Value()
		destValue, destRange := m.dest.Value()
		var err error
		switch {
		case sourceValue == nil && destValue == nil:
			err = m.handleBothRanges(sourceRange, destRange)
		case destValue == nil && sourceValue != nil:
			err = m.handleDestRangeSourceKey(destRange, sourceValue)
		case sourceValue == nil && destValue != nil:
			err = m.handleSourceRangeDestKey(sourceRange, destValue)
		default:
			err = m.handleBothKeys(sourceValue, destValue)
		}
		if err != nil {
			return err
		}
		if err = m.source.Err(); err != nil {
			return err
		}
		if err = m.dest.Err(); err != nil {
			return err
		}
		if err = m.base.Err(); err != nil {
			return err
		}
	}

	if m.haveSource {
		if err := m.handleAll(m.source); err != nil {
			return err
		}
	}
	if m.haveDest {
		if err := m.handleAll(m.dest); err != nil {
			return err
		}
	}
	return nil
}

func Merge(ctx context.Context, writer MetaRangeWriter, base Iterator, source Iterator, destination Iterator) error {
	m := merger{
		ctx:    ctx,
		logger: logging.FromContext(ctx),
		writer: writer,
		base:   base,
		source: source,
		dest:   destination,
	}
	return m.merge()
}