package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runupload"
)

const syncTimeout = 10 * time.Minute

func newRunsSyncCommand(stdout, stderr io.Writer) *cobra.Command {
	var configPath string
	var includeExisting bool
	cmd := &cobra.Command{
		Use: "sync", Short: "Retry pending run-bundle uploads", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rolesReady := func() bool { return rolesResolve(noFlagsChanged, roleFlags{}, roleFlags{}) }
			if err := maybeProvision(cmd, configPath, stderr, rolesReady); err != nil {
				return err
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			resolved, err := cfg.ResolveUpload()
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			store, err := runupload.NewStore(resolved)
			if err != nil {
				return err
			}
			dir := record.DefaultDir()
			if dir == "" {
				return fmt.Errorf("cannot resolve record directory; set ROAST_RECORD_DIR")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), syncTimeout)
			defer cancel()
			outcomes, err := runupload.Sync(ctx, store, dir, includeExisting)
			if err != nil {
				return err
			}
			if len(outcomes) == 0 {
				_, _ = fmt.Fprintln(stdout, "roast: no runs pending upload")
				return nil
			}
			for _, outcome := range outcomes {
				if outcome.Detail == "" {
					_, _ = fmt.Fprintf(stdout, "%s  %s\n", outcome.RunID, outcome.Result)
				} else {
					_, _ = fmt.Fprintf(stdout, "%s  %-16s %s\n", outcome.RunID, outcome.Result, outcome.Detail)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file (default: user config directory/roast/config.yaml)")
	cmd.Flags().BoolVar(&includeExisting, "include-existing", false, "also upload runs which predate upload opt-in")
	return cmd
}

func newRunsStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show local run-upload state", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			dir := record.DefaultDir()
			if dir == "" {
				return fmt.Errorf("cannot resolve record directory; set ROAST_RECORD_DIR")
			}
			summaries, err := runupload.ListStatus(dir)
			if err != nil {
				return err
			}
			if len(summaries) == 0 {
				_, _ = fmt.Fprintln(stdout, "roast: no runs found")
				return nil
			}
			for _, summary := range summaries {
				if summary.Detail == "" {
					_, _ = fmt.Fprintf(stdout, "%s  %s\n", summary.RunID, summary.Status)
				} else {
					_, _ = fmt.Fprintf(stdout, "%s  %-24s %s\n", summary.RunID, summary.Status, summary.Detail)
				}
			}
			return nil
		},
	}
}
