package gate

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Outcome describes what a one-time gate enrollment check did.
type Outcome string

const (
	OutcomeQuiet      Outcome = "quiet"
	OutcomeSuppressed Outcome = "suppressed"
	OutcomeSatisfied  Outcome = "satisfied"
	OutcomeInstalled  Outcome = "installed"
	OutcomeRefreshed  Outcome = "refreshed"
	OutcomeDeclined   Outcome = "declined"
	OutcomeHinted     Outcome = "hinted"
)

// BootstrapOptions contains the replaceable inputs for one enrollment check.
type BootstrapOptions struct {
	Repo   string
	Out    io.Writer
	Getenv func(string) string
	Prompt func(question string) (bool, error)
}

// Bootstrap asks once per repository before installing the pre-commit gate.
// A recorded decision is final until an explicit gate enable/disable command
// changes it. Non-interactive invocations never install a hook silently.
func Bootstrap(opts BootstrapOptions) (Outcome, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if truthy(getenv("ROAST_NO_HOOK")) || getenv("GIT_INDEX_FILE") != "" || opts.Repo == "" {
		return OutcomeSuppressed, nil
	}

	status, err := Inspect(opts.Repo)
	if err != nil {
		return OutcomeQuiet, err
	}
	if status.Shared {
		if err := refreshOwnedSidecar(status.SidecarPath); err != nil {
			return OutcomeQuiet, err
		}
	}
	// Refresh only an already recognized Roast dispatcher, never a foreign
	// hook. In particular, an inherited old shared hook must gain the consent
	// check even when this repo declined or has not answered yet.
	if status.Decision != DecisionEnabled && (status.Ours || status.Appended) && status.Stale {
		if _, _, err := Install(opts.Repo); err != nil {
			return OutcomeQuiet, err
		}
		status, err = Inspect(opts.Repo)
		if err != nil {
			return OutcomeQuiet, err
		}
	}
	switch status.Decision {
	case DecisionDeclined:
		return OutcomeQuiet, nil
	case DecisionEnabled:
		if status.Active() && !status.Stale {
			return OutcomeSatisfied, nil
		}
		return opts.install(false)
	}

	if status.Foreign && !status.Appendable {
		return opts.hint(status, fmt.Sprintf(
			"this repo's %s hook is not an appendable shell script, so Roast left it untouched; run `roast gate enable` to install the sidecar and print the line to add",
			HookName))
	}
	if opts.Prompt == nil {
		return opts.hint(status, "no pre-commit gate in this repo; run `roast gate enable` to install it (non-interactive runs never install Git hooks without consent)")
	}

	question := fmt.Sprintf("Install the Roast pre-commit gate in this repository (%s)?", opts.Repo)
	if status.Foreign {
		question = fmt.Sprintf("This repository already has a %s hook. Append the Roast gate to it (%s)?", HookName, status.HookPath)
	}
	ok, err := opts.Prompt(question)
	if err != nil {
		return OutcomeQuiet, err
	}
	if !ok {
		if err := RecordDecision(opts.Repo, DecisionDeclined); err != nil {
			return OutcomeQuiet, err
		}
		opts.printf("gate declined for this repository; `roast gate enable` turns it on later")
		return OutcomeDeclined, nil
	}
	return opts.install(true)
}

func (opts BootstrapOptions) install(record bool) (Outcome, error) {
	action, status, err := Install(opts.Repo)
	if err != nil {
		return OutcomeQuiet, err
	}
	if action == ActionManual {
		if record {
			if err := RecordDecision(opts.Repo, DecisionEnabled); err != nil {
				return OutcomeQuiet, err
			}
		}
		return opts.hint(status, fmt.Sprintf(
			"this repo's %s hook cannot be appended to, so Roast left it untouched; add this line to enable the gate:\n    %s",
			HookName, ManualLine()))
	}
	if record {
		if err := RecordDecision(opts.Repo, DecisionEnabled); err != nil {
			return OutcomeQuiet, err
		}
	}
	switch action {
	case ActionInstalled:
		opts.printf("pre-commit gate installed: %s (bypass one commit with ROAST_SKIP=1)", status.HookPath)
		return OutcomeInstalled, nil
	case ActionAppended:
		opts.printf("pre-commit gate appended to the existing hook: %s", status.HookPath)
		return OutcomeInstalled, nil
	case ActionRefreshed:
		return OutcomeRefreshed, nil
	default:
		return OutcomeSatisfied, nil
	}
}

func (opts BootstrapOptions) hint(status Status, message string) (Outcome, error) {
	if status.NoticeSent {
		return OutcomeQuiet, nil
	}
	opts.printf("%s", message)
	if err := RecordNotice(opts.Repo); err != nil {
		return OutcomeHinted, err
	}
	return OutcomeHinted, nil
}

func (opts BootstrapOptions) printf(format string, args ...any) {
	fmt.Fprintf(opts.Out, "roast: "+format+"\n", args...)
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
