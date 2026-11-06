package scope

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// initRepo creates a temp git repo with user identity configured.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@example.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", c, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitAdd(t *testing.T, repo string, paths ...string) {
	t.Helper()
	args := append([]string{"-C", repo, "add"}, paths...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
}

func gitCommit(t *testing.T, repo, msg string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repo, "commit", "-m", msg).CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func gitRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepoWithDefaultBranch sets up a bare "remote" and a clone of it so
// refs/remotes/origin/HEAD is populated, matching what resolveBranchBase
// looks for in a real checkout. Returns the clone's path.
func initRepoWithDefaultBranch(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	seed := initRepo(t)
	writeFile(t, seed, "README.md", "seed\n")
	gitAdd(t, seed, ".")
	gitCommit(t, seed, "initial")
	gitRun(t, seed, "branch", "-M", "main")
	gitRun(t, seed, "remote", "add", "origin", remote)
	gitRun(t, seed, "push", "-q", "origin", "main")
	// git init --bare sets the new repo's HEAD symref from the ambient
	// init.defaultBranch (or the git-version default), independently of
	// what the seed pushes. On a host defaulting to "master" this bare repo
	// still points refs/remotes/origin/HEAD at a ref that was never pushed,
	// so the clone below would come up unborn and merge-base against it in
	// the branch-survey tests would fail with "unrelated histories" —
	// nondeterministically, depending on the test runner's git config. Pin
	// it explicitly instead of relying on that ambient default.
	gitRun(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", remote, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	gitRun(t, clone, "config", "user.email", "test@example.com")
	gitRun(t, clone, "config", "user.name", "Test")
	return clone
}

// TestRenameBecomesDeleteAdd confirms the vendor-escape bug class is dead:
// a file renamed INTO vendor/ must not appear in the diff because the
// destination path is excluded. The source side (delete) is outside vendor,
// so it IS in scope as a deletion.
func TestRenameBecomesDeleteAdd(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "main.go", "package main\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Simulate a rename by deleting old and creating new in vendor/.
	if err := os.Remove(filepath.Join(repo, "main.go")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "vendor/foo/main.go", "package main\n")
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	// vendor/foo/main.go must NOT appear (excluded).
	for _, f := range s.Files {
		if strings.HasPrefix(f, "vendor/") {
			t.Errorf("vendor file leaked into files list: %s", f)
		}
	}
	if strings.Contains(s.Diff, "vendor/foo/main.go") {
		t.Error("vendor path appeared in diff text")
	}
	// The deletion of main.go should appear.
	found := false
	for _, f := range s.Files {
		if f == "main.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected main.go (deletion) in files list")
	}
}

