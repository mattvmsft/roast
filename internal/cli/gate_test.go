package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/gate"
)

func TestGateCommandsKeepSharedDispatcher(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, hooks := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.hooksPath", hooks}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v, %s", err, out)
		}
	}
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	for _, command := range []string{"enable", "disable"} {
		stdout.Reset()
		code, err := ExecuteContext(t.Context(), []string{"gate", command}, &stdout, &stderr)
		if err != nil || code != ExitClean {
			t.Fatalf("%s code=%d err=%v stdout=%s stderr=%s", command, code, err, stdout.String(), stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), "shared dispatcher preserved") {
		t.Fatalf("disable output = %q", stdout.String())
	}
	status, err := gate.Inspect(repo)
	if err != nil || status.Decision != gate.DecisionDeclined || status.Active() || !status.Ours {
		t.Fatalf("disabled shared status = %#v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(hooks, gate.HookName)); err != nil {
		t.Fatal("shared dispatcher removed:", err)
	}
}

func TestManualGateEnableRecordsConsent(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git setup: %v, %s", err, out)
	}
	hook := filepath.Join(repo, ".git", "hooks", gate.HookName)
	const foreign = "#!/usr/bin/env python3\nprint('foreign hook')\n"
	if err := os.WriteFile(hook, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"gate", "enable"}, &stdout, &stderr)
	if err != nil || code != ExitClean || !strings.Contains(stdout.String(), "manually") {
		t.Fatalf("enable code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	status, err := gate.Inspect(repo)
	if err != nil || status.Decision != gate.DecisionEnabled || !status.Foreign {
		t.Fatalf("manual gate status = %#v, %v", status, err)
	}
	if body, err := os.ReadFile(hook); err != nil || string(body) != foreign {
		t.Fatalf("foreign hook was modified: %v", err)
	}
}
