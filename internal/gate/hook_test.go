package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates an isolated temp git repo. The developer's own ~/.gitconfig
// is deliberately cut out (GIT_CONFIG_GLOBAL/SYSTEM): a machine that sets
// core.hooksPath globally would otherwise redirect every hook path these
// tests assert on.
func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dir := t.TempDir()
	// macOS puts TempDir under /var, a symlink to /private/var; git reports
	// the resolved path, so resolve here or every path comparison drifts.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	git(t, dir, "init", "-q")
	// These disposable repos do not need background object maintenance. Git
	// commits may detach it, racing TempDir's removal of .git/objects.
	git(t, dir, "config", "maintenance.auto", "false")
	git(t, dir, "config", "gc.auto", "0")
	git(t, dir, "config", "user.email", "roast-test@example.com")
	git(t, dir, "config", "user.name", "roast test")
	git(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepoDisablesAutomaticMaintenance(t *testing.T) {
	repo := newRepo(t)
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		if got := git(t, repo, "config", "--local", "--get", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func writeHook(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHookScriptCarriesMarker guards the recognition contract: Inspect finds
// our own hook by looking for HookMarker in the file, so the embedded script
// must actually contain it.
func TestHookScriptCarriesMarker(t *testing.T) {
	if !strings.Contains(HookScript, HookMarker) {
		t.Fatalf("embedded hook script does not contain %q", HookMarker)
	}
	if !strings.HasPrefix(HookScript, "#!/bin/sh\n") {
		t.Error("hook script must be POSIX sh")
	}
}

func TestInspectNotEnrolled(t *testing.T) {
	repo := newRepo(t)
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Exists || st.Active() || st.Decision != DecisionNone {
		t.Fatalf("fresh repo should be unenrolled: %+v", st)
	}
	if want := filepath.Join(repo, ".git", "hooks", "pre-commit"); st.HookPath != want {
		t.Errorf("HookPath = %q, want %q", st.HookPath, want)
	}
	if !strings.Contains(st.Summary(), "not enrolled") {
		t.Errorf("summary = %q", st.Summary())
	}
}

func TestInstallCreatesExecutableHook(t *testing.T) {
	repo := newRepo(t)
	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Fatalf("action = %q, want %q", action, ActionInstalled)
	}
	if got := read(t, st.HookPath); got != HookScript {
		t.Error("installed hook does not match the embedded script")
	}
	info, err := os.Stat(st.HookPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook is not executable: %v", info.Mode())
	}

	// Idempotent.
	again, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if again != ActionUpToDate {
		t.Errorf("second install = %q, want %q", again, ActionUpToDate)
	}
}

// TestInstallRefreshesStaleOwnHook covers the "present-and-ours-but-stale →
// refresh silently" case: an older roast wrote the file, so rewriting it is
// not a clobber.
func TestInstallRefreshesStaleOwnHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\n"+HookMarker+"\n# an older version\nexit 0\n", 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ours || !st.Stale {
		t.Fatalf("expected ours+stale, got %+v", st)
	}

	action, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRefreshed {
		t.Fatalf("action = %q, want %q", action, ActionRefreshed)
	}
	if read(t, hook) != HookScript {
		t.Error("stale hook was not refreshed to the embedded script")
	}
}

// TestInstallRepairsLostExecuteBit: a hook git will not run is as broken as a
// missing one, and it is ours, so repairing it needs no consent.
func TestInstallRepairsLostExecuteBit(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, HookScript, 0o644)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ours || !st.Stale {
		t.Fatalf("non-executable own hook should read as stale: %+v", st)
	}
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("execute bit not repaired: %v", info.Mode())
	}
}

