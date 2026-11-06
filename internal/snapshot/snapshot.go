// Package snapshot captures a repository's exact working state — index and
// worktree, including uncommitted and untracked content — as objects in the
// repo's own store, anchored by a ref under refs/roast/snapshots/<run-id>.
//
// The mechanic: write-tree over a COPY of
// .git/index yields the index tree; a second copy plus `git add -A` yields the
// worktree tree; one composite tree holds both under `index/` and `worktree/`;
// one commit anchors the composite with HEAD (and, for branch scope, the base)
// as parents so a later rebase or branch delete cannot strand the range. The
// live index is never touched.
//
// Restore materializes a snapshot into a throwaway linked worktree: read-tree
// the worktree tree, checkout-index to disk, then read-tree the index tree —
// files on disk stay as the worktree tree left them while the index reverts to
// what was actually staged, so paths on disk but absent from the index reappear
// as untracked, exactly their original status.
//
// Two accepted losses, both documented in the plan: empty directories (git
// cannot represent them) and .gitignore'd files (`git add -A` skips them,
// matching roast's own scope rules). A third is deliberate: untracked,
// non-ignored files whose names match the secret denylist are skipped and
// recorded in Snapshot.Excluded.
//
// Everything shells out to git — no go-git dependency.
package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RefPrefix is roast's private ref namespace. Prune only ever deletes refs
// under this prefix.
const RefPrefix = "refs/roast/snapshots/"

// DefaultDenylist are the untracked-non-ignored path shapes never captured
// into the worktree tree, never folded into a review diff, and never served
// from the live working tree. `git add -A` already
// respects .gitignore, which is where secrets normally live; this denylist
// covers the one residual class — "dropped creds.json and forgot to gitignore
// it". Patterns are matched against the basename AND the full repo-relative
// path with filepath.Match semantics (`*` does not cross `/`).
//
// SSH keys are matched by key-type prefix so suffixed names (id_ed25519_github,
// id_rsa_work) and .pub files are covered too; a bare `id_*` would also drop
// ordinary sources such as id_map.go.
var DefaultDenylist = []string{
	".env*", "*.pem", "*_rsa", "*.key", "creds*",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", "id_xmss*",
	"credentials.json", ".npmrc", ".pypirc", ".netrc", "_netrc", ".pgpass",
	".git-credentials", "*.p12", "*.pfx",
}

// Denylisted reports whether path matches DefaultDenylist.
func Denylisted(path string) bool {
	return matchesAny(path, DefaultDenylist)
}

// DenylistedUntracked returns the untracked, non-ignored paths that match
// DefaultDenylist, sorted.
func DenylistedUntracked(repo string) ([]string, error) {
	return denylisted(repo, nil)
}

// DefaultRefRetention is how long snapshot refs live by default. Tighter than
// the artifact retention because this one lives in the user's own repo.
const DefaultRefRetention = 30 * 24 * time.Hour

// Options configures one capture.
type Options struct {
	// RunID is the record run id; it names the ref.
	RunID string
	// HeadOID is the commit at HEAD, recorded as the snapshot commit's first
	// parent so it stays reachable. Empty in a repo with no commits.
	HeadOID string
	// BaseOID is the branch-scope base commit, recorded as a second parent so
	// a rebase or branch delete cannot strand the reviewed range. Empty when
	// there is no base or it equals HeadOID.
	BaseOID string
	// Denylist overrides DefaultDenylist. A non-nil empty slice disables
	// denylist filtering entirely.
	Denylist []string
	// IndexTree, when set, is a tree object already pinned from the live
	// index (via context.PinIndexTree, run by the caller before the review
	// itself started reading from it). Capture reuses this OID verbatim for
	// the index half instead of re-copying the index and write-treeing it
	// again — two independent write-trees a few milliseconds apart could
	// disagree if something touched the index in between, which defeats the
	// point of pinning: recording and review must provably see the same
	// tree. Empty falls back to Capture's own copy+write-tree, as before.
	IndexTree string
	// Worktree, when its Tree is set, is the corpus PinWorktree already
	// pinned for this review. Capture records it verbatim, for the same
	// reason as IndexTree: the recorded tree must be the reviewed tree.
	Worktree Pin
}

