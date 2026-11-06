package snapshot_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/snapshot"
)

func treeNames(t *testing.T, dir, tree string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, l := range strings.Split(git(t, dir, "ls-tree", "-r", "--name-only", tree), "\n") {
		if l != "" {
			names[l] = true
		}
	}
	return names
}

// TestPinWorktreeMatchesCapture: the pinned corpus and the recorded snapshot
// must be the same tree, and Capture must reuse a pin rather than re-pin.
func TestPinWorktreeMatchesCapture(t *testing.T) {
	src := messyRepo(t)
	pin, err := snapshot.PinWorktree(src, nil)
	if err != nil {
		t.Fatalf("PinWorktree: %v", err)
	}
	names := treeNames(t, src, pin.Tree)
	if names["ignored-secret.txt"] || names["deploy.pem"] {
		t.Errorf("pin captured an ignored or denylisted file: %v", names)
	}
	if !names["untracked.go"] || !names["unstaged.go"] {
		t.Errorf("pin dropped reviewable content: %v", names)
	}
	if len(pin.Excluded) != 1 || pin.Excluded[0] != "deploy.pem" {
		t.Errorf("Excluded = %v, want [deploy.pem]", pin.Excluded)
	}

	snap, err := snapshot.Capture(src, snapshot.Options{RunID: "run-pinned", HeadOID: headOID(t, src), Worktree: pin})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if snap.WorktreeTree != pin.Tree || len(snap.Excluded) != 1 {
		t.Errorf("Capture recorded %s %v, want the pin %s %v", snap.WorktreeTree, snap.Excluded, pin.Tree, pin.Excluded)
	}
	fresh, err := snapshot.PinWorktree(src, nil)
	if err != nil || fresh.Tree != pin.Tree {
		t.Errorf("an unchanged tree must pin to the same OID: %s vs %s (%v)", fresh.Tree, pin.Tree, err)
	}
}

// TestPinWorktreeWithoutHead: a repository with no commits cannot be recorded
// as a replayable snapshot, but its review must still be pinned.
func TestPinWorktreeWithoutHead(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, ".gitignore", ".env\n")
	write(t, dir, "new.go", "package a\n")
	write(t, dir, ".env", "TOKEN=ignored\n")
	write(t, dir, ".env.local", "TOKEN=denylisted\n")

	pin, err := snapshot.PinWorktree(dir, nil)
	if err != nil {
		t.Fatalf("PinWorktree without HEAD: %v", err)
	}
	names := treeNames(t, dir, pin.Tree)
	if !names["new.go"] || names[".env"] || names[".env.local"] {
		t.Errorf("unexpected pinned tree %v", names)
	}
	if out := git(t, dir, "for-each-ref", snapshot.RefPrefix); out != "" {
		t.Errorf("pinning must not record a ref: %s", out)
	}
}

// TestPinWorktreeUnmergedIndex: Capture degrades on a conflicted index, but
// pinning resolves the conflict in a private index copy and never touches the
// live one.
func TestPinWorktreeUnmergedIndex(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	write(t, dir, "f.txt", "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	git(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "f.txt", "other\n")
	git(t, dir, "commit", "-q", "-am", "other")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "f.txt", "main\n")
	git(t, dir, "commit", "-q", "-am", "main")
	_ = exec.Command("git", "-C", dir, "merge", "other").Run()

	pin, err := snapshot.PinWorktree(dir, nil)
	if err != nil {
		t.Fatalf("PinWorktree on an unmerged index: %v", err)
	}
	blob := git(t, dir, "cat-file", "blob", pin.Tree+":f.txt")
	if !strings.Contains(blob, "<<<<<<<") {
		t.Errorf("pinned f.txt should hold the conflicted worktree content, got %q", blob)
	}
	if unmerged := git(t, dir, "ls-files", "-u"); unmerged == "" {
		t.Error("pinning must not resolve the conflict in the live index")
	}
}

func TestDenylistCoversCredentialFiles(t *testing.T) {
	for _, name := range []string{
		".env.local", "config/.env", "id_ed25519", "id_rsa", "server.pem", "tls.key",
		".npmrc", ".pypirc", ".netrc", ".git-credentials", "credentials.json",
		"sub/credentials.json", "creds.txt", "cert.p12", ".ENV", "Config/.Env.Local", "ID_ED25519", "Credentials.JSON",
		"id_ed25519_github", "id_rsa_work", "ID_ECDSA_deploy", "id_ecdsa_sk", "id_ed25519_sk.pub", "keys/id_xmss_old", "id_dsa.bak",
	} {
		if !snapshot.Denylisted(name) {
			t.Errorf("%s should be denylisted", name)
		}
	}
	for _, name := range []string{"main.go", "credentials.go", "npmrc.go", "README.md", "id_map.go", "internal/id_test.go", "identity.go"} {
		if snapshot.Denylisted(name) {
			t.Errorf("%s should not be denylisted", name)
		}
	}
}
