package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"

	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"github.com/treeverse/lakefs/cmd/lakefs/application"
	"github.com/treeverse/lakefs/pkg/block/factory"
	"github.com/treeverse/lakefs/pkg/cmdutils"
	"github.com/treeverse/lakefs/pkg/db"
	"github.com/treeverse/lakefs/pkg/graveler"
	"github.com/treeverse/lakefs/pkg/logging"
	"github.com/treeverse/lakefs/pkg/onboard"
	"github.com/treeverse/lakefs/pkg/stats"
)

const (
	DryRunFlagName       = "dry-run"
	WithMergeFlagName    = "with-merge"
	HideProgressFlagName = "hide-progress"
	ManifestURLFlagName  = "manifest"
	PrefixesFileFlagName = "prefix-file"
	BaseCommitFlagName   = "commit"
	ManifestURLFormat    = "s3://example-bucket/inventory/YYYY-MM-DDT00-00Z/manifest.json"
	ImportCmdNumArgs     = 1
	CommitterName        = "lakefs"
)

var importCmd = &cobra.Command{
	Use:   "import <repository uri> --manifest <s3 uri to manifest.json>",
	Short: "Import data from S3 to a lakeFS repository",
	Long:  fmt.Sprintf("Import from an S3 inventory to lakeFS without copying the data. It will be added as a new commit in branch %s", onboard.DefaultImportBranchName),
	Args:  cobra.ExactArgs(ImportCmdNumArgs),
	Run: func(cmd *cobra.Command, args []string) {
		rc := runImport(cmd, args)
		os.Exit(rc)
	},
}

var importBaseCmd = &cobra.Command{
	Use:    "import-base <repository uri> --manifest <s3 uri to manifest.json> --commit <base commit>",
	Short:  "Import data from S3 to a lakeFS repository on top of existing commit",
	Long:   "Creates a new commit with the imported data, on top of the given commit. Does not affect any branch",
	Hidden: true,
	Args:   cobra.ExactArgs(ImportCmdNumArgs),
	Run: func(cmd *cobra.Command, args []string) {
		rc := runImport(cmd, args)
		os.Exit(rc)
	},
}

func getPrefixes(prefixFile string) ([]string, error) {
	var prefixes []string
	if prefixFile != "" {
		file, err := os.Open(prefixFile)
		if err != nil {
			fmt.Printf("Failed to read prefix filter: %s\n", err)
			return nil, err
		}
		defer func() {
			_ = file.Close()
		}()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			prefix := scanner.Text()
			if prefix != "" {
				prefixes = append(prefixes, prefix)
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("Failed to read prefix filter: %s\n", err)
			return nil, err
		}
		fmt.Printf("Filtering according to %d prefixes\n", len(prefixes))
	}
	return prefixes, nil
}

