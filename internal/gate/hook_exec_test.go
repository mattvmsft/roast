package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the embedded hook through a REAL `git commit` in a
// throwaway repo, with a stub standing in for the roast binary (ROAST_BIN) so
// nothing ever calls a model. What they prove is the part no unit test can:
// that git's hook environment — above all GIT_INDEX_FILE — reaches the review
// unmodified, and that every failure mode lets the commit through.

// stubScript records exactly what the commit's index looks like from inside
// the hook, then behaves as the test dictates.
const stubScript = `#!/bin/sh
{
  echo "NAMES: $(git -C "$(pwd)" diff --cached --name-only HEAD | tr '\n' ' ')"
  echo "DIFF-START"
  git -C "$(pwd)" diff --cached HEAD
  echo "DIFF-END"
} >> "$STUB_LOG"
if [ -n "${STUB_SLEEP:-}" ]; then sleep "$STUB_SLEEP"; fi
if [ -n "${STUB_STDOUT:-}" ]; then printf '%s\n' "$STUB_STDOUT"; fi
if [ -n "${STUB_STDERR:-}" ]; then printf '%s\n' "$STUB_STDERR" >&2; fi
exit "${STUB_EXIT:-0}"
`

type stub struct {
	bin string
	log string
}

func newStub(t *testing.T) stub {
	t.Helper()
	dir := t.TempDir()
	s := stub{bin: filepath.Join(dir, "roast-stub"), log: filepath.Join(dir, "scope.log")}
	writeHook(t, s.bin, stubScript, 0o755)
	return s
}

// ran reports the stub's recorded observations ("" when it never ran).
func (s stub) ran(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gitCommit runs a real commit with extra env, returning the combined output
// and whether it succeeded.
func gitCommit(t *testing.T, repo string, extraEnv []string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo, "commit"}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func writeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seededRepo returns a repo with a.txt/b.txt committed, both modified in the
// worktree, and a.txt staged — the dirty shared index the old PreToolUse gate
// could not review correctly.
func seededRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a-committed\n")
	writeFile(t, repo, "b.txt", "b-committed\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "seed")

	writeFile(t, repo, "a.txt", "a-STAGED-NOT-COMMITTED\n")
	writeFile(t, repo, "b.txt", "b-IN-THIS-COMMIT\n")
	git(t, repo, "add", "a.txt")
	// These execution tests exercise an explicitly enrolled repository.
	if err := RecordDecision(repo, DecisionEnabled); err != nil {
		t.Fatal(err)
	}
	return repo
}

func stubEnv(s stub, extra ...string) []string {
	return append([]string{
		"ROAST_BIN=" + s.bin,
		"STUB_LOG=" + s.log,
	}, extra...)
}

// TestGateReviewsOnlyThePathScopedCommitsContent is the whole point of the
// native hook. The tree has a staged a.txt that is NOT part of this commit;
// the review must see b.txt's content and nothing else. The retired
// PreToolUse mechanism reviewed the whole shared index here — and reviewed it
// before `git commit <path>` had even staged anything.
func TestGateReviewsOnlyThePathScopedCommitsContent(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit failed:\n%s", out)
	}

	seen := s.ran(t)
	if seen == "" {
		t.Fatal("the hook never ran the review")
	}
	names := ""
	for _, line := range strings.Split(seen, "\n") {
		if strings.HasPrefix(line, "NAMES: ") {
			names = strings.TrimSpace(strings.TrimPrefix(line, "NAMES: "))
		}
	}
	if names != "b.txt" {
		t.Errorf("review scope = %q, want exactly \"b.txt\"\n%s", names, seen)
	}
	if !strings.Contains(seen, "b-IN-THIS-COMMIT") {
		t.Errorf("review did not see the committed content:\n%s", seen)
	}
	if strings.Contains(seen, "a-STAGED-NOT-COMMITTED") {
		t.Errorf("review saw staged content that is NOT part of this commit:\n%s", seen)
	}

	// And the commit really was path-scoped: a.txt's staged change is still
	// staged afterwards.
	if staged := git(t, repo, "diff", "--cached", "--name-only"); staged != "a.txt" {
		t.Errorf("post-commit staged set = %q, want a.txt", staged)
	}
}

func TestGateBlocksOnFindingsAndPrintsThemToStderr(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-abc123 HIGH — nil deref in handler",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")

	if ok {
		t.Fatalf("commit succeeded despite a gating finding:\n%s", out)
	}
	if !strings.Contains(out, "R-abc123") {
		t.Errorf("findings markdown missing from the failed commit output:\n%s", out)
	}
	if !strings.Contains(out, "ROAST_SKIP=1") {
		t.Errorf("no bypass instructions in the denial:\n%s", out)
	}
	// Nothing was committed.
	if head := git(t, repo, "log", "--oneline"); strings.Count(head, "\n") != 0 {
		t.Errorf("a commit landed anyway: %q", head)
	}
}

func TestGateSkipBypassesEntirely(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s, "ROAST_SKIP=1", "STUB_EXIT=1"), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("ROAST_SKIP=1 did not bypass the gate:\n%s", out)
	}
	if seen := s.ran(t); seen != "" {
		t.Errorf("roast ran despite ROAST_SKIP=1:\n%s", seen)
	}
}

