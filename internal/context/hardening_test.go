package context

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/snapshot"
)

// liveFixture is a repository whose live working tree holds everything a
// pinned tree leaves out: Git's own directory, an ignored secret, an
// untracked denylisted credential, and a symlink into .git.
func liveFixture(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	writeFile(t, repo, ".gitignore", ".env\nbuild/\n")
	writeFile(t, repo, "a.go", "package a\n\nfunc A() {}\n")
	writeFile(t, repo, "id_test.go", "package a\n\nfunc TrackedDenylistShape() {}\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "initial")
	if out, err := runGit(repo, "config", "remote.origin.url", "https://user:FAKETOKEN@example.com/x/y"); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	writeFile(t, repo, ".env", "API_TOKEN=GITIGNORED_SECRET\n")
	writeFile(t, repo, "build/out.go", "package build // GITIGNORED_SECRET\n")
	writeFile(t, repo, "credentials.json", `{"token":"DENYLISTED_SECRET"}`+"\n")
	writeFile(t, repo, "new.go", "package a\n\nfunc New() {}\n")
	if err := os.Symlink(".git", filepath.Join(repo, "gitlink")); err != nil {
		t.Fatal(err)
	}
	return repo
}

func runGit(repo string, args ...string) (string, error) {
	out, err := literalGit(repo, args...).CombinedOutput()
	return string(out), err
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestLiveTreeServesOnlyTheReviewCorpus: when no tree could be pinned, the
// live working tree is served under the rules a pinned tree enforces by
// construction — never .git (in any case spelling or through a symlink),
// never an ignored file, never an untracked denylisted credential.
func TestLiveTreeServesOnlyTheReviewCorpus(t *testing.T) {
	repo := liveFixture(t)
	live := Tree{}
	secrets := []string{"FAKETOKEN", "GITIGNORED_SECRET", "DENYLISTED_SECRET", "repositoryformatversion"}
	leaks := func(what string, v any) {
		t.Helper()
		s := asJSON(v)
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("%s leaked %s: %s", what, secret, s)
			}
		}
	}

	for _, p := range []string{".git/config", ".GIT/config", ".Git/HEAD", "gitlink/config", ".env", "build/out.go", "credentials.json", "./.git/config", "a/../.git/config"} {
		res := readFileTool(repo, map[string]any{"path": p}, live)
		if _, ok := res["error"]; !ok {
			t.Errorf("read_file(%q) served content: %s", p, asJSON(res))
		}
		leaks("read_file "+p, res)
		leaks("NumberedSnippet "+p, NumberedSnippet(repo, p, 10_000, live))
	}
	for _, p := range []string{"a.go", "new.go", "id_test.go"} {
		if res := readFileTool(repo, map[string]any{"path": p}, live); res["error"] != nil {
			t.Errorf("read_file(%q) should serve a reviewed file: %v", p, res["error"])
		}
	}

	ground := NewQuoteGrounder(repo, live, "", "")
	if ground(".git/config", "url = https://user:FAKETOKEN@example.com/x/y") || ground(".env", "API_TOKEN=GITIGNORED_SECRET") || ground("credentials.json", `"token":"DENYLISTED_SECRET"`) {
		t.Error("grounder accepted a quote from outside the review corpus")
	}
	if !ground("new.go", "func New() {}") {
		t.Error("grounder must still ground untracked, non-ignored files")
	}

	grep := grepRepoTool(repo, map[string]any{"pattern": "SECRET|FAKETOKEN|func"}, live)
	leaks("grep_repo", grep["matches"])
	if !strings.Contains(asJSON(grep), "func New") {
		t.Errorf("grep_repo lost the reviewed untracked file: %s", asJSON(grep))
	}

	list := listFilesTool(repo, map[string]any{"pattern": "**"}, live)
	got := asJSON(list)
	for _, hidden := range []string{"credentials.json", "build/out.go", "gitlink", ".env"} {
		if strings.Contains(got, hidden) {
			t.Errorf("list_files listed %s: %s", hidden, got)
		}
	}
	for _, want := range []string{"a.go", "new.go", "id_test.go"} {
		if !strings.Contains(got, `"`+want+`"`) {
			t.Errorf("list_files lost %s: %s", want, got)
		}
	}
	for _, dir := range []string{".git", "build", "gitlink"} {
		if res := listFilesTool(repo, map[string]any{"directory": dir}, live); res["error"] == nil {
			t.Errorf("list_files(directory=%q) should be refused: %s", dir, asJSON(res))
		}
	}
}