func runImport(cmd *cobra.Command, args []string) (statusCode int) {
	flags := cmd.Flags()
	dryRun, _ := flags.GetBool(DryRunFlagName)
	manifestURL, _ := flags.GetString(ManifestURLFlagName)
	withMerge, _ := flags.GetBool(WithMergeFlagName)
	hideProgress, _ := flags.GetBool(HideProgressFlagName)
	prefixFile, _ := flags.GetString(PrefixesFileFlagName)
	baseCommit, _ := flags.GetString(BaseCommitFlagName)
	cfg := loadConfig()
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	lakeFSCmdCtx := application.NewLakeFSCmdContext(cfg, logger)
	databaseService := application.NewDatabaseService(ctx, lakeFSCmdCtx)
	err := databaseService.ValidateSchemaIsUpToDate(ctx, lakeFSCmdCtx)
	defer databaseService.Close()
	if err != nil {
		if errors.Is(err, db.ErrSchemaNotCompatible) {
			fmt.Println("Migration version mismatch, for more information see https://docs.lakefs.io/deploying-aws/upgrade.html")
		} else {
			fmt.Printf("%s\n", err)
		}
		return 1
	}

	c, err := databaseService.NewCatalog(ctx, lakeFSCmdCtx)

	if err != nil {
		fmt.Printf("Failed to create c: %s\n", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	bufferedCollector := stats.NewBufferedCollector(cfg.GetFixedInstallationID(), cfg)
	// TODO: are there good reasons why the statsCollector is not set here?
	blockStore, err := factory.BuildBlockAdapter(ctx, nil, cfg)
	if err != nil {
		fmt.Printf("Failed to create block adapter: %s\n", err)
		return 1
	}
	if blockStore.BlockstoreType() != "s3" {
		fmt.Printf("Configuration uses unsupported block adapter: %s. Only s3 is supported.\n", blockStore.BlockstoreType())
		return 1
	}
	defer bufferedCollector.Close()
	bufferedCollector.SetRuntimeCollector(blockStore.RuntimeStats)

	// wire actions into entry catalog
	actionsService := application.NewActionsService(ctx, lakeFSCmdCtx, databaseService, c, bufferedCollector)

	defer actionsService.Stop()

	if dryRun {
		fmt.Print("Starting import dry run. Will not perform any changes.\n\n")
	}
	var prefixes []string
	prefixes, err = getPrefixes(prefixFile)
	if err != nil {
		return 1
	}
	repo, err := application.NewRepository(ctx, c, args[0], manifestURL)
	if err != nil {
		logger.WithError(err).Fatal("Error getting repository")
	}

	importConfig := &onboard.Config{
		CommitUsername:     CommitterName,
		InventoryURL:       manifestURL,
		RepositoryID:       graveler.RepositoryID(repo.Name),
		DefaultBranchID:    graveler.BranchID(repo.DefaultBranch),
		InventoryGenerator: blockStore,
		Store:              c.Store,
		KeyPrefixes:        prefixes,
		BaseCommit:         graveler.CommitID(baseCommit),
	}

	importer, err := onboard.CreateImporter(ctx, logger, importConfig)
	if err != nil {
		logger.WithError(err).Fatal("Import failed")
	}
	var multiBar *cmdutils.MultiBar
	if !hideProgress {
		multiBar = cmdutils.NewMultiBar(importer)
		multiBar.Start()
	}
	stats, err := importer.Import(ctx, dryRun)
	if err != nil {
		if multiBar != nil {
			multiBar.Stop()
		}
		logger.WithError(err).Fatal("Import failed")
	}
	if multiBar != nil {
		multiBar.Stop()
	}
	fmt.Println()
	fmt.Println(text.FgYellow.Sprint("Added or changed objects:"), stats.AddedOrChanged)

	if dryRun {
		fmt.Println("Dry run successful. No changes were made.")
		return 0
	}

	fmt.Print(text.FgYellow.Sprint("Commit ref:"), stats.CommitRef)
	fmt.Println()

	if baseCommit == "" {
		fmt.Printf("Import to branch %s finished successfully.\n", onboard.DefaultImportBranchName)
		fmt.Println()
	}

	if withMerge {
		fmt.Printf("Merging import changes into lakefs://%s/%s/\n", repo.Name, repo.DefaultBranch)
		msg := fmt.Sprintf(onboard.CommitMsgTemplate, stats.CommitRef)
		commitLog, err := c.Merge(ctx, repo.Name, onboard.DefaultImportBranchName, repo.DefaultBranch, CommitterName, msg, nil, "")
		if err != nil {
			logger.WithError(err).Fatal("Merge failed")
		}
		fmt.Println("Merge was completed successfully.")
		fmt.Printf("To list imported objects, run:\n\t$ lakectl fs ls lakefs://%s/%s/\n", repo.Name, commitLog)
	} else {
		fmt.Printf("To list imported objects, run:\n\t$ lakectl fs ls lakefs://%s/%s/\n", repo.Name, stats.CommitRef)
		fmt.Printf("To merge the changes to your main branch, run:\n\t$ lakectl merge lakefs://%s/%s lakefs://%s/%s\n", repo.Name, stats.CommitRef, repo.Name, repo.DefaultBranch)
	}

	return 0
}

//nolint:gochecknoinits
func init() {
	manifestFlagMsg := fmt.Sprintf("S3 uri to the manifest.json to use for the import. Format: %s", ManifestURLFormat)
	const (
		hideMsg     = "Suppress progress bar"
		prefixesMsg = "File with a list of key prefixes. Imported object keys will be filtered according to these prefixes"
	)

	rootCmd.AddCommand(importCmd)
	importCmd.Flags().Bool(DryRunFlagName, false, "Only read inventory, print stats and write metarange. Commits nothing")
	importCmd.Flags().StringP(ManifestURLFlagName, "m", "", manifestFlagMsg)
	_ = importCmd.MarkFlagRequired(ManifestURLFlagName)
	importCmd.Flags().Bool(WithMergeFlagName, false, "Merge imported data to the repository's main branch")
	importCmd.Flags().Bool(HideProgressFlagName, false, hideMsg)
	importCmd.Flags().StringP(PrefixesFileFlagName, "p", "", prefixesMsg)

	rootCmd.AddCommand(importBaseCmd)
	importBaseCmd.Flags().StringP(ManifestURLFlagName, "m", "", manifestFlagMsg)
	_ = importBaseCmd.MarkFlagRequired(ManifestURLFlagName)
	importBaseCmd.Flags().Bool(HideProgressFlagName, false, hideMsg)
	importBaseCmd.Flags().StringP(PrefixesFileFlagName, "p", "", prefixesMsg)
	importBaseCmd.Flags().StringP(BaseCommitFlagName, "b", "", "Commit to apply to apply the import on top of")
	_ = importCmd.MarkFlagRequired(BaseCommitFlagName)
}
