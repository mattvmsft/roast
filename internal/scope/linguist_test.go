package scope

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLinguistIgnores covers the .gitattributes value forms: bare (set) and
// =true count as generated/vendored; =false and unspecified do not.
func TestLinguistIgnores(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "gen_true.json", "{}\n")
	writeFile(t, repo, "gen_bare.txt", "x\n")
	writeFile(t, repo, "vend.json", "{}\n")
	writeFile(t, repo, "gen_false.json", "{}\n")
	writeFile(t, repo, "normal.go", "package x\n")
	writeFile(t, repo, ".gitattributes",
		"gen_true.json linguist-generated=true\n"+
			"gen_bare.txt linguist-generated\n"+
			"vend.json linguist-vendored=true\n"+
			"gen_false.json linguist-generated=false\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	got := linguistIgnores(repo, false, "")
	want := []string{"gen_bare.txt", "gen_true.json", "vend.json"}
	if !slices.Equal(got, want) {
		t.Errorf("linguistIgnores() = %v, want %v", got, want)
	}
}

// TestLinguistIgnoresNoAttributes: a repo with no .gitattributes yields no
// excludes (and does not error).
func TestLinguistIgnoresNoAttributes(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package a\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	if got := linguistIgnores(repo, false, ""); len(got) != 0 {
		t.Errorf("linguistIgnores() = %v, want empty", got)
	}
}

