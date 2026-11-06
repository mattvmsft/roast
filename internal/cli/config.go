package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/textsafe"
)

func newConfigCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Initialize Roast configuration", Args: noArgs, RunE: showHelp}
	cmd.AddCommand(newConfigInitCommand(stdout, stderr))
	return cmd
}

func newConfigInitCommand(stdout, stderr io.Writer) *cobra.Command {
	var sourceURL, destination string
	cmd := &cobra.Command{
		Use: "init", Short: "Install configuration from a trusted URL", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			input := bufio.NewReader(cmd.InOrStdin())
			if strings.TrimSpace(sourceURL) == "" {
				_, _ = fmt.Fprint(stderr, "Configuration URL: ")
				line, _ := input.ReadString('\n')
				sourceURL = strings.TrimSpace(line)
				if sourceURL == "" {
					return &usageError{msg: "configuration URL is required"}
				}
			}
			legacy, _ := config.LegacyUserPath()
			if destination == "" {
				destination, _ = config.UserPath()
			}
			confirm := uploadConfirmer(cmd.InOrStdin(), input, stderr)
			if err := installTemplate(cmd.Context(), sourceURL, destination, legacy, stderr, confirm); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "roast: installed configuration at %s\n", destination)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceURL, "from", "", "HTTPS URL containing a complete Roast configuration")
	cmd.Flags().StringVar(&destination, "output", "", "config path (default: user config directory/roast/config.yaml)")
	return cmd
}

// maybeProvision offers the first-run configuration prompt. It never prompts
// without a terminal on both stdin and stderr, and it does not prompt an
// unconfigured user whose roles already resolve from the environment and
// flags.
func maybeProvision(cmd *cobra.Command, explicitPath string, stderr io.Writer, rolesResolve func() bool) error {
	if explicitPath != "" {
		return nil
	}
	path, err := config.UserPath()
	if err != nil {
		return err
	}
	legacy := ""
	if _, err := os.Stat(path); err == nil {
		needs, checkErr := config.NeedsProvision(path)
		if checkErr != nil {
			return checkErr
		}
		if !needs {
			return nil
		}
		legacy = path
	} else if !os.IsNotExist(err) {
		return err
	}
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !interactive(input, stderr) {
		return nil
	}
	if legacy == "" && rolesResolve() {
		return nil
	}
	return offerProvision(cmd, path, legacy, input, stderr)
}

func offerProvision(cmd *cobra.Command, path, legacy string, input io.Reader, stderr io.Writer) error {
	if legacy == "" {
		legacy, _ = config.LegacyUserPath()
	}
	if _, err := os.Stat(legacy); err == nil {
		_, _ = fmt.Fprintf(stderr, "Roast found a configuration to initialize or update at %s.\n", legacy)
		_, _ = fmt.Fprintln(stderr, "A trusted configuration URL can update matching Claude profiles to native Messages without replacing your other settings. Leave blank to keep intentional compatibility protocols.")
	} else {
		legacy = ""
		_, _ = fmt.Fprintln(stderr, "Roast is not configured.")
	}
	_, _ = fmt.Fprint(stderr, "Configuration URL (leave blank to configure manually): ")
	reader := bufio.NewReader(input)
	line, _ := reader.ReadString('\n')
	sourceURL := strings.TrimSpace(line)
	if sourceURL == "" {
		return config.AcknowledgeProtocolUpdate(path)
	}
	confirm := func(question string) (bool, error) { return promptNoDefault(stderr, reader, question) }
	if err := installTemplate(cmd.Context(), sourceURL, path, legacy, stderr, confirm); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "roast: installed configuration at %s\n", path)
	return nil
}

// installTemplate downloads and validates a template, shows what it installs,
// asks confirm before enabling uploads when confirm is non-nil, then installs.
func installTemplate(ctx context.Context, sourceURL, path, legacy string, stderr io.Writer, confirm func(string) (bool, error)) error {
	plan, err := config.PlanProvision(ctx, sourceURL, path, legacy)
	if err != nil {
		return provisionError(err)
	}
	writeProvisionSummary(stderr, plan)
	if plan.TemplateUpload() && confirm != nil {
		ok, err := confirm(uploadQuestion(plan.Config.Upload))
		if err != nil {
			return err
		}
		if !ok {
			return &usageError{msg: "configuration not installed: run uploads were not confirmed"}
		}
	}
	if err := plan.Install(); err != nil {
		return &usageError{msg: err.Error()}
	}
	return nil
}

// provisionError maps a configuration fetch failure to the endpoint exit code
// and every other provisioning failure to the usage exit code.
func provisionError(err error) error {
	var fetch *config.FetchError
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &fetch):
		return &endpointError{err: err}
	default:
		return &usageError{msg: err.Error()}
	}
}

