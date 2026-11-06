package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/scope"
	"github.com/bluesky-social/roast/internal/snapshot"
	"github.com/bluesky-social/roast/internal/textsafe"
)

func newRunsCommand(stdout, stderr io.Writer) *cobra.Command {
	var limit int
	var rawJSON bool
	cmd := &cobra.Command{
		Use: "runs", Short: "Show and inspect local review runs", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			dir := record.DefaultDir()
			if dir == "" {
				return fmt.Errorf("cannot resolve record directory; set ROAST_RECORD_DIR")
			}
			files, _ := filepath.Glob(filepath.Join(dir, "runs-*.jsonl"))
			sort.Strings(files)
			lines := tailRunLines(files, limit)
			if len(lines) == 0 {
				fmt.Fprintf(stderr, "roast: no run records in %s\n", dir)
				return nil
			}
			for _, line := range lines {
				if rawJSON {
					fmt.Fprintln(stdout, line)
					continue
				}
				var run struct {
					TS         string   `json:"ts"`
					RunID      string   `json:"run_id"`
					Repo       string   `json:"repo"`
					Mode       string   `json:"mode"`
					Assessment string   `json:"assessment"`
					Quick      bool     `json:"quick"`
					NFindings  int      `json:"n_findings"`
					NRefuted   int      `json:"n_refuted"`
					Files      []string `json:"files_reviewed"`
				}
				if err := json.Unmarshal([]byte(line), &run); err != nil {
					continue
				}
				fmt.Fprintf(stdout, "%s  %-25s %-8s q=%-5v %-30s findings=%d refuted=%d files=%d  %s\n",
					run.TS, run.RunID, run.Mode, run.Quick, run.Assessment,
					run.NFindings, run.NRefuted, len(run.Files), run.Repo)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum runs to show")
	cmd.Flags().BoolVar(&rawJSON, "json", false, "emit raw JSONL records")
	cmd.AddCommand(
		newRunsShowCommand(stdout), newRunsCheckoutCommand(stdout, stderr),
		newRunsPruneCommand(stdout, stderr),
		newRunsSyncCommand(stdout, stderr), newRunsStatusCommand(stdout),
	)
	return cmd
}

func tailRunLines(files []string, limit int) []string {
	var lines []string
	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		_ = file.Close()
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

func newRunsShowCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "show <run-id>", Short: "Print a run's Markdown report", Args: usageArgs(cobra.ExactArgs(1)),
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := record.ReportPath(args[0])
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("run has no report artifact: %w", err)
			}
			// Reports written before render escaped finding text can hold raw
			// control bytes from model output; escape on the way out too.
			_, err = io.WriteString(stdout, textsafe.Text(string(data)))
			return err
		},
	}
}

func newRunsCheckoutCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "checkout <run-id> [dir]", Short: "Materialize a run snapshot into a linked worktree",
		Args: usageArgs(cobra.RangeArgs(1, 2)), SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, args []string) error {
			meta, _, err := record.ReadMeta(args[0])
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if !meta.Snapshot.Captured {
				return &usageError{msg: fmt.Sprintf("run %s has no Git snapshot (%s)", meta.RunID, meta.Snapshot.Reason)}
			}
			dest := ""
			if len(args) == 2 {
				dest = args[1]
			} else {
				base, err := os.MkdirTemp("", "roast-checkout-")
				if err != nil {
					return err
				}
				dest = filepath.Join(base, meta.RunID)
			}
			if _, err := snapshot.Restore(meta.Repo, meta.RunID, dest); err != nil {
				return fmt.Errorf("restore run %s: %w", meta.RunID, err)
			}
			fmt.Fprintf(stderr, "roast: remove with: git -C %s worktree remove --force %s\n", meta.Repo, dest)
			fmt.Fprintln(stdout, dest)
			return nil
		},
	}
}

func newRunsPruneCommand(stdout, stderr io.Writer) *cobra.Command {
	var olderThan time.Duration
	var dryRun bool
	cmd := &cobra.Command{
		Use: "prune", Short: "Delete aged Roast snapshot refs from the current repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			items, err := snapshot.Prune(repo, olderThan, dryRun)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				fmt.Fprintf(stderr, "roast: no snapshot refs older than %s\n", olderThan)
			}
			for _, item := range items {
				verb := "deleted"
				if dryRun {
					verb = "would delete"
				}
				fmt.Fprintf(stdout, "%s %s (age %s)\n", verb, item.Ref, item.Age.Round(time.Hour))
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", snapshot.DefaultRefRetention, "delete refs older than this")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list refs without deleting them")
	return cmd
}