// TestExcludesFilterBothDiffAndFiles verifies excludes apply to both the diff
// text and the files list.
func TestExcludesFilterBothDiffAndFiles(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "keep.go", "package p\n")
	writeFile(t, repo, "skip.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	writeFile(t, repo, "keep.go", "package p\n// changed\n")
	writeFile(t, repo, "skip.go", "package p\n// changed\n")
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{
		StagedOnly: true,
		Excludes:   []string{"skip.go"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range s.Files {
		if f == "skip.go" {
			t.Error("excluded file appeared in files list")
		}
	}
	if strings.Contains(s.Diff, "skip.go") {
		t.Error("excluded file appeared in diff text")
	}
	found := false
	for _, f := range s.Files {
		if f == "keep.go" {
			found = true
		}
	}
	if !found {
		t.Error("kept file missing from files list")
	}
}

// TestUntrackedSynthesisHonorsExcludesAndPaths verifies untracked synthesis
// respects both excludes and path filters.
func TestUntrackedSynthesisHonorsExcludesAndPaths(t *testing.T) {
	repo := initRepo(t)

	// Create an initial commit so we have HEAD.
	writeFile(t, repo, "readme.txt", "hello\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// These are untracked files.
	writeFile(t, repo, "include.go", "package p\n")
	writeFile(t, repo, "exclude.go", "package p\n")
	writeFile(t, repo, "node_modules/dep.js", "// lib\n")

	s, err := Build(repo, BuildOpts{
		Excludes: []string{"exclude.go"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range s.Files {
		if f == "exclude.go" {
			t.Error("explicitly excluded untracked file appeared in files list")
		}
		if strings.HasPrefix(f, "node_modules/") {
			t.Error("node_modules untracked file appeared in files list")
		}
	}

	found := false
	for _, f := range s.Files {
		if f == "include.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected include.go in files list (untracked synthesis)")
	}
}

// TestStagedModeReadsIndexOnly verifies that staged content differs from
// the worktree when the file has been modified after staging.
func TestStagedModeReadsIndexOnly(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "file.go", "package p\n// v1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Stage v2.
	writeFile(t, repo, "file.go", "package p\n// v2\n")
	gitAdd(t, repo, ".")

	// Unstaged v3 (written after staging).
	writeFile(t, repo, "file.go", "package p\n// v3\n")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	// Staged diff should show v2 (not v3).
	if strings.Contains(s.Diff, "v3") {
		t.Error("staged diff includes unstaged worktree content (v3)")
	}
	if !strings.Contains(s.Diff, "v2") {
		t.Error("staged diff missing staged content (v2)")
	}
	if !s.Staged {
		t.Error("Staged flag not set")
	}
}

// TestEmptyRepoEmptyTreeFallback verifies a brand-new repo with no commits
// still produces a diff (staged content diff against the empty tree).
func TestEmptyRepoEmptyTreeFallback(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "new.go", "package p\n")
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	if s.Empty() {
		t.Error("expected non-empty diff for brand-new repo with staged file")
	}
	found := false
	for _, note := range s.Notes {
		if strings.Contains(note, "no commits") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'no commits' note")
	}
}

// TestFitTruncationAndBreakdown verifies Fit truncates the diff and returns
// a breakdown sorted largest-first.
func TestFitTruncationAndBreakdown(t *testing.T) {
	repo := initRepo(t)

	// Create a commit with two files of different sizes.
	big := strings.Repeat("x", 500) + "\n"
	small := "y\n"
	writeFile(t, repo, "big.go", big)
	writeFile(t, repo, "small.go", small)
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Modify both.
	writeFile(t, repo, "big.go", big+"// modified\n")
	writeFile(t, repo, "small.go", small+"// modified\n")
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Diff) <= 100 {
		t.Skip("diff too small to test truncation")
	}

	breakdown := Fit(&s, 100)
	if !strings.Contains(s.Diff, "... (diff truncated)") {
		t.Error("truncated marker not appended")
	}
	if len(breakdown) == 0 {
		t.Error("expected non-empty breakdown")
	}
	// Breakdown must be sorted largest first.
	for i := 1; i < len(breakdown); i++ {
		if breakdown[i].Bytes > breakdown[i-1].Bytes {
			t.Errorf("breakdown not sorted: [%d]=%d > [%d]=%d",
				i, breakdown[i].Bytes, i-1, breakdown[i-1].Bytes)
		}
	}
	truncNote := false
	for _, n := range s.Notes {
		if strings.Contains(n, "truncated") {
			truncNote = true
		}
	}
	if !truncNote {
		t.Error("truncation note not added")
	}
}

// TestBaseFallbackNotes verifies that when no base ref is found, a note is
// appended and build still returns uncommitted changes.
func TestBaseFallbackNotes(t *testing.T) {
	repo := initRepo(t)

	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Modify a file (uncommitted).
	writeFile(t, repo, "a.go", "package p\n// changed\n")

	s, err := Build(repo, BuildOpts{
		Base: "nonexistent-ref-abc123",
	})
	if err != nil {
		t.Fatal(err)
	}

	hasNote := false
	for _, n := range s.Notes {
		if strings.Contains(n, "no usable base ref") {
			hasNote = true
		}
	}
	if !hasNote {
		t.Error("expected 'no usable base ref' note when base ref missing")
	}
	// Should still show uncommitted changes.
	if s.Empty() {
		t.Error("expected non-empty diff even without base ref (uncommitted changes present)")
	}
}

// TestGlobMatch tests the cross-directory glob matching behaviour.
func TestGlobMatch(t *testing.T) {
	cases := []struct {
		path, pattern string
		want          bool
	}{
		{"vendor/foo/bar.go", "vendor/**", true},
		{"node_modules/dep.js", "node_modules/**", true},
		{"a/node_modules/dep.js", "**/node_modules/**", true},
		{"foo.lock", "*.lock", true},
		{"go.sum", "go.sum", true},
		{"foo.pb.go", "*.pb.go", true},
		{"keep.go", "*.pb.go", false},
		{"keep.go", "vendor/**", false},
	}
	for _, c := range cases {
		got := globMatch(c.path, c.pattern)
		if got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.path, c.pattern, got, c.want)
		}
	}
}

// TestUntrackedSymlinkNotSynthesized confirms an untracked symlink cannot
// exfiltrate its target's contents into the review prompt: os.ReadFile
// follows symlinks, so synthesis must Lstat-guard to regular files only.
func TestUntrackedSymlinkNotSynthesized(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "real.txt", "tracked content\n")
	gitAdd(t, repo, "real.txt")
	gitCommit(t, repo, "init")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET-OUTSIDE-REPO\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "leak")); err != nil {
		t.Fatal(err)
	}

	sc, err := Build(repo, BuildOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sc.Diff, "SECRET-OUTSIDE-REPO") {
		t.Fatal("symlink target contents leaked into the diff")
	}
	for _, f := range sc.Files {
		if f == "leak" {
			t.Fatal("symlink appeared in files list")
		}
	}
}

