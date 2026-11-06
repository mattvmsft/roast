package gate

import (
	"bytes"
	"os"
	"testing"
)

func bootstrapForTest(t *testing.T, repo string, prompt func(string) (bool, error)) (Outcome, string) {
	t.Helper()
	var out bytes.Buffer
	outcome, err := Bootstrap(BootstrapOptions{
		Repo: repo, Out: &out, Prompt: prompt,
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return outcome, out.String()
}

func TestBootstrapPromptDeclineIsRemembered(t *testing.T) {
	repo := newRepo(t)
	outcome, _ := bootstrapForTest(t, repo, func(string) (bool, error) { return false, nil })
	if outcome != OutcomeDeclined {
		t.Fatalf("outcome = %q", outcome)
	}
	status, err := Inspect(repo)
	if err != nil || status.Decision != DecisionDeclined {
		t.Fatalf("status = %#v, %v", status, err)
	}
	outcome, output := bootstrapForTest(t, repo, func(string) (bool, error) {
		t.Fatal("declined repository prompted again")
		return false, nil
	})
	if outcome != OutcomeQuiet || output != "" {
		t.Fatalf("second bootstrap = %q, %q", outcome, output)
	}
}

func TestBootstrapPromptAcceptsAndInstalls(t *testing.T) {
	repo := newRepo(t)
	outcome, _ := bootstrapForTest(t, repo, func(string) (bool, error) { return true, nil })
	if outcome != OutcomeInstalled {
		t.Fatalf("outcome = %q", outcome)
	}
	status, err := Inspect(repo)
	if err != nil || status.Decision != DecisionEnabled || !status.Active() {
		t.Fatalf("status = %#v, %v", status, err)
	}
}

func TestBootstrapNonInteractiveHintsOnce(t *testing.T) {
	repo := newRepo(t)
	outcome, output := bootstrapForTest(t, repo, nil)
	if outcome != OutcomeHinted || output == "" {
		t.Fatalf("first bootstrap = %q, %q", outcome, output)
	}
	outcome, output = bootstrapForTest(t, repo, nil)
	if outcome != OutcomeQuiet || output != "" {
		t.Fatalf("second bootstrap = %q, %q", outcome, output)
	}
	status, _ := Inspect(repo)
	if status.Decision != DecisionNone {
		t.Fatalf("hint recorded a decision: %q", status.Decision)
	}
}

func TestBootstrapSuppressesInsideHookAndByEnvironment(t *testing.T) {
	repo := newRepo(t)
	for _, env := range []map[string]string{{"GIT_INDEX_FILE": "index"}, {"ROAST_NO_HOOK": "1"}} {
		outcome, err := Bootstrap(BootstrapOptions{
			Repo: repo, Out: os.Stderr,
			Getenv: func(name string) string { return env[name] },
			Prompt: func(string) (bool, error) {
				t.Fatal("suppressed bootstrap prompted")
				return false, nil
			},
		})
		if err != nil || outcome != OutcomeSuppressed {
			t.Fatalf("Bootstrap() = %q, %v", outcome, err)
		}
	}
}