// TestGateTreatsEmptyStdoutAsInfrastructureFailure: no JSON result means the
// review never ran. That must never read as clean — but it must also not
// wedge the commit.
func TestGateTreatsEmptyStdoutAsInfrastructureFailure(t *testing.T) {
	for _, exit := range []string{"STUB_EXIT=0", "STUB_EXIT=3"} {
		t.Run(exit, func(t *testing.T) {
			repo := seededRepo(t)
			if _, _, err := Install(repo); err != nil {
				t.Fatal(err)
			}
			s := newStub(t)
			out, ok := gitCommit(t, repo, stubEnv(s, exit), "b.txt", "-m", "scoped")
			if !ok {
				t.Fatalf("commit blocked by an infrastructure failure:\n%s", out)
			}
			if !strings.Contains(out, "WITHOUT review") {
				t.Errorf("empty stdout was not reported as an unreviewed commit:\n%s", out)
			}
		})
	}
}

func TestGateAllowsCommitOnOperationalFailure(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[]}`,
		"STUB_STDERR=inference endpoint unreachable",
		"STUB_EXIT=4",
	), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit blocked by a partial-failure exit:\n%s", out)
	}
	if !strings.Contains(out, "exit 4") || !strings.Contains(out, "WITHOUT review") {
		t.Errorf("operational failure not surfaced:\n%s", out)
	}
}

func TestGateAllowsCommitWhenRoastIsMissing(t *testing.T) {
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	out, ok := gitCommit(t, repo, []string{"ROAST_BIN=/nonexistent/roast"}, "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("a missing roast binary blocked the commit:\n%s", out)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("missing binary not reported:\n%s", out)
	}
}

func TestGateTimesOutWithoutBlocking(t *testing.T) {
	if testing.Short() {
		t.Skip("timeout test sleeps")
	}
	repo := seededRepo(t)
	if _, _, err := Install(repo); err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s,
		"STUB_SLEEP=10",
		`STUB_STDOUT={"findings":[]}`,
		"ROAST_GATE_TIMEOUT=1",
	), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("a timed-out review blocked the commit:\n%s", out)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("timeout not reported:\n%s", out)
	}
}

// TestGateRunsAlongsideAForeignHook: on a passing review both the gate and
// the foreign hook run; on a blocking one the gate fails the commit. The
// block sits right after the shebang, so a denial short-circuits the foreign
// hook — the documented ordering tradeoff in appendBlock.
func TestGateRunsAlongsideAForeignHook(t *testing.T) {
	repo := seededRepo(t)
	marker := filepath.Join(t.TempDir(), "foreign-ran")
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"),
		"#!/bin/sh\ntouch \""+marker+"\"\n", 0o755)
	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-deadbe HIGH — boom",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")
	if ok {
		t.Fatalf("appended gate did not fail the commit:\n%s", out)
	}
	if !strings.Contains(out, "R-deadbe") {
		t.Errorf("appended gate did not surface findings:\n%s", out)
	}

	// Passing review: the commit lands and the foreign hook ran too.
	out, ok = gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("commit failed on a clean review:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the foreign hook did not run: %v", err)
	}
}

// TestGateRunsInAForeignHookThatExitsEarly is the regression for roast
// finding R-5351f8: a hook ending in `exit 0` (a perfectly ordinary shape)
// would never reach a block appended at EOF, so the gate would report itself
// installed and never run once.
func TestGateRunsInAForeignHookThatExitsEarly(t *testing.T) {
	repo := seededRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"),
		"#!/bin/sh\nexit 0\n", 0o755)
	if action, _, err := Install(repo); err != nil || action != ActionAppended {
		t.Fatalf("install = %q (%v)", action, err)
	}
	s := newStub(t)

	out, ok := gitCommit(t, repo, stubEnv(s,
		`STUB_STDOUT={"findings":[{"severity":"HIGH"}]}`,
		"STUB_STDERR=## R-c0ffee HIGH — boom",
		"STUB_EXIT=1",
	), "b.txt", "-m", "scoped")
	if ok {
		t.Fatalf("gate never ran in a hook that exits early:\n%s", out)
	}
	if s.ran(t) == "" {
		t.Fatal("the review never ran")
	}
	if !strings.Contains(out, "R-c0ffee") {
		t.Errorf("findings not surfaced:\n%s", out)
	}
}

// TestGateDoesNotRunWhenHookIsNotExecutable: git skips a non-executable hook,
// so Inspect must not call that an active gate (roast finding R-1375c9).
func TestGateDoesNotRunWhenHookIsNotExecutable(t *testing.T) {
	repo := seededRepo(t)
	writeHook(t, filepath.Join(repo, ".git", "hooks", "pre-commit"), "#!/bin/sh\nmake lint\n", 0o755)
	if _, st, err := Install(repo); err != nil {
		t.Fatal(err)
	} else if err := os.Chmod(st.HookPath, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Appended {
		t.Fatalf("expected an appended hook: %+v", st)
	}
	if st.Active() {
		t.Error("a non-executable hook reports as an active gate")
	}
	if !strings.Contains(st.Summary(), "NOT RUNNING") {
		t.Errorf("summary hides the dead gate: %q", st.Summary())
	}

	s := newStub(t)
	out, ok := gitCommit(t, repo, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "scoped")
	if !ok {
		t.Fatalf("git ran a non-executable hook?\n%s", out)
	}
	if s.ran(t) != "" {
		t.Error("the review ran from a hook git should have skipped")
	}
}