// --- survey scope: branch ---------------------------------------------------

// TestSurveyBranchManifest verifies branch-scope manifest content, Files
// derivation, and snapshot identity population.
func TestSurveyBranchManifest(t *testing.T) {
	repo := initRepoWithDefaultBranch(t)

	gitRun(t, repo, "checkout", "-qb", "feature")
	writeFile(t, repo, "README.md", "seed\n// changed\n")
	writeFile(t, repo, "new.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "feature work")

	s, err := Build(repo, BuildOpts{Scope: "branch"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !s.Survey {
		t.Error("expected Survey=true")
	}
	if s.Diff != "" {
		t.Error("expected Diff empty in survey mode")
	}
	if s.Manifest == "" {
		t.Fatal("expected non-empty Manifest")
	}
	if !strings.Contains(s.Manifest, "README.md") || !strings.Contains(s.Manifest, "new.go") {
		t.Error("manifest missing changed files in diffstat/name-status")
	}
	if !strings.Contains(s.Manifest, "feature work") {
		t.Error("manifest missing commit log entry")
	}
	wantFiles := []string{"README.md", "new.go"}
	if strings.Join(s.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("Files = %v, want %v", s.Files, wantFiles)
	}
	if s.BaseOID == "" || s.MergeBaseOID == "" || s.HeadOID == "" {
		t.Errorf("expected BaseOID/MergeBaseOID/HeadOID populated, got %q/%q/%q", s.BaseOID, s.MergeBaseOID, s.HeadOID)
	}
	if s.HeadOID == s.BaseOID {
		t.Error("expected HeadOID != BaseOID after a feature commit")
	}
	if s.Empty() {
		t.Error("expected non-empty survey scope with changed files")
	}
}

// TestSurveyBranchExcludesApplied verifies vendor/default-ignore excludes
// apply to the branch manifest and its Files list, same as diff scope.
func TestSurveyBranchExcludesApplied(t *testing.T) {
	repo := initRepoWithDefaultBranch(t)

	gitRun(t, repo, "checkout", "-qb", "feature")
	writeFile(t, repo, "keep.go", "package p\n")
	writeFile(t, repo, "vendor/foo/bar.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "add files")

	s, err := Build(repo, BuildOpts{Scope: "branch"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if strings.Contains(s.Manifest, "vendor/foo/bar.go") {
		t.Error("vendor path leaked into manifest")
	}
	for _, f := range s.Files {
		if strings.HasPrefix(f, "vendor/") {
			t.Errorf("vendor file leaked into Files: %s", f)
		}
	}
	found := false
	for _, f := range s.Files {
		if f == "keep.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected keep.go in Files")
	}
}

// TestSurveyBranchNoResolvableBaseErrors verifies branch scope fails loudly
// when no base can be resolved, and — critically — does not fall back to
// HEAD~1 (roast finding #7: that would silently turn "review the whole
// branch" into "review one commit").
func TestSurveyBranchNoResolvableBaseErrors(t *testing.T) {
	repo := initRepo(t) // no remote configured, so no origin/HEAD, no origin/main

	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "c1")
	writeFile(t, repo, "b.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "c2")

	_, err := Build(repo, BuildOpts{Scope: "branch"})
	if err == nil {
		t.Fatal("expected an error when no base ref resolves")
	}
	if strings.Contains(err.Error(), "HEAD~1") {
		t.Errorf("error must not mention HEAD~1 fallback: %v", err)
	}
}

// TestSurveyBranchExplicitBaseInvalidErrors verifies an explicit but
// unresolvable --base also fails rather than silently falling back.
func TestSurveyBranchExplicitBaseInvalidErrors(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "c1")

	_, err := Build(repo, BuildOpts{Scope: "branch", Base: "nonexistent-ref-xyz"})
	if err == nil {
		t.Fatal("expected an error for an unresolvable explicit base")
	}
}

// TestSurveyBranchDirtyTreeErrors verifies a dirty index/worktree fails
// branch scope by default (survey reads committed HEAD; a dirty tree would
// let tool reads disagree with the manifest over a long run).
func TestSurveyBranchDirtyTreeErrors(t *testing.T) {
	repo := initRepoWithDefaultBranch(t)
	gitRun(t, repo, "checkout", "-qb", "feature")
	writeFile(t, repo, "new.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "feature work")

	// Dirty the worktree post-commit.
	writeFile(t, repo, "new.go", "package p\n// dirty\n")

	_, err := Build(repo, BuildOpts{Scope: "branch"})
	if err == nil {
		t.Fatal("expected an error for a dirty worktree without IncludeWorktree")
	}
}

// TestSurveyBranchIncludeWorktreeProceedsOnDirtyTree verifies
// IncludeWorktree skips the dirty-tree check and lets the build proceed.
func TestSurveyBranchIncludeWorktreeProceedsOnDirtyTree(t *testing.T) {
	repo := initRepoWithDefaultBranch(t)
	gitRun(t, repo, "checkout", "-qb", "feature")
	writeFile(t, repo, "new.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "feature work")

	writeFile(t, repo, "new.go", "package p\n// dirty\n")

	s, err := Build(repo, BuildOpts{Scope: "branch", IncludeWorktree: true})
	if err != nil {
		t.Fatalf("expected IncludeWorktree to allow a dirty tree, got: %v", err)
	}
	if s.Manifest == "" {
		t.Error("expected a manifest even with a dirty tree under IncludeWorktree")
	}
}

// --- survey scope: repo -----------------------------------------------------

// TestSurveyRepoManifest verifies repo-scope manifest content and that
// Files equals the tracked-file set.
func TestSurveyRepoManifest(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "cmd/main.go", "package main\n")
	writeFile(t, repo, "internal/foo.go", "package foo\n")
	writeFile(t, repo, "README.md", "hi\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	s, err := Build(repo, BuildOpts{Scope: "repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if !s.Survey {
		t.Error("expected Survey=true")
	}
	if s.Diff != "" {
		t.Error("expected Diff empty in survey mode")
	}
	wantFiles := []string{"README.md", "cmd/main.go", "internal/foo.go"}
	if strings.Join(s.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("Files = %v, want %v", s.Files, wantFiles)
	}
	for _, f := range wantFiles {
		if !strings.Contains(s.Manifest, f) {
			t.Errorf("manifest missing tracked file %s", f)
		}
	}
	if s.HeadOID == "" {
		t.Error("expected HeadOID populated for repo scope with commits")
	}
	if s.Base != "" {
		t.Error("expected Base empty for repo scope")
	}
	if s.Empty() {
		t.Error("expected non-empty survey scope with tracked files")
	}
}

// TestSurveyRepoExcludesApplied verifies excludes apply to the repo
// manifest's tracked-file listing and Files.
func TestSurveyRepoExcludesApplied(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "keep.go", "package p\n")
	writeFile(t, repo, "vendor/foo/bar.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	s, err := Build(repo, BuildOpts{Scope: "repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if strings.Contains(s.Manifest, "vendor/foo/bar.go") {
		t.Error("vendor path leaked into repo manifest")
	}
	for _, f := range s.Files {
		if strings.HasPrefix(f, "vendor/") {
			t.Errorf("vendor file leaked into Files: %s", f)
		}
	}
}

// TestSurveyRepoEmptyWhenNoTrackedFiles verifies Empty() treats a repo with
// no tracked files as empty, even though it would produce a (short) manifest
// string — Empty() for survey scope is judged by Files, not by string
// blankness.
func TestSurveyRepoEmptyWhenNoTrackedFiles(t *testing.T) {
	repo := initRepo(t) // no commits, no tracked files

	s, err := Build(repo, BuildOpts{Scope: "repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !s.Empty() {
		t.Error("expected Empty()=true for a repo with no tracked files")
	}
}

// --- diff-scope regression guard --------------------------------------------

// TestDiffScopeUnchangedByDefaultAndExplicit verifies that Build with
// Scope=="" and Scope=="diff" both produce byte-for-byte identical output to
// each other, and that adding the Scope field did not alter the default
// (empty-scope) code path's behavior versus the pre-existing diff-mode
// tests above (TestRenameBecomesDeleteAdd, TestExcludesFilterBothDiffAndFiles,
// etc., which all call Build with no Scope set and continue to pass).
func TestDiffScopeUnchangedByDefaultAndExplicit(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	writeFile(t, repo, "a.go", "package p\n// changed\n")
	gitAdd(t, repo, ".")

	base, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatalf("Build (default scope): %v", err)
	}
	explicit, err := Build(repo, BuildOpts{StagedOnly: true, Scope: "diff"})
	if err != nil {
		t.Fatalf("Build (scope=diff): %v", err)
	}

	if base.Diff != explicit.Diff {
		t.Errorf("Diff differs between default scope and explicit 'diff' scope:\n%q\nvs\n%q", base.Diff, explicit.Diff)
	}
	if strings.Join(base.Files, ",") != strings.Join(explicit.Files, ",") {
		t.Errorf("Files differ between default scope and explicit 'diff' scope: %v vs %v", base.Files, explicit.Files)
	}
	if base.Survey || explicit.Survey {
		t.Error("expected Survey=false for diff scope")
	}
	if base.Manifest != "" || explicit.Manifest != "" {
		t.Error("expected empty Manifest for diff scope")
	}
}

// --- roast-finding regression guards -----------------------------------

// TestSurveyRepoFilesReflectHeadOIDNotIndex verifies buildSurveyRepo's Files
// list is derived from the HeadOID tree, not the mutable index: a staged
// (but uncommitted) addition must not appear, since it doesn't exist at
// HeadOID (roast finding, confirmed MEDIUM, 2026-07-22).
func TestSurveyRepoFilesReflectHeadOIDNotIndex(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "committed.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Stage an addition without committing — must NOT appear in a
	// HeadOID-pinned repo-scope manifest.
	writeFile(t, repo, "staged_only.go", "package p\n")
	gitAdd(t, repo, "staged_only.go")

	s, err := Build(repo, BuildOpts{Scope: "repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range s.Files {
		if f == "staged_only.go" {
			t.Error("staged-but-uncommitted file appeared in HeadOID-pinned repo-scope Files")
		}
	}
	found := false
	for _, f := range s.Files {
		if f == "committed.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected committed.go in Files")
	}
}

// TestBuildUnknownScopeErrors verifies an unrecognized Scope value fails
// loudly instead of silently degrading to diff mode (roast finding,
// confirmed LOW, 2026-07-22).
func TestBuildUnknownScopeErrors(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	_, err := Build(repo, BuildOpts{Scope: "brnch"})
	if err == nil {
		t.Fatal("expected an error for an unrecognized Scope value")
	}
}

// TestSurveyBranchNameStatusDecodesQuotedPaths verifies branch scope decodes
// git's core.quotepath C-quoting of --name-status output (paths with a
// non-ASCII byte, tab, newline, quote, or backslash) instead of storing the
// display-quoted literal in Files — that literal doesn't exist on disk and
// can't be read by a later file-context lookup (roast finding, confirmed
// MEDIUM, 2026-07-22).
func TestSurveyBranchNameStatusDecodesQuotedPaths(t *testing.T) {
	clone := initRepoWithDefaultBranch(t)
	// A non-ASCII filename triggers git's default core.quotepath C-quoting
	// for --name-status without -z.
	const special = "café.go"
	writeFile(t, clone, special, "package p\n")
	gitAdd(t, clone, ".")
	gitCommit(t, clone, "add special file")

	s, err := Build(clone, BuildOpts{Scope: "branch"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, f := range s.Files {
		if f == special {
			found = true
		}
		if strings.HasPrefix(f, `"`) {
			t.Errorf("Files contains an undecoded quoted path: %q", f)
		}
	}
	if !found {
		t.Errorf("expected decoded %q in Files, got %v", special, s.Files)
	}
}

// TestSurveyRepoDirtyTreeDoesNotDriftLinguistExcludes verifies that an
// uncommitted .gitattributes edit cannot narrow repo scope's HeadOID-pinned
// Files below what HeadOID actually contains: linguist-attribute excludes
// are resolved from the mutable index/worktree, so they must be dropped (not
// applied) whenever the tree is dirty, rather than silently hiding a
// committed file that HeadOID's own attributes don't exclude (roast finding,
// confirmed MEDIUM, 2026-07-22).
func TestSurveyRepoDirtyTreeDoesNotDriftLinguistExcludes(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "tracked.go", "package p\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Dirty the tree with an uncommitted .gitattributes marking the
	// already-committed file linguist-generated. HeadOID has no such rule.
	writeFile(t, repo, ".gitattributes", "tracked.go linguist-generated\n")

	s, err := Build(repo, BuildOpts{Scope: "repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, f := range s.Files {
		if f == "tracked.go" {
			found = true
		}
	}
	if !found {
		t.Error("tracked.go dropped from Files by an uncommitted .gitattributes edit; repo scope must reflect HeadOID, not the dirty worktree")
	}
}

// TestSurveyRepoDirectoryPathspecMatchesSubtree verifies repo scope honors a
// bare directory pathspec (e.g. "internal") the way git itself does for diff
// scope — recursively selecting everything under it — instead of requiring
// an exact full-path match via the in-process glob matcher (roast finding,
// confirmed MEDIUM, 2026-07-22).
func TestSurveyRepoDirectoryPathspecMatchesSubtree(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "internal/foo.go", "package foo\n")
	writeFile(t, repo, "cmd/main.go", "package main\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	s, err := Build(repo, BuildOpts{Scope: "repo", Paths: []string{"internal"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantFiles := []string{"internal/foo.go"}
	if strings.Join(s.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("Files = %v, want %v", s.Files, wantFiles)
	}
}

// TestStagedModeHonorsGitIndexFile is the invariant the native pre-commit
// gate rests on: git points GIT_INDEX_FILE at a temporary index holding
// exactly the commit's content (for a path-scoped `git commit <path>`, only
// that path), and --staged must read THAT index, not the repo's live one.
// scope shells out to git without clearing the environment, which is what
// makes this work — a cmd.Env that dropped the inherited value would
// silently review the wrong content.
func TestStagedModeHonorsGitIndexFile(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package p\n// a-v1\n")
	writeFile(t, repo, "b.go", "package p\n// b-v1\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Live index: a.go staged. This is the dirty shared index the gate must
	// NOT review when the commit is scoped to b.go.
	writeFile(t, repo, "a.go", "package p\n// a-v2-staged-not-committed\n")
	writeFile(t, repo, "b.go", "package p\n// b-v2-in-this-commit\n")
	gitAdd(t, repo, "a.go")

	// Alternate index holding only b.go's change — what git hands a hook for
	// `git commit b.go`.
	alt := filepath.Join(t.TempDir(), "next-index")
	t.Setenv("GIT_INDEX_FILE", alt)
	gitRun(t, repo, "read-tree", "HEAD")
	gitRun(t, repo, "add", "b.go")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Files, ",") != "b.go" {
		t.Errorf("Files = %v, want [b.go]", s.Files)
	}
	if !strings.Contains(s.Diff, "b-v2-in-this-commit") {
		t.Errorf("diff missing the committed content:\n%s", s.Diff)
	}
	if strings.Contains(s.Diff, "a-v2-staged-not-committed") {
		t.Errorf("diff leaked the live index's staged content:\n%s", s.Diff)
	}
}

// gitMove is git mv into a directory that may not exist yet.
func gitMove(t *testing.T, repo, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(to)), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "mv", from, to)
}

func bigFile(lines int, extra string) string {
	var b strings.Builder
	b.WriteString("package p\n")
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "var v%d = %d\n", i, i)
	}
	b.WriteString(extra)
	return b.String()
}

func countAddedLines(diff string) int {
	n := 0
	for l := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			n++
		}
	}
	return n
}

// A moved file with a small edit is shown as git's rename section with only
// the edited lines, not as a whole-file delete plus a whole-file add.
func TestMovedFilesCollapseToRenameSections(t *testing.T) {
	repo := initRepoWithDefaultBranch(t)
	gitRun(t, repo, "switch", "-q", "-c", "move")
	writeFile(t, repo, "pkg/big.go", bigFile(200, ""))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "add big")
	gitMove(t, repo, "pkg/big.go", "moved/big.go")
	writeFile(t, repo, "moved/big.go", bigFile(200, "var edited = true\n"))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "move big")

	s, err := Build(repo, BuildOpts{Base: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rename from pkg/big.go", "rename to moved/big.go", "+var edited = true"} {
		if !strings.Contains(s.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, s.Diff)
		}
	}
	if strings.Contains(s.Diff, "deleted file mode") && strings.Contains(s.Diff, "--- a/pkg/big.go\n+++ /dev/null") {
		t.Errorf("moved file still shown as a whole-file deletion")
	}
	if n := countAddedLines(s.Diff); n > 20 {
		t.Errorf("diff adds %d lines; a move with one edit should add a handful", n)
	}
	if !slices.ContainsFunc(s.Notes, func(n string) bool { return strings.Contains(n, "collapsed") }) {
		t.Errorf("notes = %v, want a collapse note", s.Notes)
	}
}

func TestPureMoveHasNoHunks(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "src/x.go", bigFile(50, ""))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	gitMove(t, repo, "src/x.go", "lib/x.go")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Diff, "similarity index 100%") || !strings.Contains(s.Diff, "rename to lib/x.go") {
		t.Fatalf("diff = %s", s.Diff)
	}
	if strings.Contains(s.Diff, "@@") {
		t.Fatalf("pure move has hunks: %s", s.Diff)
	}
	for _, f := range []string{"src/x.go", "lib/x.go"} {
		if !slices.Contains(s.Files, f) {
			t.Errorf("files = %v, missing %s", s.Files, f)
		}
	}
}

