package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/skill"
)

func newSkillCommand(stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "skill",
		Short:         "Manage the Roast agent skill",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          showHelp,
	}
	cmd.AddCommand(newSkillInstallCommand(stdout), newSkillStatusCommand(stdout))
	return cmd
}

func newSkillInstallCommand(stdout io.Writer) *cobra.Command {
	var parent string
	var force bool
	cmd := &cobra.Command{
		Use:           "install",
		Short:         "Install the Roast agent skill",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			targets, err := skill.InstallationTargets(parent)
			if err != nil {
				return err
			}
			before := make([]skill.Status, len(targets))
			for i, target := range targets {
				before[i], err = skill.Inspect(target.Path)
				if err != nil {
					return err
				}
				if before[i].State == skill.Stale && !force {
					return fmt.Errorf("%s skill files differ (%s); re-run with --force to overwrite", target.Harness, strings.Join(before[i].Stale, ", "))
				}
			}
			for i, target := range targets {
				after, installErr := skill.Install(target.Path, force)
				if installErr != nil {
					return installErr
				}
				switch before[i].State {
				case skill.Symlink:
					fmt.Fprintf(stdout, "%s\tsymlink (development install), left unchanged\t%s\n", target.Harness, target.Path)
				case skill.UpToDate:
					fmt.Fprintf(stdout, "%s\talready up to date\t%s\n", target.Harness, target.Path)
				default:
					fmt.Fprintf(stdout, "%s\tinstalled (%s)\t%s\n", target.Harness, after.State, target.Path)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&parent, "dir", "", "install only below this parent skills directory; /roast is appended")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite installed skill files that differ")
	return cmd
}

func newSkillStatusCommand(stdout io.Writer) *cobra.Command {
	var parent string
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Report the Roast agent skill installation status",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			targets, err := skill.InstallationTargets(parent)
			if err != nil {
				return err
			}
			for _, target := range targets {
				status, inspectErr := skill.Inspect(target.Path)
				if inspectErr != nil {
					return inspectErr
				}
				fmt.Fprintf(stdout, "%s\t%s\t%s", target.Harness, status.State, status.Path)
				if len(status.Stale) > 0 {
					fmt.Fprintf(stdout, "\t%s", strings.Join(status.Stale, ","))
				}
				fmt.Fprintln(stdout)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&parent, "dir", "", "inspect only below this parent skills directory; /roast is appended")
	return cmd
}