// endpointError classifies a non-inference network failure with the endpoint
// category without changing its message.
type endpointError struct{ err error }

func (e *endpointError) Error() string   { return e.err.Error() }
func (e *endpointError) Unwrap() []error { return []error{e.err, llm.ErrEndpoint} }

// uploadConfirmer returns a y/N prompt when the install is interactive, and
// nil when it is not: a non-interactive install proceeds on the explicit
// command with only the summary.
func uploadConfirmer(stdin io.Reader, input *bufio.Reader, stderr io.Writer) func(string) (bool, error) {
	if !interactive(stdin, stderr) {
		return nil
	}
	return func(question string) (bool, error) { return promptNoDefault(stderr, input, question) }
}

// interactive reports whether a prompt can be shown on stderr and answered on
// stdin. Tests replace it; character devices such as /dev/null do not count.
var interactive = func(stdin, stderr any) bool {
	return isTerminal(stdin) && isTerminal(stderr)
}

func isTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// writeProvisionSummary reports, per role, where reviewed code is sent and
// which credential is used, plus the upload destination, so a template's
// effect is visible before and after it is installed. Every configuration
// value passes through textsafe.Line, so none can rewrite the summary.
func writeProvisionSummary(w io.Writer, plan *config.ProvisionPlan) {
	cfg := plan.Config
	if len(plan.ProtocolUpdates) > 0 {
		_, _ = fmt.Fprintf(w, "roast: the template switches %s to the native anthropic protocol; no other setting changes.\n", textsafe.Line(strings.Join(plan.ProtocolUpdates, ", ")))
	}
	_, _ = fmt.Fprintf(w, "roast: configuration for %s:\n", plan.Path)
	for _, name := range []string{"finder", "verifier"} {
		role := cfg.Roles[name]
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", name+":", describeProfile(cfg, name, role.Profile, plan.ImportedLegacyCredential))
		for _, route := range slices.Sorted(maps.Keys(role.AuthorRoutes)) {
			_, _ = fmt.Fprintf(w, "  %-9s author %s -> %s\n", "", textsafe.Line(route), describeProfile(cfg, name, role.AuthorRoutes[route], plan.ImportedLegacyCredential))
		}
	}
	_, _ = fmt.Fprintf(w, "  %-9s %s\n", "upload:", describeUpload(cfg.Upload, plan.ImportedLegacyCredential))
}

func describeProfile(cfg config.Config, role, name string, imported bool) string {
	profile := cfg.InferenceProfiles[name]
	credential := profile.DescribeCredential(role)
	if profile.APIKey != "" && imported {
		credential = "api_key imported from the legacy config"
	}
	return textsafe.Line(fmt.Sprintf("profile %s, %s, protocol %s, credential %s", name, profile.DescribeEndpoint(), profile.Protocol, credential))
}

// uploadQuestion asks before installing a template that enables uploads.
func uploadQuestion(upload config.Upload) string {
	return fmt.Sprintf("This configuration uploads every completed review's run bundle, including reviewed code, prompts, and model responses, to %s. Install it?", uploadDestination(upload))
}

func describeUpload(upload config.Upload, imported bool) string {
	if upload.Endpoint == "" {
		return "off"
	}
	state := "off (roast runs sync sends to " + uploadDestination(upload) + ")"
	if upload.Enabled {
		state = "ON, after every review, to " + uploadDestination(upload)
	}
	switch {
	case upload.Backend == "presigned" && upload.TokenEnv != "":
		state += ", token env " + upload.TokenEnv
	case upload.Backend == "presigned" && upload.Token != "" && imported:
		state += ", token imported from the legacy config"
	case upload.Backend == "s3":
		names := []string{upload.AccessKeyEnv, upload.SecretKeyEnv}
		if upload.SessionTokenEnv != "" {
			names = append(names, upload.SessionTokenEnv)
		}
		state += ", credentials env " + strings.Join(names, ", ")
	}
	return textsafe.Line(state)
}

func uploadDestination(upload config.Upload) string {
	if upload.Backend == "presigned" {
		return textsafe.Line("presigned broker " + upload.Endpoint)
	}
	return textsafe.Line(fmt.Sprintf("s3 %s bucket %s prefix %s", upload.Endpoint, upload.Bucket, upload.Prefix))
}

// rolesResolve reports whether both roles are usable from the default config
// location, the environment, and the given role flags.
func rolesResolve(changed func(string) bool, finder, verifier roleFlags) bool {
	cfg, err := config.Load("")
	if err != nil {
		return false
	}
	for name, flags := range map[string]roleFlags{"finder": finder, "verifier": verifier} {
		role, err := cfg.ResolveRole(name)
		if err != nil {
			return false
		}
		if config.ValidateRole(name, applyRoleFlags(changed, name, role, flags)) != nil {
			return false
		}
	}
	return true
}

func noFlagsChanged(string) bool { return false }