// Moving a file across an exclude boundary must never pair it: the excluded
// side stays invisible and the included side is shown in full.
func TestMovesAcrossExcludesAreNotPaired(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "src/in.go", "package in\n\nfunc In() string { return \"in\" }\n")
	writeFile(t, repo, "vendor/lib/out.go", "package out\n\nvar vendored = 1\n\nfunc Out() int { return vendored }\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	gitMove(t, repo, "src/in.go", "vendor/lib/in.go")
	gitMove(t, repo, "vendor/lib/out.go", "src/out.go")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Diff, "vendor/") {
		t.Fatalf("vendored path surfaced: %s", s.Diff)
	}
	if strings.Contains(s.Diff, "rename ") {
		t.Fatalf("a move across the exclude boundary was paired: %s", s.Diff)
	}
	if !strings.Contains(s.Diff, "deleted file mode") || !strings.Contains(s.Diff, "+var vendored = 1") {
		t.Fatalf("expected a plain delete of src/in.go and a full add of src/out.go: %s", s.Diff)
	}
}

func TestUncommittedMoveCollapses(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a/f.go", bigFile(80, ""))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	gitMove(t, repo, "a/f.go", "b/f.go")

	s, err := Build(repo, BuildOpts{UncommittedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Diff, "rename from a/f.go") || countAddedLines(s.Diff) != 0 {
		t.Fatalf("diff = %s", s.Diff)
	}
}

func TestFitBreakdownCountsCollapsedMoves(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a/f.go", bigFile(80, ""))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	gitMove(t, repo, "a/f.go", "b/f.go")
	writeFile(t, repo, "b/f.go", bigFile(80, "var changed = true\n"))
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Diff, "rename to b/f.go") {
		t.Fatalf("move not collapsed: %s", s.Diff)
	}
	for _, f := range []string{"a/f.go", "b/f.go"} {
		if fileDiffBytes(&s, f) == 0 {
			t.Errorf("fileDiffBytes(%s) = 0 for a collapsed move", f)
		}
	}
}

// The rename parser walks --name-status -z records, not fields: a path that
// starts with "R" (README.md sorts before lowercase paths) must not be read
// as a rename status and knock the following move out of step.
func TestMoveCollapsesAfterPathStartingWithR(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "README.md", "# readme\n")
	writeFile(t, repo, "src/x.go", bigFile(50, ""))
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	writeFile(t, repo, "README.md", "# readme\n\nedited\n")
	gitMove(t, repo, "src/x.go", "lib/x.go")
	gitAdd(t, repo, ".")

	s, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Diff, "rename from src/x.go") || strings.Contains(s.Diff, "deleted file mode") {
		t.Fatalf("move after README.md was not collapsed:\n%s", s.Diff)
	}
	if !strings.Contains(s.Diff, "+edited") {
		t.Fatalf("README.md edit missing:\n%s", s.Diff)
	}
}
