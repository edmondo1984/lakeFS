BEGIN;
CREATE TABLE IF NOT EXISTS kv_staging
(
    staging_token varchar not null,
    key bytea not null,
    identity bytea not null,
    data bytea
) PARTITION BY HASH (staging_token);
CREATE TABLE IF NOT EXISTS kv_staging_p0 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 0);
CREATE TABLE IF NOT EXISTS kv_staging_p1 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 1);
CREATE TABLE IF NOT EXISTS kv_staging_p2 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 2);
CREATE TABLE IF NOT EXISTS kv_staging_p3 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 3);
CREATE TABLE IF NOT EXISTS kv_staging_p4 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 4);
CREATE TABLE IF NOT EXISTS kv_staging_p5 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 5);
CREATE TABLE IF NOT EXISTS kv_staging_p6 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 6);
CREATE TABLE IF NOT EXISTS kv_staging_p7 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 7);
CREATE TABLE IF NOT EXISTS kv_staging_p8 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 8);
CREATE TABLE IF NOT EXISTS kv_staging_p9 PARTITION OF kv_staging FOR VALUES WITH (MODULUS 10, REMAINDER 9);

CREATE UNIQUE index IF NOT EXISTS kv_staging_uidx
    on kv_staging (staging_token asc, key asc);
COMMIT;