// TestPinnedTreeLeavesOutWhatTheLiveGuardBlocks keeps the two corpora honest
// with each other: what the live guard refuses is exactly what pinning leaves
// out.
func TestPinnedTreeLeavesOutWhatTheLiveGuardBlocks(t *testing.T) {
	repo := liveFixture(t)
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatalf("PinWorktree: %v", err)
	}
	tree := PinnedWorktree(pin.Tree)
	for _, p := range []string{".git/config", ".env", "build/out.go", "credentials.json"} {
		if res := readFileTool(repo, map[string]any{"path": p}, tree); res["error"] == nil {
			t.Errorf("pinned read_file(%q) served content: %s", p, asJSON(res))
		}
	}
	if res := readFileTool(repo, map[string]any{"path": "new.go"}, tree); res["error"] != nil {
		t.Errorf("pinned tree lost an untracked reviewed file: %v", res["error"])
	}
}

// TestToolResultsAreBounded: every exploration tool result stays within
// toolOutputCap whatever the model requests or the repository holds.
func TestToolResultsAreBounded(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "wide.txt", strings.Repeat(strings.Repeat("A", 100_000)+"\n", 40))
	writeFile(t, repo, "tall.txt", strings.Repeat("short line\n", 200_000))
	writeFile(t, repo, "min.js", strings.Repeat("B", 5_000_000))
	var many strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&many, "needle %d %s\n", i, strings.Repeat("x", 500))
	}
	writeFile(t, repo, "many.txt", many.String())
	long := strings.Repeat("d", 200)
	for i := 0; i < 400; i++ {
		writeFile(t, repo, fmt.Sprintf("%s/%s/f%03d.go", long, long, i), "package p\n")
	}
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "big")
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	const slack = 512 // the trailing hint lines
	for _, tree := range []Tree{{}, PinnedWorktree(pin.Tree)} {
		name := "live"
		if tree.Pinned() {
			name = "pinned"
		}
		for _, args := range []map[string]any{
			{"path": "wide.txt", "limit": 1e9},
			{"path": "tall.txt", "limit": 1e9},
			{"path": "tall.txt", "offset": 1e18, "limit": 1e18},
			{"path": "min.js"},
		} {
			res := readFileTool(repo, args, tree)
			content, _ := res["content"].(string)
			if len(content) > toolOutputCap+slack {
				t.Errorf("%s read_file(%v) returned %d bytes", name, args, len(content))
			}
			if args["path"] == "wide.txt" && (!strings.Contains(content, "output capped") || fmt.Sprint(res["more_below"]) != "true") {
				t.Errorf("%s read_file must say it capped the output: %.200s", name, content)
			}
		}
		res := readFileTool(repo, map[string]any{"path": "wide.txt", "offset": 1, "limit": 3}, tree)
		if r := fmt.Sprint(res["returned_range"]); r != "[1 1]" || !strings.Contains(res["content"].(string), "offset>=2") {
			t.Errorf("%s read_file must resume after the last whole line served: range %s", name, r)
		}
		for _, pattern := range []string{"needle", "B"} {
			out := asJSON(grepRepoTool(repo, map[string]any{"pattern": pattern}, tree))
			if len(out) > toolOutputCap+slack {
				t.Errorf("%s grep_repo(%s) returned %d bytes", name, pattern, len(out))
			}
			if !strings.Contains(out, "truncated") {
				t.Errorf("%s grep_repo(%s) must mark the truncation: %.200s", name, pattern, out)
			}
		}
		if out := asJSON(listFilesTool(repo, map[string]any{"pattern": "*.go"}, tree)); len(out) > toolOutputCap+slack || !strings.Contains(out, "truncated at") {
			t.Errorf("%s list_files returned %d bytes without a truncation marker", name, len(out))
		}
	}
}