// Pin is a working-tree state written as an immutable Git tree object.
type Pin struct {
	Tree string
	// Excluded lists untracked-non-ignored paths skipped by the denylist.
	Excluded []string
}

// Snapshot is a captured (or loaded) snapshot's object identity.
type Snapshot struct {
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	IndexTree    string `json:"index_tree"`
	WorktreeTree string `json:"worktree_tree"`
	Head         string `json:"head,omitempty"`
	Base         string `json:"base,omitempty"`
	// Excluded lists untracked-non-ignored paths skipped by the secret
	// denylist. Non-empty means the worktree tree is NOT a faithful copy of
	// the working tree, which is exactly what a replay needs to know.
	Excluded []string `json:"excluded_paths,omitempty"`
}

// Enabled reports whether snapshot capture is on. Default-on; ROAST_SNAPSHOT=0
// (or "false"/"no") disables it, and a run then records no ref or commit. The
// review corpus is still pinned with PinWorktree, whose tree objects stay
// unreferenced and are left for git gc.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ROAST_SNAPSHOT"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// Capture snapshots repo's current index and worktree. The live index is never
// written. Returns an error (never a partial snapshot) when any git step fails
// — an unmerged index makes write-tree refuse, a read-only .git makes the ref
// update fail — and callers degrade to `snapshot: failed` rather than aborting
// the review.
func Capture(repo string, opts Options) (Snapshot, error) {
	if opts.RunID == "" {
		return Snapshot{}, errors.New("snapshot: empty run id")
	}
	// An unborn HEAD cannot be restored. Restore detaches a linked worktree at
	// the original HEAD; with none, the only commit-ish available is the
	// snapshot commit itself, whose tree is the synthetic index/+worktree/
	// composite — so the restored worktree would diff against the wrong
	// baseline and the digest check would refuse the replay anyway. Refusing
	// here records captured=false with a reason instead of advertising a
	// snapshot that cannot be replayed.
	if opts.HeadOID == "" {
		return Snapshot{}, errors.New("repository has no commits; an unborn HEAD cannot be restored, so no replayable snapshot is possible")
	}
	gitDir, err := out(repo, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve git dir: %w", err)
	}

	tmp, err := os.MkdirTemp("", "roast-snapshot-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	indexTree := opts.IndexTree
	if indexTree == "" {
		idxCopy := filepath.Join(tmp, "index")
		if err := copyIndex(liveIndex(repo, gitDir), idxCopy); err != nil {
			return Snapshot{}, err
		}
		indexTree, err = out(repo, indexEnv(idxCopy), "write-tree")
		if err != nil {
			return Snapshot{}, fmt.Errorf("write-tree (index): %w", err)
		}
	}

	pin := opts.Worktree
	if pin.Tree == "" {
		pin, err = pinWorktree(repo, gitDir, tmp, opts.Denylist)
		if err != nil {
			return Snapshot{}, err
		}
	}
	worktreeTree, excluded := pin.Tree, pin.Excluded

	composite, err := mktree(repo, indexTree, worktreeTree)
	if err != nil {
		return Snapshot{}, err
	}

	commitArgs := []string{"commit-tree", composite}
	if opts.HeadOID != "" {
		commitArgs = append(commitArgs, "-p", opts.HeadOID)
	}
	if opts.BaseOID != "" && opts.BaseOID != opts.HeadOID {
		commitArgs = append(commitArgs, "-p", opts.BaseOID)
	}
	commitArgs = append(commitArgs, "-m", "roast run "+opts.RunID)
	commit, err := out(repo, identityEnv(), commitArgs...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("commit-tree: %w", err)
	}

	ref := RefPrefix + opts.RunID
	if _, err := out(repo, nil, "update-ref", ref, commit); err != nil {
		return Snapshot{}, fmt.Errorf("update-ref %s: %w", ref, err)
	}

	return Snapshot{
		Ref: ref, Commit: commit,
		IndexTree: indexTree, WorktreeTree: worktreeTree,
		Head: opts.HeadOID, Base: opts.BaseOID,
		Excluded: excluded,
	}, nil
}

// PinWorktree writes repo's working tree, as a review sees it, into an
// immutable tree object: tracked files plus untracked, non-ignored files,
// minus untracked paths matching denylist (nil means DefaultDenylist). It is
// the worktree half of Capture, kept separate so a review is pinned even when
// no replayable snapshot can be recorded: it needs no HEAD, touches no ref
// or commit, and starts from a private copy of the index, so an unmerged
// live index is resolved by `git add -A` in the copy rather than refused.
// The live index is never written. The objects it writes are unreferenced
// until Capture anchors them, so git gc can collect them.
func PinWorktree(repo string, denylist []string) (Pin, error) {
	gitDir, err := out(repo, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Pin{}, fmt.Errorf("resolve git dir: %w", err)
	}
	tmp, err := os.MkdirTemp("", "roast-pin-")
	if err != nil {
		return Pin{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	return pinWorktree(repo, gitDir, tmp, denylist)
}

func pinWorktree(repo, gitDir, tmp string, denylist []string) (Pin, error) {
	wtCopy := filepath.Join(tmp, "worktree")
	if err := copyIndex(liveIndex(repo, gitDir), wtCopy); err != nil {
		return Pin{}, err
	}

	excluded, err := denylisted(repo, denylist)
	if err != nil {
		return Pin{}, err
	}

	// `.` is the whole repo because every git invocation here runs with
	// -C <repo toplevel>. A positive pathspec is required: an exclude-only
	// pathspec matches nothing and makes `git add` error.
	addArgs := []string{"add", "-A", "--", "."}
	for _, p := range excluded {
		// :(literal) so a pathspec metacharacter in a filename cannot widen
		// the exclusion to unrelated files — the same guard internal/scope
		// applies to linguist excludes.
		addArgs = append(addArgs, ":(exclude,literal)"+p)
	}
	if _, err := out(repo, indexEnv(wtCopy), addArgs...); err != nil {
		return Pin{}, fmt.Errorf("git add -A (snapshot index): %w", err)
	}
	tree, err := out(repo, indexEnv(wtCopy), "write-tree")
	if err != nil {
		return Pin{}, fmt.Errorf("write-tree (worktree): %w", err)
	}
	return Pin{Tree: tree, Excluded: excluded}, nil
}

// liveIndex returns the index file this review reads. GIT_INDEX_FILE wins
// when set. Under the native pre-commit gate git points it at the temporary
// index holding exactly the commit's content, which is what `--staged`
// reviews — so capturing gitDir/index instead would snapshot a DIFFERENT
// state than the one reviewed, and every path-scoped gate run would fail its
// own replay digest check. The check would fail loudly (it misleads no one),
// but the run would be unreplayable for no reason.
func liveIndex(repo, gitDir string) string {
	live := os.Getenv("GIT_INDEX_FILE")
	if live == "" {
		return filepath.Join(gitDir, "index")
	}
	if !filepath.IsAbs(live) {
		// git may hand it over relative to the repo root.
		return filepath.Join(repo, live)
	}
	return live
}

// Load reads a captured snapshot's identity back out of the ref. The snapshot
// is self-describing: the composite tree names both sub-trees and the commit
// parents name HEAD and (when present) the base.
func Load(repo, runID string) (Snapshot, error) {
	ref := RefPrefix + runID
	commit, err := out(repo, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || commit == "" {
		return Snapshot{}, fmt.Errorf("no snapshot ref %s in %s (run predates snapshots, was pruned, or lives in another clone)", ref, repo)
	}
	indexTree, err := out(repo, nil, "rev-parse", "--verify", "--quiet", ref+":index")
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s has no index/ tree: %w", ref, err)
	}
	worktreeTree, err := out(repo, nil, "rev-parse", "--verify", "--quiet", ref+":worktree")
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s has no worktree/ tree: %w", ref, err)
	}
	// Parents are best-effort: a snapshot of a repo with no commits has none.
	head, _ := out(repo, nil, "rev-parse", "--verify", "--quiet", ref+"^1")
	base, _ := out(repo, nil, "rev-parse", "--verify", "--quiet", ref+"^2")
	return Snapshot{
		Ref: ref, Commit: commit,
		IndexTree: indexTree, WorktreeTree: worktreeTree,
		Head: head, Base: base,
	}, nil
}

// Delete removes the exact snapshot ref for runID. It is used when a review
// captured repository state but cannot initialize the durable run record; in
// that case retaining the private ref would leak an unlisted orphan snapshot.
func Delete(repo, runID string) error {
	if runID == "" {
		return errors.New("delete snapshot: empty run id")
	}
	ref := RefPrefix + runID
	if _, err := out(repo, nil, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	return nil
}

// Restore materializes a snapshot into dir as a detached linked worktree of
// repo. dir must not exist. The returned Snapshot is the loaded identity.
//
// The final read-tree is the trick: disk keeps the worktree tree's contents
// while the index reverts to what was actually staged, so `git status`,
// `git diff HEAD` and `git diff --cached HEAD` all reproduce the original.
func Restore(repo, runID, dir string) (Snapshot, error) {
	snap, err := Load(repo, runID)
	if err != nil {
		return Snapshot{}, err
	}
	// Detach at the original HEAD so the restored worktree's HEAD means what
	// it meant during the run. Falling back to the snapshot commit would be
	// worse than failing: its tree is the synthetic index/+worktree/ composite,
	// so every `git diff HEAD` in the restored worktree would measure against a
	// baseline that never existed. Capture refuses unborn HEADs for the same
	// reason, so this only fires on a hand-made ref.
	if snap.Head == "" {
		return Snapshot{}, fmt.Errorf("snapshot %s records no HEAD commit; it cannot be restored to a meaningful baseline", snap.Ref)
	}
	if _, err := out(repo, nil, "worktree", "add", "--detach", "--no-checkout", dir, snap.Head); err != nil {
		return Snapshot{}, fmt.Errorf("git worktree add: %w", err)
	}
	// From here on a failure leaves a half-built worktree; tear it down so the
	// caller isn't left with administrative droppings in the source repo.
	fail := func(err error) (Snapshot, error) {
		_ = RemoveWorktree(repo, dir)
		return Snapshot{}, err
	}
	if _, err := out(dir, nil, "read-tree", snap.WorktreeTree); err != nil {
		return fail(fmt.Errorf("read-tree (worktree): %w", err))
	}
	if _, err := out(dir, nil, "checkout-index", "-a", "-f"); err != nil {
		return fail(fmt.Errorf("checkout-index: %w", err))
	}
	if _, err := out(dir, nil, "read-tree", snap.IndexTree); err != nil {
		return fail(fmt.Errorf("read-tree (index): %w", err))
	}
	return snap, nil
}

// RemoveWorktree tears down a worktree created by Restore, including the
// administrative entry in the source repo. Best-effort: a failed removal is
// reported but leaves nothing that `git worktree prune` cannot clean up.
func RemoveWorktree(repo, dir string) error {
	if _, err := out(repo, nil, "worktree", "remove", "--force", dir); err != nil {
		// The worktree may never have been registered; fall back to removing
		// the directory and pruning the stale administrative entry.
		_ = os.RemoveAll(dir)
		if _, perr := out(repo, nil, "worktree", "prune"); perr != nil {
			return fmt.Errorf("worktree remove %s: %w", dir, err)
		}
	}
	return nil
}

// Pruned is one ref considered by Prune.
type Pruned struct {
	Ref     string
	RunID   string
	Age     time.Duration
	Deleted bool
}

// Prune deletes snapshot refs older than olderThan. Only refs under RefPrefix
// are ever touched. dryRun reports what would be deleted without deleting.
// Refs whose age cannot be determined are left alone.
func Prune(repo string, olderThan time.Duration, dryRun bool) ([]Pruned, error) {
	if olderThan <= 0 {
		olderThan = DefaultRefRetention
	}
	listing, err := out(repo, nil, "for-each-ref", "--format=%(refname)%09%(committerdate:unix)", RefPrefix)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref %s: %w", RefPrefix, err)
	}
	now := time.Now()
	var pruned []Pruned
	for _, line := range strings.Split(listing, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, tsRaw, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(name, RefPrefix) {
			continue
		}
		secs, convErr := strconv.ParseInt(strings.TrimSpace(tsRaw), 10, 64)
		if convErr != nil {
			continue
		}
		age := now.Sub(time.Unix(secs, 0))
		if age < olderThan {
			continue
		}
		p := Pruned{Ref: name, RunID: strings.TrimPrefix(name, RefPrefix), Age: age}
		if !dryRun {
			if _, derr := out(repo, nil, "update-ref", "-d", name); derr != nil {
				return pruned, fmt.Errorf("delete %s: %w", name, derr)
			}
			p.Deleted = true
		}
		pruned = append(pruned, p)
	}
	return pruned, nil
}

// HeadOID returns the commit at HEAD, or "" in a repo with no commits. Diff
// scope does not pin OIDs the way survey scope does, but a snapshot still
// needs HEAD as a commit parent to keep it reachable.
func HeadOID(repo string) string {
	return RevOID(repo, "HEAD")
}

// RevOID resolves rev to a commit OID, or "" when it does not resolve.
func RevOID(repo, rev string) string {
	oid, err := out(repo, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return ""
	}
	return oid
}

// TreeOID resolves rev to its tree OID, or "" when it does not resolve. The
// base tree is the third archive an off-laptop replay needs: the reviewed diff
// is base→worktree or HEAD→index, so index+worktree alone cannot rebuild it.
func TreeOID(repo, rev string) string {
	oid, err := out(repo, nil, "rev-parse", "--verify", "--quiet", rev+"^{tree}")
	if err != nil {
		return ""
	}
	return oid
}

// --- internal helpers -------------------------------------------------------

// denylisted returns the untracked, non-ignored paths matching the secret
// denylist, sorted. Tracked files are out of scope by construction: their
// content is already in the repo's history.
func denylisted(repo string, patterns []string) ([]string, error) {
	if patterns == nil {
		patterns = DefaultDenylist
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	others, err := out(repo, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("ls-files --others: %w", err)
	}
	var hits []string
	for _, p := range strings.Split(others, "\x00") {
		if p == "" {
			continue
		}
		if matchesAny(p, patterns) {
			hits = append(hits, p)
		}
	}
	sort.Strings(hits)
	return hits, nil
}

// matchesAny reports whether path matches any denylist pattern, testing both
// the basename (so `.env*` catches `config/.env.local`) and the full
// repo-relative path (so a caller can pass a path-shaped pattern).
func matchesAny(path string, patterns []string) bool {
	// Fold case: on case-insensitive filesystems (APFS, NTFS) `.ENV` opens
	// `.env`, so a case-sensitive match would let a requested spelling slip
	// past the denylist.
	path = strings.ToLower(path)
	base := filepath.Base(path)
	for _, pat := range patterns {
		pat = strings.ToLower(pat)
		if ok, err := filepath.Match(pat, base); err == nil && ok {
			return true
		}
		if ok, err := filepath.Match(pat, path); err == nil && ok {
			return true
		}
	}
	return false
}

// copyIndex copies the live index to dst. A missing index (a repo where
// nothing was ever staged) is not an error: git treats an absent
// GIT_INDEX_FILE as an empty index.
func copyIndex(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(f, in); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy index: %w", err)
	}
	return f.Close()
}

// mktree builds the two-entry composite tree holding both states.
func mktree(repo, indexTree, worktreeTree string) (string, error) {
	spec := fmt.Sprintf("040000 tree %s\tindex\n040000 tree %s\tworktree\n", indexTree, worktreeTree)
	cmd := exec.Command("git", "-C", repo, "mktree")
	cmd.Stdin = strings.NewReader(spec)
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("mktree: %w", withStderr(err))
	}
	return strings.TrimSpace(string(raw)), nil
}

// indexEnv points git at a private index copy. The live index is never
// written by this package.
func indexEnv(path string) []string {
	return []string{"GIT_INDEX_FILE=" + path}
}

// identityEnv pins the snapshot commit's author/committer. These objects are
// roast bookkeeping, not the user's history, so capture must not fail on a
// machine with no user.email configured — and must not silently attribute
// roast's own object to whoever happens to be committing.
func identityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=roast", "GIT_AUTHOR_EMAIL=roast@localhost",
		"GIT_COMMITTER_NAME=roast", "GIT_COMMITTER_EMAIL=roast@localhost",
	}
}

// out runs a git command in dir with extra environment, returning trimmed
// stdout. Extra env is appended to the process environment so a caller-set
// GIT_INDEX_FILE wins over an inherited one.
func out(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	raw, err := cmd.Output()
	if err != nil {
		return "", withStderr(err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// withStderr folds a failed command's stderr into the error, which is where
// git puts every message worth reading.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
