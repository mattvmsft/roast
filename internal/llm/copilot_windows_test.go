package llm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func copilotTestPATH(t *testing.T) (broken, working string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"WindowsApps", "cli"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	broken = filepath.Join(root, "WindowsApps", "copilot.exe")
	working = filepath.Join(root, "cli", "copilot.exe")
	if err := os.WriteFile(broken, nil, 0600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(working, content, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROAST_TEST_COPILOT_PROCESS", "1")
	t.Setenv("ROAST_COPILOT_COMMAND", "")
	t.Setenv("PATH", filepath.Dir(broken)+";"+filepath.Dir(working))
	t.Setenv("PATHEXT", ".EXE")
	return broken, working
}

func TestCopilotWindowsSkipsUnlaunchablePATHAlias(t *testing.T) {
	_, working := copilotTestPATH(t)
	model, err := newCopilotModel(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(model.executable, working) {
		t.Fatalf("selected %q, want %q", model.executable, working)
	}
	client := &client{cfg: ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 1}, model: model}
	var output struct {
		OK bool `json:"ok"`
	}
	if err := client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output); err != nil || !output.OK {
		t.Fatalf("fallback execution: output=%+v err=%v", output, err)
	}
}

func TestCopilotWindowsExplicitOverrideDoesNotFallBack(t *testing.T) {
	broken, _ := copilotTestPATH(t)
	t.Setenv("ROAST_COPILOT_COMMAND", broken)
	client, err := New(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	var output struct{}
	err = client.CompleteStructured(t.Context(), "", "Return the probe.", "probe", &output)
	if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), broken) {
		t.Fatalf("explicit broken command must not fall back: %v", err)
	}
}

func TestCopilotWindowsIgnoresCheckoutExecutable(t *testing.T) {
	_, working := copilotTestPATH(t)
	t.Chdir(t.TempDir())
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",execerrdot=1")
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
	if err := os.Unsetenv("NoDefaultCurrentDirectoryInExePath"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("copilot.exe", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("copilot"); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("fixture must trigger current-directory lookup rejection: %v", err)
	}
	path, err := resolveCopilotExecutable(t.Context())
	if err != nil || !strings.EqualFold(path, working) {
		t.Fatalf("selected %q, err=%v; want PATH executable %q", path, err, working)
	}
}

func TestCopilotWindowsRejectsRelativePATH(t *testing.T) {
	copilotTestPATH(t)
	t.Chdir(t.TempDir())
	if err := os.Mkdir("cli", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("cli", "copilot.exe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "cli")
	if _, err := resolveCopilotExecutable(t.Context()); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("relative PATH must not select a checkout executable: %v", err)
	}
}

func TestCopilotWindowsUnusablePATH(t *testing.T) {
	broken, _ := copilotTestPATH(t)
	t.Setenv("PATH", filepath.Dir(broken))
	_, err := New(t.Context(), ClientConfig{Protocol: "copilot", MaxOutputTokens: 100})
	if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), broken) ||
		!strings.Contains(err.Error(), "ROAST_COPILOT_COMMAND") {
		t.Fatalf("expected actionable discovery error: %v", err)
	}
}

func TestCopilotWindowsDiscoveryCancellation(t *testing.T) {
	copilotTestPATH(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := resolveCopilotExecutable(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestCopilotWindowsDiscoveryBoundsInheritedPipeWait(t *testing.T) {
	_, working := copilotTestPATH(t)
	t.Setenv("PATH", filepath.Dir(working))
	copilotInheritedPipes(t, "exit")
	result := make(chan error, 1)
	go func() {
		_, err := resolveCopilotExecutable(t.Context())
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("error = %v, want pipe-drain failure", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("discovery did not bound inherited-pipe wait")
	}
}