// TestLongLineDoesNotSwallowTheNextLine: a line longer than the per-line cap
// but shorter than the read buffer arrives with its newline in the same
// chunk. Cutting it must not leave the reader to "drain" into the next line.
func TestLongLineDoesNotSwallowTheNextLine(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", strings.Repeat("A", maxLineBytes+100)+"\nsecond line\n"+
		"needle "+strings.Repeat("n", grepLineBytes+100)+"\nneedle short\n")
	res := readFileTool(repo, map[string]any{"path": "f.txt"}, Tree{})
	content, _ := res["content"].(string)
	if !strings.Contains(content, "    2  second line") || !strings.Contains(content, "line truncated at") {
		t.Errorf("read_file lost the line after a cut one: %.120s ... %s", content, content[max(0, len(content)-300):])
	}
	got := asJSON(grepRepoTool(repo, map[string]any{"pattern": "needle"}, Tree{}))
	if !strings.Contains(got, "needle short") || !strings.Contains(got, "line truncated") {
		t.Errorf("grep_repo lost the match after a cut line: %.300s", got)
	}
}

// TestNormalizeWSMatchesFields pins the allocation-light normalizer to the
// strings.Fields definition it replaced, including non-ASCII whitespace and
// invalid UTF-8, and the streaming form to the whole-string form.
func TestNormalizeWSMatchesFields(t *testing.T) {
	alphabet := []string{"a", "é", " ", "\t", "\n", "\r", "\v", "\f", " ", "\u0085", " ", "　", "\xff", "\xe2\x80", "x", "\x00"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for j := rng.Intn(40); j > 0; j-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		s := b.String()
		want := strings.Join(strings.Fields(s), " ")
		if got := normalizeWS(s); got != want {
			t.Fatalf("normalizeWS(%q) = %q, want %q", s, got, want)
		}
		// Stream the same input in random chunks, splitting multi-byte runes.
		var n wsNormalizer
		var out strings.Builder
		for rest := s; ; {
			k := min(len(rest), rng.Intn(4))
			n.write(&out, rest[:k], false)
			rest = rest[k:]
			if rest == "" {
				n.write(&out, "", true)
				break
			}
		}
		if out.String() != want {
			t.Fatalf("streamed normalize(%q) = %q, want %q", s, out.String(), want)
		}
	}
}

// referenceDiffSides is normalizedDiffSides as it was written before it
// streamed: raw sides first, then strings.Fields.
func referenceDiffSides(s string) string {
	var oldSide, newSide strings.Builder
	sentinel := func(b *strings.Builder) {
		if b.Len() > 0 {
			b.WriteString("\n\x00\n")
		}
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	inHunk := false
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			sentinel(&oldSide)
			sentinel(&newSide)
			inHunk = true
			continue
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
			continue
		}
		if !inHunk || l == "" {
			continue
		}
		switch l[0] {
		case ' ':
			oldSide.WriteString(l[1:] + "\n")
			newSide.WriteString(l[1:] + "\n")
		case '-':
			oldSide.WriteString(l[1:] + "\n")
		case '+':
			newSide.WriteString(l[1:] + "\n")
		}
	}
	o, n := norm(oldSide.String()), norm(newSide.String())
	switch {
	case o == "":
		return n
	case n == "":
		return o
	default:
		return o + " \x00 " + n
	}
}

func TestNormalizedDiffSidesMatchesReference(t *testing.T) {
	lines := []string{"@@ -1 +1 @@", "diff --git a/x b/x", " ctx  line", "-old\tline", "+new line", "+", "-", " ", "+\xe2", "-\x80 tail", "\\ No newline at end of file", "", "+   ", "--- a/x", "+++ b/x"}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 3000; i++ {
		var parts []string
		for j := rng.Intn(12); j > 0; j-- {
			parts = append(parts, lines[rng.Intn(len(lines))])
		}
		s := strings.Join(parts, "\n")
		if got, want := normalizedDiffSides(s), referenceDiffSides(s); got != want {
			t.Fatalf("normalizedDiffSides(%q) = %q, want %q", s, got, want)
		}
	}
}