// TestLinguistGeneratedExcludedFromDiff is the end-to-end check: a file marked
// linguist-generated is absent from the diff and Files by default, and present
// with IncludeVendored. This is the "ignore vendored files for all users of
// the repo" behavior, driven by the repo's own .gitattributes.
func TestLinguistGeneratedExcludedFromDiff(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "package x\n")
	writeFile(t, repo, "schema.json", "{}\n")
	writeFile(t, repo, ".gitattributes", "schema.json linguist-generated=true\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Change both files so both would otherwise appear in the diff.
	writeFile(t, repo, "code.go", "package x\n\nfunc F() {}\n")
	writeFile(t, repo, "schema.json", "{\"changed\": true}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "change both")

	// Default: schema.json is suppressed, code.go is reviewed.
	sc, err := Build(repo, BuildOpts{Base: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sc.Files, "schema.json") {
		t.Errorf("schema.json should be excluded by default; Files=%v", sc.Files)
	}
	if !slices.Contains(sc.Files, "code.go") {
		t.Errorf("code.go should be reviewed; Files=%v", sc.Files)
	}

	// IncludeVendored: the escape hatch brings the marked file back.
	scv, err := Build(repo, BuildOpts{Base: "HEAD~1", IncludeVendored: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(scv.Files, "schema.json") {
		t.Errorf("schema.json should be reviewed with IncludeVendored; Files=%v", scv.Files)
	}
}

// TestLinguistLiteralPathspec is the regression for the metacharacter-injection
// finding: a tracked file literally named with pathspec metacharacters, marked
// generated, must exclude ONLY itself — not every file its name would match if
// interpreted as a glob. Exercised via a bracket char-class name because git's
// glob pathspec treats `[ab]` as a class; :(literal) magic keeps it a filename.
func TestLinguistLiteralPathspec(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.go", "package a\n")
	writeFile(t, repo, "b.go", "package b\n")
	// A file whose literal name is a glob that would match a.go and b.go.
	writeFile(t, repo, "[ab].go", "package weird\n")
	writeFile(t, repo, ".gitattributes", "[[]ab[]].go linguist-generated=true\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Change all three so all would otherwise be in the diff.
	writeFile(t, repo, "a.go", "package a\n\nfunc A() {}\n")
	writeFile(t, repo, "b.go", "package b\n\nfunc B() {}\n")
	writeFile(t, repo, "[ab].go", "package weird\n\nfunc W() {}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "change all")

	sc, err := Build(repo, BuildOpts{Base: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the literally-named file is excluded; a.go and b.go survive.
	if slices.Contains(sc.Files, "[ab].go") {
		t.Errorf("[ab].go should be excluded; Files=%v", sc.Files)
	}
	if !slices.Contains(sc.Files, "a.go") || !slices.Contains(sc.Files, "b.go") {
		t.Errorf("a.go and b.go must NOT be swept up by a metacharacter filename; Files=%v", sc.Files)
	}
}

// TestLinguistStagedUsesIndexAttributes is the regression for the TOCTOU
// finding: in staged-only mode, attributes resolve from the index, so an
// UNSTAGED .gitattributes edit cannot pull a staged file out of the gate's
// scope. The invariant is "staged-only == exactly what the commit contains."
func TestLinguistStagedUsesIndexAttributes(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "schema.json", "{}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Stage a change to schema.json — this IS part of the would-be commit.
	writeFile(t, repo, "schema.json", "{\"v\": 1}\n")
	gitAdd(t, repo, "schema.json")

	// Now write an UNSTAGED .gitattributes marking it generated. In the index
	// there is no such attribute, so staged-only must still review schema.json.
	writeFile(t, repo, ".gitattributes", "schema.json linguist-generated=true\n")

	sc, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sc.Files, "schema.json") {
		t.Errorf("staged schema.json must be reviewed despite an unstaged generated marker; Files=%v", sc.Files)
	}

	// Once the marker is staged too, it becomes part of the commit and applies.
	gitAdd(t, repo, ".gitattributes")
	sc2, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sc2.Files, "schema.json") {
		t.Errorf("with the marker staged, schema.json should be excluded; Files=%v", sc2.Files)
	}
}

// TestLinguistExclusionsHonorThePinnedTree is the regression for roast R-65db13:
// the staged diff and every content reader resolve against a pinned tree, but
// the linguist exclusion set — which decides WHICH PATHS ENTER THE REVIEW AT ALL
// — was read from the LIVE index. So a .gitattributes staged after the pin could
// mark a file that exists in the pinned tree as generated and silently drop it.
//
// Scope-narrowing is the worse direction for this race: the review shrinks, the
// dropped file is never mentioned, and the diff the reviewer sees is a subset of
// the commit with nothing saying so.
func TestLinguistExclusionsHonorThePinnedTree(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "schema.json", "{}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// The commit under review changes schema.json, with no attribute marker
	// anywhere. This is the state the pin captures.
	writeFile(t, repo, "schema.json", "{\"v\": 1}\n")
	gitAdd(t, repo, "schema.json")
	pinned := gitRun(t, repo, "write-tree")
	if pinned == "" {
		t.Fatal("test premise: write-tree must produce a tree")
	}

	// A concurrent process (another session sharing the tree, a formatter with
	// auto-stage, a just recipe) now stages a marker. The live index has it; the
	// pinned tree does not.
	writeFile(t, repo, ".gitattributes", "schema.json linguist-generated=true\n")
	gitAdd(t, repo, ".gitattributes")

	sc, err := Build(repo, BuildOpts{StagedOnly: true, StagedTreeOID: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sc.Files, "schema.json") {
		t.Errorf("a .gitattributes staged AFTER the pin dropped a file that is in the pinned tree; Files=%v\n"+
			"the exclusion set must resolve against the pinned tree, not the live index", sc.Files)
	}
	if strings.Contains(sc.Diff, "linguist-generated") {
		t.Errorf("the post-pin .gitattributes leaked into the reviewed diff; Diff=%q", sc.Diff)
	}

	// Control: with no pin, the live index legitimately governs, which is the
	// pre-existing behavior every non-staged caller still gets.
	unpinned, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(unpinned.Files, "schema.json") {
		t.Errorf("test premise: without a pin the live index's marker should apply; Files=%v", unpinned.Files)
	}
}

// TestLinguistStagedDeletionExcluded is the regression for the staged-deletion
// finding: deleting a linguist-marked file removes it from the index (so
// ls-files no longer lists it), but its deletion still shows in git diff
// --cached. That removal-diff must stay excluded from a staged-only review.
func TestLinguistStagedDeletionExcluded(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "keep.go", "package x\n")
	writeFile(t, repo, "schema.json", "{\"big\": true}\n")
	writeFile(t, repo, ".gitattributes", "schema.json linguist-generated=true\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Stage a deletion of the generated file plus a real code change.
	if out, err := exec.Command("git", "-C", repo, "rm", "schema.json").CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v\n%s", err, out)
	}
	writeFile(t, repo, "keep.go", "package x\n\nfunc F() {}\n")
	gitAdd(t, repo, "keep.go")

	sc, err := Build(repo, BuildOpts{StagedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sc.Files, "schema.json") {
		t.Errorf("staged deletion of a generated file should be excluded; Files=%v", sc.Files)
	}
	if !slices.Contains(sc.Files, "keep.go") {
		t.Errorf("keep.go should still be reviewed; Files=%v", sc.Files)
	}
}

// TestLinguistIgnoresGlobalAttributes is the regression for the
// attribute-source finding: only in-tree/index .gitattributes may narrow
// review scope. A global core.attributesFile marking everything generated must
// NOT suppress files — it is machine-level config, not a committed,
// PR-reviewable claim.
func TestLinguistIgnoresGlobalAttributes(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "code.go", "package x\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// A global attributes file that marks every path generated. Wire it up
	// via an isolated global git config so the real environment is untouched.
	globalAttrs := filepath.Join(t.TempDir(), "attributes")
	if err := os.WriteFile(globalAttrs, []byte("* linguist-generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[core]\n\tattributesFile = "+globalAttrs+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	// Sanity: the global attribute IS visible to a plain check-attr, so the
	// test would fail without the suppression.
	if got := linguistIgnores(repo, false, ""); len(got) != 0 {
		t.Errorf("global attributesFile must not narrow scope; linguistIgnores()=%v", got)
	}
}

// TestLinguistGeneratedExcludesUntracked: the marker also suppresses untracked
// files, matching how DefaultIgnores applies to untracked synthesis.
func TestLinguistGeneratedExcludesUntracked(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "keep.go", "package x\n")
	writeFile(t, repo, ".gitattributes", "*.gen.json linguist-generated=true\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")

	// Untracked files: one normal, one matching the generated glob.
	writeFile(t, repo, "new.go", "package y\n")
	writeFile(t, repo, "big.gen.json", "{\"a\": 1}\n")

	sc, err := Build(repo, BuildOpts{UncommittedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sc.Files, "big.gen.json") {
		t.Errorf("untracked big.gen.json should be excluded; Files=%v", sc.Files)
	}
	if !slices.Contains(sc.Files, "new.go") {
		t.Errorf("untracked new.go should be reviewed; Files=%v", sc.Files)
	}
}