// TestInstallAppendsToForeignShellHook is the never-clobber contract for a
// hook we CAN extend: their content survives byte-for-byte and our block
// invokes a sidecar script.
func TestInstallAppendsToForeignShellHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/bash\n# somebody else's hook\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionAppended {
		t.Fatalf("action = %q, want %q", action, ActionAppended)
	}
	body := read(t, hook)
	// Their content survives verbatim, in order, with our block inserted
	// directly after the shebang (never at EOF — see appendBlock).
	if !strings.HasPrefix(body, "#!/bin/bash\n") {
		t.Errorf("shebang no longer first:\n%s", body)
	}
	if !strings.HasSuffix(body, "# somebody else's hook\nmake lint\n") {
		t.Errorf("foreign hook content was modified:\n%s", body)
	}
	if !strings.Contains(body, appendBegin) || !strings.Contains(body, ManualLine()) {
		t.Errorf("append block missing:\n%s", body)
	}
	if strings.Index(body, appendBegin) > strings.Index(body, "make lint") {
		t.Errorf("gate block lands after the foreign hook's code:\n%s", body)
	}
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar script not written")
	}

	// Re-inspect: appended, not foreign, and not stale.
	st2, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Appended || st2.Foreign || st2.Stale {
		t.Fatalf("expected appended+current, got %+v", st2)
	}
	if again, _, err := Install(repo); err != nil || again != ActionUpToDate {
		t.Fatalf("second install = %q (%v), want %q", again, err, ActionUpToDate)
	}
}

// TestInstallRefreshesStaleSidecar: the foreign hook's block still points at
// the sidecar, so only the sidecar needs rewriting — their file is untouched.
func TestInstallRefreshesStaleSidecar(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\nexit 0\n", 0o755)
	if _, st, err := Install(repo); err != nil {
		t.Fatal(err)
	} else {
		writeHook(t, st.SidecarPath, "#!/bin/sh\n"+HookMarker+"\n# old\n", 0o755)
	}
	before := read(t, hook)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRefreshed {
		t.Fatalf("action = %q, want %q", action, ActionRefreshed)
	}
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar not refreshed")
	}
	if read(t, hook) != before {
		t.Error("foreign hook was rewritten during a sidecar refresh")
	}
}

// TestInstallNeverTouchesNonShellHook: a python (or binary, or symlinked)
// hook gets the manual one-liner, never an edit.
func TestInstallNeverTouchesNonShellHook(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/usr/bin/env python3\nprint('hi')\n"
	writeHook(t, hook, foreign, 0o755)

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionManual {
		t.Fatalf("action = %q, want %q", action, ActionManual)
	}
	if read(t, hook) != foreign {
		t.Error("a non-appendable foreign hook was modified")
	}
	// The sidecar IS written so the printed one-liner works immediately.
	if read(t, st.SidecarPath) != HookScript {
		t.Error("sidecar not written for the manual case")
	}
}

func TestInstallTreatsSymlinkedHookAsForeign(t *testing.T) {
	repo := newRepo(t)
	target := filepath.Join(t.TempDir(), "their-hook.sh")
	writeHook(t, target, "#!/bin/sh\nexit 0\n", 0o755)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hook); err != nil {
		t.Fatal(err)
	}

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Foreign || st.Appendable {
		t.Fatalf("symlinked hook must be foreign and non-appendable: %+v", st)
	}
	action, _, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionManual {
		t.Fatalf("action = %q, want %q", action, ActionManual)
	}
	info, err := os.Lstat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced")
	}
	if read(t, target) != "#!/bin/sh\nexit 0\n" {
		t.Error("symlink target was rewritten")
	}
}

func TestRemoveDeletesOwnHook(t *testing.T) {
	repo := newRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	action, st, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRemoved {
		t.Fatalf("action = %q, want %q", action, ActionRemoved)
	}
	if _, err := os.Stat(st.HookPath); !os.IsNotExist(err) {
		t.Errorf("hook still present: %v", err)
	}
}

func TestRemoveStripsAppendedBlockOnly(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/sh\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}

	action, st, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRemoved {
		t.Fatalf("action = %q, want %q", action, ActionRemoved)
	}
	if got := read(t, hook); got != foreign {
		t.Errorf("foreign hook not restored byte-for-byte:\n%q\nwant\n%q", got, foreign)
	}
	if _, err := os.Stat(st.SidecarPath); !os.IsNotExist(err) {
		t.Error("sidecar not removed")
	}
}

