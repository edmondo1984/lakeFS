package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/treeverse/lakefs/cmd/lakefs/application"
	"github.com/treeverse/lakefs/pkg/logging"
)

// setupCmd initial lakeFS system setup - build database, load initial data and create first superuser
var setupCmd = &cobra.Command{
	Use:     "setup",
	Aliases: []string{"init"},
	Short:   "Setup a new LakeFS instance with initial credentials",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := loadConfig()
		ctx := cmd.Context()
		logger := logging.Default()
		lakeFSCmdCtx := application.NewLakeFSCmdContext(cfg, logger)
		databaseService := application.NewDatabaseService(ctx, lakeFSCmdCtx)
		defer databaseService.Close()
		err := databaseService.Migrate(ctx)
		if err != nil {
			logger.WithError(err).Fatal("Failed to setup DB")
		}
		createUser(cmd, InitialSetup, databaseService, cfg, logger, ctx)
	},
}

const internalErrorCode = 2

//nolint:gochecknoinits
func init() {
	rootCmd.AddCommand(setupCmd)
	f := setupCmd.Flags()
	f.String("user-name", "", "an identifier for the user (e.g. \"jane.doe\")")
	f.String("access-key-id", "", "AWS-format access key ID to create for that user (for integration)")
	f.String("secret-access-key", "", "AWS-format secret access key to create for that user (for integration)")
	if err := f.MarkHidden("access-key-id"); err != nil {
		// (internal error)
		fmt.Fprint(os.Stderr, err)
		os.Exit(internalErrorCode)
	}
	if err := f.MarkHidden("secret-access-key"); err != nil {
		// (internal error)
		fmt.Fprint(os.Stderr, err)
		os.Exit(internalErrorCode)
	}
	_ = setupCmd.MarkFlagRequired("user-name")
}
