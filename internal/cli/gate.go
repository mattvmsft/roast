package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/gate"
	"github.com/bluesky-social/roast/internal/scope"
)

func newGateCommand(stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "gate", Short: "Manage the native Git pre-commit review gate",
		Long: "The gate reviews exactly the staged commit. Findings at HIGH or above block; operational failures fail open with a warning.",
		Args: noArgs, SilenceUsage: true, SilenceErrors: true, RunE: showHelp,
	}
	cmd.AddCommand(newGateStatusCommand(stdout), newGateEnableCommand(stdout), newGateDisableCommand(stdout))
	return cmd
}

func newGateStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Report gate status for this repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			status, err := gate.Inspect(repo)
			if err != nil {
				return err
			}
			decision := string(status.Decision)
			if decision == "" {
				decision = "none recorded"
			}
			fmt.Fprintf(stdout, "repo:     %s\ngate:     %s\ndecision: %s\n", status.Repo, status.Summary(), decision)
			return nil
		},
	}
}

func newGateEnableCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "enable", Short: "Install the pre-commit gate in this repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			action, status, err := gate.Install(repo)
			if err != nil {
				return err
			}
			if err := gate.RecordDecision(repo, gate.DecisionEnabled); err != nil {
				return err
			}
			if action == gate.ActionManual {
				fmt.Fprintf(stdout, "%s is not an appendable shell script; Roast left it untouched.\nAdd this line manually:\n\n    %s\n", status.HookPath, gate.ManualLine())
				return nil
			}
			switch action {
			case gate.ActionInstalled:
				fmt.Fprintf(stdout, "gate installed: %s\n", status.HookPath)
			case gate.ActionAppended:
				fmt.Fprintf(stdout, "gate appended to existing hook: %s\n", status.HookPath)
			case gate.ActionRefreshed:
				fmt.Fprintf(stdout, "gate refreshed: %s\n", status.HookPath)
			case gate.ActionUpToDate:
				fmt.Fprintf(stdout, "gate already up to date: %s\n", status.HookPath)
			}
			fmt.Fprintln(stdout, "bypass one commit with: ROAST_SKIP=1 git commit ...")
			return nil
		},
	}
}

func newGateDisableCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "disable", Short: "Disable the Roast pre-commit gate in this repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			action, status, err := gate.Remove(repo)
			if err != nil {
				return err
			}
			if err := gate.RecordDecision(repo, gate.DecisionDeclined); err != nil {
				return err
			}
			if action == gate.ActionRemoved {
				fmt.Fprintf(stdout, "gate removed from %s\n", status.HookPath)
			} else if action == gate.ActionPreserved {
				fmt.Fprintf(stdout, "gate disabled for this repository; shared dispatcher preserved at %s\n", status.HookPath)
			} else {
				fmt.Fprintln(stdout, "no Roast gate was installed; recorded the decline")
			}
			return nil
		},
	}
}