func TestRemoveLeavesForeignHookAlone(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/usr/bin/env python3\nprint('hi')\n"
	writeHook(t, hook, foreign, 0o755)

	action, _, err := Remove(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNothing {
		t.Fatalf("action = %q, want %q", action, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("foreign hook was modified by disable")
	}
}

// TestOwnershipIsAnchored is the regression for roast finding R-911598: a
// foreign file that merely mentions our markers must not be treated as ours
// (which would let `gate enable` overwrite it and `gate disable` delete it).
func TestOwnershipIsAnchored(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	// A hook that documents roast rather than being roast.
	foreign := "#!/bin/sh\n# we used to use " + HookMarker + " here\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ours {
		t.Fatalf("a foreign hook mentioning the marker was claimed as ours: %+v", st)
	}
	if !st.Foreign || !st.Appendable {
		t.Fatalf("expected foreign+appendable, got %+v", st)
	}
	if action, _, err := Remove(repo); err != nil || action != ActionNothing {
		t.Fatalf("remove = %q (%v), want %q", action, err, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("disable modified a foreign hook")
	}
}

// TestMalformedAppendBlockIsNeverStripped: a begin marker with no end (a
// hand-edit or a crash) must leave the file alone rather than deleting
// everything to EOF.
func TestMalformedAppendBlockIsNeverStripped(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	broken := "#!/bin/sh\n" + appendBegin + "\n# end marker deleted by hand\nmake lint\n"
	writeHook(t, hook, broken, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Appended {
		t.Fatalf("a truncated block read as an installed gate: %+v", st)
	}
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, hook) != broken {
		t.Errorf("a malformed block was stripped, taking the file with it:\n%s", read(t, hook))
	}
}

// TestAppendPreservesForeignMode is the regression for roast finding
// R-5dae8f: chmod -x is how people disable a hook, and roast must not undo
// that just because it appended a block.
func TestAppendPreservesForeignMode(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeHook(t, hook, "#!/bin/sh\nmake lint\n", 0o644)

	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("append made a deliberately non-executable hook executable: %v", info.Mode())
	}
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active() {
		t.Error("a non-executable hook reports as an active gate")
	}
	if !strings.Contains(st.Summary(), "NOT RUNNING") {
		t.Errorf("summary hides the dead gate: %q", st.Summary())
	}
}

// TestSidecarIsNeverClobbered is the regression for roast finding R-a90671:
// never-clobber covers every file roast did not write, not just `pre-commit`.
func TestSidecarIsNeverClobbered(t *testing.T) {
	repo := newRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"), "#!/bin/sh\nmake lint\n", 0o755)
	sidecar := filepath.Join(repo, ".git", "hooks", SidecarName)
	theirs := "#!/bin/sh\n# someone else got here first\n"
	writeHook(t, sidecar, theirs, 0o755)

	if _, _, err := Install(repo); err == nil {
		t.Fatal("install overwrote a foreign sidecar without complaint")
	}
	if read(t, sidecar) != theirs {
		t.Error("foreign sidecar was overwritten")
	}
}

// TestRemoveLeavesForeignSidecarAlone is the delete-side mirror of
// TestSidecarIsNeverClobbered (roast finding R-01c2b0): `gate disable` must
// not delete a file at the sidecar path that roast did not write.
func TestRemoveLeavesForeignSidecarAlone(t *testing.T) {
	repo := newRepo(t)
	sidecar := filepath.Join(repo, ".git", "hooks", SidecarName)
	theirs := "#!/bin/sh\n# not roast's\n"
	writeHook(t, sidecar, theirs, 0o755)

	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, sidecar) != theirs {
		t.Error("disable deleted a foreign file at the sidecar path")
	}

	// Ours, by contrast, does go.
	writeHook(t, sidecar, HookScript, 0o755)
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("roast's own sidecar survived disable: %v", err)
	}
}

