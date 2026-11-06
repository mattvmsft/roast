package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstallAndInspect(t *testing.T) {
	target := filepath.Join(t.TempDir(), "roast")
	status, err := Inspect(target)
	if err != nil || status.State != NotInstalled {
		t.Fatalf("Inspect() = %#v, %v", status, err)
	}
	status, err = Install(target, false)
	if err != nil || status.State != UpToDate {
		t.Fatalf("Install() = %#v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(target)
	if err != nil || status.State != Stale || !reflect.DeepEqual(status.Stale, []string{"SKILL.md"}) {
		t.Fatalf("stale Inspect() = %#v, %v", status, err)
	}
	if _, err := Install(target, false); err == nil {
		t.Fatal("Install() unexpectedly overwrote a stale skill without --force")
	}
	status, err = Install(target, true)
	if err != nil || status.State != UpToDate {
		t.Fatalf("forced Install() = %#v, %v", status, err)
	}
}

func TestInstallRefusesEmbeddedPathSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "roast")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not replace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(target, true); err == nil {
		t.Fatal("Install() unexpectedly followed a symlink")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "do not replace" {
		t.Fatalf("outside file = %q, %v", got, err)
	}
}

func TestInspectTreatsTargetSymlinkAsDevelopmentInstall(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "roast")
	if err := os.Symlink(source, target); err != nil {
		t.Fatal(err)
	}
	status, err := Inspect(target)
	if err != nil || status.State != Symlink {
		t.Fatalf("Inspect() = %#v, %v", status, err)
	}
}

func TestInstallationTargetsIncludeDetectedHarnesses(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "alice")
	polytokenRoot := filepath.Join(home, ".config", "polytoken")
	targets := installationTargets(home, filepath.Join(home, ".claude"), true, polytokenRoot, true)
	want := []InstallationTarget{
		{Harness: "agents", Parent: filepath.Join(home, ".agents", "skills"), Path: filepath.Join(home, ".agents", "skills", "roast")},
		{Harness: "claude", Parent: filepath.Join(home, ".claude", "skills"), Path: filepath.Join(home, ".claude", "skills", "roast")},
		{Harness: "polytoken", Parent: filepath.Join(polytokenRoot, "skills"), Path: filepath.Join(polytokenRoot, "skills", "roast")},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("installationTargets() = %#v, want %#v", targets, want)
	}
}

func TestInstallationTargetsSkipUndetectedHarnesses(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "alice")
	targets := installationTargets(home, filepath.Join(home, ".claude"), false, filepath.Join(home, ".config", "polytoken"), false)
	if len(targets) != 1 || targets[0].Harness != "agents" {
		t.Fatalf("installationTargets() = %#v", targets)
	}
}

func TestConfiguredRootExpandsHome(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "alice")
	if got := configuredRoot(home, "~/claude-config", ".claude"); got != filepath.Join(home, "claude-config") {
		t.Fatalf("configuredRoot() = %q", got)
	}
}