// TestGrounderBoundsHugeFiles: a quote from deep inside a file far larger
// than the grounding cache still grounds (read_file can page to it), and
// grounding a huge diff does not cost a multiple of its size in word slices.
func TestGrounderBoundsHugeFiles(t *testing.T) {
	repo := initRepo(t)
	body := strings.Repeat("filler words here\n", (groundingCacheBytes/18)*2) + "func deepNeedle() {}\n"
	writeFile(t, repo, "huge.txt", body)
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "huge")
	pin, err := snapshot.PinWorktree(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range []Tree{{}, PinnedWorktree(pin.Tree)} {
		g := NewQuoteGrounder(repo, tree, "", "")
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		ok := g("huge.txt", "func   deepNeedle()\n{}")
		runtime.ReadMemStats(&after)
		if !ok {
			t.Errorf("pinned=%v: a quote at the end of a huge file must ground", tree.Pinned())
		}
		if g("huge.txt", "func absentNeedle() {}") {
			t.Errorf("pinned=%v: an absent quote grounded", tree.Pinned())
		}
		// Streaming converts each 64 KiB read once (about 1x the file in
		// short-lived chunks); reading the file whole and normalizing it
		// costs several times its size, and held all of it at once.
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(len(body))*3/2 {
			t.Errorf("pinned=%v: grounding allocated %d bytes for a %d-byte file", tree.Pinned(), alloc, len(body))
		}
	}

	var diff strings.Builder
	diff.WriteString("diff --git a/data.txt b/data.txt\nnew file mode 100644\n--- /dev/null\n+++ b/data.txt\n@@ -0,0 +1,1000000 @@\n")
	for i := 0; i < 1_000_000; i++ {
		diff.WriteString("+a b c d e f g h\n")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := NewQuoteGrounder(repo, Tree{}, diff.String(), "")
	runtime.ReadMemStats(&after)
	// strings.Fields allocated a 16-byte header per word: 8 words per
	// 17-byte line made it about 8x the diff before the join, on top of the
	// raw copies. Normalizing into presized builders costs about 2x.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 3*uint64(diff.Len()) {
		t.Errorf("grounder construction allocated %d bytes for a %d-byte diff", alloc, diff.Len())
	}
	if !g("data.txt", "h a b c") {
		t.Error("diff content must still ground")
	}
}

// TestGitDiffReadersIgnoreUserDiffDrivers: git_diff and the survey range
// grounder are patch-producing git calls too. A repository attribute naming
// the user's textconv driver must not run it, and color or prefix settings
// must not change the text they parse.
func TestGitDiffReadersIgnoreUserDiffDrivers(t *testing.T) {
	repo, rangeSpec := setupBranchRepo(t, "f.go", "package p\n// original\n", "package p\n// modified\n")
	writeFile(t, repo, ".gitattributes", "*.go diff=pdf\n")
	gitAdd(t, repo, ".")
	gitCommit(t, repo, "route .go to the pdf driver")
	rangeSpec = strings.SplitN(rangeSpec, "..", 2)[0] + ".." + gitOID(t, repo, "HEAD")

	dir := t.TempDir()
	ran := filepath.Join(dir, "driver-ran")
	script := filepath.Join(dir, "driver.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+ran+"'\necho CONVERTED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	body := "[diff \"pdf\"]\n\ttextconv = " + script + "\n[diff]\n\texternal = " + script + "\n\tnoprefix = true\n[color]\n\tui = always\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	res, err := GitDiffTool(repo, rangeSpec, []string{"f.go"}).Run(t.Context(), map[string]any{"path": "f.go"})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := res.(map[string]any)["content"].(string)
	if strings.Contains(content, "CONVERTED") || strings.Contains(content, "\x1b") || !strings.Contains(content, "+++ b/f.go") || !strings.Contains(content, "+// modified") {
		t.Errorf("git_diff did not return the plain diff: %q", content)
	}
	if !NewQuoteGrounder(repo, Tree{}, "", rangeSpec)("f.go", "// original") {
		t.Error("the range grounder must parse the plain old side")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("a user diff driver ran on reviewed content")
	}
}