// TestAtomicWriteDoesNotFollowAPlantedTempSymlink is the regression for roast
// finding R-306969: with core.hooksPath inside the working tree, a repo could
// plant a symlink at the old predictable temp path and have roast truncate
// whatever it pointed at.
func TestAtomicWriteDoesNotFollowAPlantedTempSymlink(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "config", "core.hooksPath", ".githooks")
	hooks := filepath.Join(repo, ".githooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "precious")
	writeHook(t, victim, "do not touch\n", 0o644)
	if err := os.Symlink(victim, filepath.Join(hooks, "pre-commit.roast-tmp")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	if got := read(t, victim); got != "do not touch\n" {
		t.Errorf("a planted temp symlink was followed; victim now:\n%s", got)
	}
}

// TestMarkerPairWithoutInvocationIsForeign is the regression for roast
// finding R-742ef8: the markers are public comment text, so a copy-pasted
// pair with no gate invocation between them is not roast's block.
func TestMarkerPairWithoutInvocationIsForeign(t *testing.T) {
	repo := newRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	foreign := "#!/bin/sh\n" + appendBegin + "\n# we deleted roast's line\n" + appendEnd + "\nmake lint\n"
	writeHook(t, hook, foreign, 0o755)

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Appended {
		t.Fatalf("a marker pair with no invocation read as roast's block: %+v", st)
	}
	if action, _, err := Remove(repo); err != nil || action != ActionNothing {
		t.Fatalf("remove = %q (%v), want %q", action, err, ActionNothing)
	}
	if read(t, hook) != foreign {
		t.Error("disable deleted a region of a foreign hook")
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	repo := newRepo(t)
	for _, d := range []Decision{DecisionEnabled, DecisionDeclined} {
		if err := RecordDecision(repo, d); err != nil {
			t.Fatal(err)
		}
		st, err := Inspect(repo)
		if err != nil {
			t.Fatal(err)
		}
		if st.Decision != d {
			t.Fatalf("decision = %q, want %q", st.Decision, d)
		}
	}
	// An unrecognizable recorded value reads as "still unanswered" rather
	// than silently enabling or permanently declining.
	git(t, repo, "config", "--local", "roast.gate", "maybe")
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Decision != DecisionNone {
		t.Errorf("garbage decision = %q, want none", st.Decision)
	}
}

// TestRecordDecisionClearsNotice: the one-time hint belongs to the undecided
// state only. Leaving it behind would silence a legitimate hint if the
// decision were later cleared.
func TestRecordDecisionClearsNotice(t *testing.T) {
	repo := newRepo(t)
	if err := RecordNotice(repo); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(repo); !st.NoticeSent {
		t.Fatal("notice was not recorded")
	}
	if err := RecordDecision(repo, DecisionEnabled); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.NoticeSent {
		t.Error("recording a decision left the hint marker behind")
	}
}

func TestHonorsCoreHooksPath(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "config", "core.hooksPath", ".githooks")

	action, st, err := Install(repo)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Fatalf("action = %q", action)
	}
	want := filepath.Join(repo, ".githooks", "pre-commit")
	if st.HookPath != want {
		t.Fatalf("HookPath = %q, want %q", st.HookPath, want)
	}
	if read(t, want) != HookScript {
		t.Error("hook not written to the core.hooksPath location")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "pre-commit")); !os.IsNotExist(err) {
		t.Error("hook also landed in .git/hooks despite core.hooksPath")
	}
}

func TestAppendable(t *testing.T) {
	cases := map[string]bool{
		"#!/bin/sh\n":              true,
		"#!/bin/bash -e\n":         true,
		"#!/usr/bin/env bash\n":    true,
		"#!/usr/bin/env zsh\nx\n":  true,
		"#!/usr/bin/env python3\n": false,
		"#!/usr/bin/perl\n":        false,
		"#!/usr/bin/env -S bash\n": false,
		"no shebang\n":             false,
		"":                         false,
		"\x00\x01binary":           false,
	}
	for body, want := range cases {
		if got := appendable(body); got != want {
			t.Errorf("appendable(%q) = %v, want %v", body, got, want)
		}
	}
}
