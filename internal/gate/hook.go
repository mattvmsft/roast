package gate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Decision is the per-repo enrollment decision, stored in the repo's own
// config (`git config --local roast.gate`) so it travels with the repo's
// .git, survives a fresh clone of the working tree it describes, and never
// re-nags. There is deliberately no global default beyond "ask once".
type Decision string

const (
	// DecisionNone means the repo has never been asked (or the recorded value
	// is unrecognizable, which we treat the same way rather than guessing).
	DecisionNone Decision = ""
	// DecisionEnabled means the user consented to the gate in this repo.
	DecisionEnabled Decision = "enabled"
	// DecisionDeclined means the user said no. Nothing re-prompts after this;
	// `roast gate enable` is the way back.
	DecisionDeclined Decision = "declined"
)

const (
	configKeyDecision = "roast.gate"
	// configKeyNotice records that we already printed the "here's how to turn
	// this on" line for a repo we could not enroll (no TTY, or a foreign hook
	// we cannot append to). Separate from the decision because it is not one:
	// the user hasn't chosen anything, we just aren't going to say it twice.
	configKeyNotice = "roast.gate-notice"
)

// Status is everything `roast gate status` and the bootstrap need to know
// about one repo. Exists/Ours/Appended/Foreign are mutually exclusive views
// of the same file: at most one of Ours, Appended, Foreign is true.
type Status struct {
	// Repo is the working-tree root the status describes.
	Repo string
	// Decision is the recorded per-repo enrollment decision.
	Decision Decision
	// HookPath is the repo's pre-commit hook path, honouring core.hooksPath.
	HookPath string
	// SidecarPath is where the gate script lives when a foreign hook calls
	// out to it.
	SidecarPath string
	// Shared reports an effective core.hooksPath override. Even a locally
	// configured path may be used by another repo, so preserve its dispatcher.
	Shared bool
	// Exists reports whether HookPath exists at all.
	Exists bool
	// Ours reports that HookPath is roast's own gate script.
	Ours bool
	// Appended reports that HookPath is someone else's script carrying our
	// appended invocation block.
	Appended bool
	// Foreign reports that HookPath exists and is neither of the above.
	Foreign bool
	// Appendable reports that a Foreign hook is a plain shell script we can
	// safely append a block to. A symlink, a binary, or a non-shell
	// interpreter is never appendable.
	Appendable bool
	// Executable reports that the hook file carries an execute bit. git
	// silently skips one that doesn't, so this gates Active.
	Executable bool
	// Stale reports that roast's part — our script, or the sidecar a foreign
	// hook calls — has drifted from the embedded HookScript. A refresh fixes
	// it. It never covers a FOREIGN file's mode, which roast does not repair.
	Stale bool
	// NoticeSent reports that the "how to enable" line was already printed
	// for this repo.
	NoticeSent bool
}

// Active requires both an executable dispatcher and this repo's consent.
func (s Status) Active() bool {
	return s.Decision == DecisionEnabled && (s.Ours || s.Appended) && s.Executable
}

// Summary is the one-line honest state for `roast gate status`.
func (s Status) Summary() string {
	switch {
	case (s.Ours || s.Appended) && !s.Executable:
		return "INSTALLED BUT NOT RUNNING — " + s.HookPath + " is not executable, so git skips it (chmod +x it, or `roast gate enable` for roast's own hook)"
	case (s.Ours || s.Appended) && s.Stale:
		return "installed dispatcher is stale — run `roast gate enable` to refresh: " + s.HookPath
	case (s.Ours || s.Appended) && s.Decision == DecisionDeclined:
		return "declined for this repo; dispatcher retained: " + s.HookPath
	case (s.Ours || s.Appended) && s.Decision != DecisionEnabled:
		return "dispatcher installed but not enrolled in this repo (`roast gate enable` enables it): " + s.HookPath
	case s.Ours:
		return "installed: " + s.HookPath
	case s.Appended:
		return "installed via an appended block in a foreign hook: " + s.HookPath
	case s.Foreign && s.Decision == DecisionDeclined:
		return "declined; a foreign pre-commit hook is present (untouched): " + s.HookPath
	case s.Foreign && s.Appendable:
		return "not installed — a foreign pre-commit hook is present (untouched); `roast gate enable` appends to it: " + s.HookPath
	case s.Foreign:
		return "not installed — a foreign pre-commit hook is present and is not an appendable shell script (untouched): " + s.HookPath
	case s.Decision == DecisionDeclined:
		return "declined for this repo (`roast gate enable` turns it on)"
	case s.Decision == DecisionEnabled:
		return "enabled for this repo but the hook is missing — run `roast gate enable` to reinstall"
	default:
		return "not enrolled (`roast gate enable` installs it)"
	}
}

// Inspect reports the gate state of a repo. repo must be a working-tree root.
// It never mutates anything.
func Inspect(repo string) (Status, error) {
	st := Status{Repo: repo}

	hookPath, err := resolveHookPath(repo, HookName)
	if err != nil {
		return st, err
	}
	sidecarPath, err := resolveHookPath(repo, SidecarName)
	if err != nil {
		return st, err
	}
	st.HookPath = hookPath
	st.SidecarPath = sidecarPath
	_, configErr := exec.Command("git", "-C", repo, "config", "--get", "core.hooksPath").Output()
	st.Shared = configErr == nil
	st.Decision = readDecision(repo)
	st.NoticeSent = gitConfigGet(repo, configKeyNotice) != ""

	info, err := os.Lstat(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, fmt.Errorf("stat %s: %w", hookPath, err)
	}
	st.Exists = true
	// git silently ignores a hook without an execute bit, so a non-executable
	// hook is not a running gate however good its contents look (roast
	// finding R-1375c9).
	st.Executable = info.Mode().Perm()&0o100 != 0

	// A symlinked hook belongs to whatever put it there (husky or a dotfiles
	// repo). Rewriting through the link would
	// edit that other thing, and replacing the link would silently unhook it,
	// so a symlink is foreign and never appendable.
	if info.Mode()&os.ModeSymlink != 0 {
		st.Foreign = true
		return st, nil
	}

	content, err := os.ReadFile(hookPath)
	if err != nil {
		return st, fmt.Errorf("read %s: %w", hookPath, err)
	}
	body := string(content)

	// Ownership is anchored, not "contains somewhere" (roast finding
	// R-911598): both markers are readable comments, and the paths keyed off
	// these flags overwrite and delete files. Ours means the file STARTS with
	// our generated header; appended means a complete, ordered begin/end pair.
	// Anything else stays foreign, which is the safe classification.
	_, _, hasBlock := appendedBlock(strings.Split(body, "\n"))
	switch {
	case strings.HasPrefix(body, hookHeader):
		st.Ours = true
		st.Stale = body != HookScript || !st.Executable
	case hasBlock:
		st.Appended = true
		// The foreign hook's own mode is deliberately NOT repaired (see
		// Install), so it feeds Active rather than Stale: staleness is
		// "roast's part has drifted", and only roast's part gets rewritten.
		st.Stale = !sidecarCurrent(sidecarPath)
	default:
		st.Foreign = true
		st.Appendable = appendable(body)
	}
	return st, nil
}

// Action names what Install or Remove actually did, so callers can report it
// honestly instead of claiming a write that never happened.
type Action string

const (
	// ActionInstalled: we wrote the repo's pre-commit hook.
	ActionInstalled Action = "installed"
	// ActionAppended: a foreign hook gained our invocation block.
	ActionAppended Action = "appended"
	// ActionRefreshed: our script was stale (or non-executable) and was rewritten.
	ActionRefreshed Action = "refreshed"
	// ActionUpToDate: nothing to do.
	ActionUpToDate Action = "up-to-date"
	// ActionManual: a foreign hook we refuse to touch — the caller prints
	// ManualLine and nothing is changed except the sidecar script, which is
	// written so the printed line works as-is.
	ActionManual Action = "manual"
	// ActionRemoved: our hook (or our appended block) is gone.
	ActionRemoved Action = "removed"
	// ActionNothing: there was nothing of ours to remove.
	ActionNothing Action = "nothing"
	// ActionPreserved: a custom hooksPath dispatcher was kept for other repos.
	ActionPreserved Action = "preserved"
)

// Install puts the gate in place for repo without ever clobbering a hook we
// did not write. It does NOT record a decision — the caller owns that, since
// the same install runs from an explicit `roast gate enable` and from a
// consented bootstrap.
func Install(repo string) (Action, Status, error) {
	st, err := Inspect(repo)
	if err != nil {
		return ActionNothing, st, err
	}

	switch {
	case st.Foreign && !st.Appendable:
		// Write the sidecar (a new file, never someone else's) so the manual
		// one-liner we print is immediately usable, but leave their hook alone.
		if err := writeSidecar(st.SidecarPath); err != nil {
			return ActionNothing, st, err
		}
		return ActionManual, st, nil

	case st.Foreign:
		if err := writeSidecar(st.SidecarPath); err != nil {
			return ActionNothing, st, err
		}
		if err := appendBlock(st.HookPath); err != nil {
			return ActionNothing, st, err
		}
		return ActionAppended, st, nil

	case st.Appended:
		if !st.Stale {
			return ActionUpToDate, st, nil
		}
		if err := writeSidecar(st.SidecarPath); err != nil {
			return ActionNothing, st, err
		}
		return ActionRefreshed, st, nil

	case st.Ours:
		if !st.Stale {
			return ActionUpToDate, st, nil
		}
		if err := writeScript(st.HookPath, HookScript); err != nil {
			return ActionNothing, st, err
		}
		return ActionRefreshed, st, nil

	default:
		if err := writeScript(st.HookPath, HookScript); err != nil {
			return ActionNothing, st, err
		}
		return ActionInstalled, st, nil
	}
}

// Remove deletes our standalone hook or strips our block from a dedicated
// foreign hook. Custom hooksPath dispatchers are preserved for other repos;
// their callers must record this repo's decline separately.
func Remove(repo string) (Action, Status, error) {
	st, err := Inspect(repo)
	if err != nil {
		return ActionNothing, st, err
	}
	if st.Shared {
		// Older dispatchers do not check consent. Refresh our recognized script
		// before disabling locally, without removing anyone else's dispatcher.
		if (st.Ours || st.Appended) && st.Stale {
			if _, _, err := Install(repo); err != nil {
				return ActionNothing, st, err
			}
		}
		if err := refreshOwnedSidecar(st.SidecarPath); err != nil {
			return ActionNothing, st, err
		}
		return ActionPreserved, st, nil
	}
	action := ActionNothing
	switch {
	case st.Ours:
		if err := os.Remove(st.HookPath); err != nil && !os.IsNotExist(err) {
			return ActionNothing, st, fmt.Errorf("remove %s: %w", st.HookPath, err)
		}
		action = ActionRemoved
	case st.Appended:
		if err := stripBlock(st.HookPath); err != nil {
			return ActionNothing, st, err
		}
		action = ActionRemoved
	}
	// Clean up the sidecar too — including in the ActionNothing case, where
	// it is a leftover from an install whose foreign hook has since been
	// rewritten by its owner. Ownership is re-checked rather than assumed
	// from the path (roast finding R-01c2b0): writeSidecar already refuses to
	// overwrite a file roast did not write, and a delete that trusted the
	// pathname alone would contradict it.
	if err := removeOwnedSidecar(st.SidecarPath); err != nil {
		return action, st, err
	}
	return action, st, nil
}

// ManualLine is the single line a user must add to a foreign hook we refuse
// to edit. It resolves the sidecar through git rather than hardcoding a path,
// so it keeps working if the repo moves or core.hooksPath changes.
func ManualLine() string {
	return `"$(git rev-parse --git-path ` + "hooks/" + SidecarName + `)" || exit $?`
}

// RecordDecision writes the per-repo enrollment decision. Recording one also
// drops any "we already hinted at this repo" marker: the hint exists only for
// the undecided state, and leaving it behind would suppress a legitimate hint
// if the decision were ever cleared.
func RecordDecision(repo string, d Decision) error {
	// --unset on a key that isn't there exits 5; that is not a failure here.
	_ = gitConfigUnset(repo, configKeyNotice)
	if d == DecisionNone {
		_ = gitConfigUnset(repo, configKeyDecision)
		return nil
	}
	return gitConfigSet(repo, configKeyDecision, string(d))
}

// RecordNotice remembers that we already told this repo how to enable the
// gate, so a non-interactive run says it once rather than on every roast.
func RecordNotice(repo string) error {
	return gitConfigSet(repo, configKeyNotice, "sent")
}

// readDecision maps the recorded value onto a Decision. Unrecognized values
// read as DecisionNone: a typo should mean "still unanswered", not a silent
// enable or a permanent decline.
func readDecision(repo string) Decision {
	switch strings.ToLower(strings.TrimSpace(gitConfigGet(repo, configKeyDecision))) {
	case "enabled", "enable", "true", "yes", "on", "1":
		return DecisionEnabled
	case "declined", "disabled", "false", "no", "off", "0":
		return DecisionDeclined
	default:
		return DecisionNone
	}
}

// sidecarCurrent reports whether the sidecar script exists, is executable,
// and matches the embedded script.
func sidecarCurrent(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		return false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(body) == HookScript
}

// shellInterpreters are the interpreters whose scripts can safely carry an
// appended POSIX-sh block. Anything else (python, ruby, node, a compiled
// binary) gets the manual line instead.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "ksh": true, "zsh": true,
}

// appendable reports whether a foreign hook body is a plain shell script we
// can append to. Conservative on purpose: the cost of a false negative is one
// printed line, the cost of a false positive is a corrupted hook in someone
// else's repo.
func appendable(body string) bool {
	if body == "" || !utf8.ValidString(body) {
		return false
	}
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !strings.HasPrefix(line, "#!") {
		return false
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return false
	}
	interp := filepath.Base(fields[0])
	if interp == "env" {
		if len(fields) < 2 {
			return false
		}
		// `env -S`, `env VAR=x prog`, ... — anything but a bare program name
		// is more shape than we want to reason about.
		if strings.HasPrefix(fields[1], "-") || strings.Contains(fields[1], "=") {
			return false
		}
		interp = filepath.Base(fields[1])
	}
	return shellInterpreters[interp]
}

// appendedBlock locates our block in a hook's lines and returns its inclusive
// line range. A block counts only when it is complete, ordered, AND actually
// contains the invocation — markers alone are public comment text, and
// treating a marker pair as proof of authorship lets `gate disable` delete a
// region of a file roast never wrote (roast findings R-911598, R-742ef8).
// Anything less reads as foreign, which is the safe classification: a begin
// with no end would otherwise truncate the file from the marker to EOF.
//
// Deliberately NOT an exact-bytes match on the generated block: a user who
// edits a comment inside it should still have a recognizable (and removable)
// gate, not a second block appended on the next enable.
func appendedBlock(lines []string) (start, end int, ok bool) {
	start = -1
	hasInvocation := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == appendBegin:
			start, hasInvocation = i, false
		case trimmed == appendEnd && start >= 0:
			if hasInvocation {
				return start, i, true
			}
			start = -1 // a marker pair with no invocation is not our block
		case start >= 0 && trimmed == ManualLine():
			hasInvocation = true
		}
	}
	return -1, -1, false
}

// appendBlock inserts our invocation into a foreign hook, immediately after
// its shebang, preserving every byte of their content and their mode.
//
// After the shebang, not at EOF (roast finding R-5351f8): a hook that ends in
// `exit 0` or `exec some-other-hook` — an ordinary shape — never reaches an
// appended tail, so an EOF block would report "installed" while never running
// once. Shell control flow cannot be analysed safely, so the only placement
// that is reliably reached is the top. The tradeoff is ordering: roast runs
// before their checks, and a blocking finding means their hook does not run
// at all. That is the right way round — a gate that sometimes silently does
// not run is worse than one that runs first.
func appendBlock(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Their mode is preserved exactly, execute bit included (roast finding
	// R-5dae8f): OR-ing 0o100 in would silently re-enable a hook whose owner
	// disabled it with chmod -x, which is precisely the surprise `roast gate
	// enable` promises not to spring. A non-executable hook stays
	// non-executable, Active() reports the gate as not running, and the CLI
	// tells the user to chmod +x if they want it.
	mode := os.FileMode(0o755)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	// appendable() has already established that line 1 is a shell shebang.
	shebangEnd := strings.IndexByte(string(body), '\n')
	if shebangEnd < 0 {
		return fmt.Errorf("append gate to %s: no complete shebang line", path)
	}
	block := "\n" + appendBegin + "\n" +
		"# Added by `roast gate enable`. Delete this block (or run\n" +
		"# `roast gate disable`) to remove the gate; the rest of this file is yours.\n" +
		ManualLine() + "\n" +
		appendEnd + "\n"
	var b strings.Builder
	b.Write(body[:shebangEnd+1])
	b.WriteString(block)
	b.Write(body[shebangEnd+1:])
	return writeFileAtomic(path, b.String(), mode)
}

// stripBlock removes our block from a foreign hook, leaving everything else —
// including edits made around the block — byte-identical apart from the block
// and the blank line we inserted before it. A malformed block is an error,
// not a best-effort delete: guessing where someone else's file resumes is how
// you truncate it.
func stripBlock(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o755)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	lines := strings.Split(string(body), "\n")
	start, end, ok := appendedBlock(lines)
	if !ok {
		return fmt.Errorf("remove gate from %s: the roast block has no closing marker; left the file untouched (delete the block by hand)", path)
	}
	// Drop the single blank separator line we inserted before the block.
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	kept := append(append([]string{}, lines[:start]...), lines[end+1:]...)
	return writeFileAtomic(path, strings.Join(kept, "\n"), mode)
}

// writeScript writes an executable script, creating its directory if needed.
func writeScript(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return writeFileAtomic(path, body, 0o755)
}

// writeSidecar writes the gate script to the sidecar path, but only when that
// path is absent or already ours. Nothing in Inspect establishes ownership of
// `hooks/roast-pre-commit` (roast finding R-a90671) — a repo is free to have
// its own file by that name — and the never-clobber rule is about files roast
// did not write, not just about `pre-commit`. Refuse rather than overwrite:
// the user can rename their file or wire the gate by hand.
func writeSidecar(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink roast did not create; move it aside and re-run `roast gate enable`", path)
	case err == nil:
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		if !strings.HasPrefix(string(body), hookHeader) {
			return fmt.Errorf("%s already exists and was not written by roast; move it aside and re-run `roast gate enable`", path)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return writeScript(path, HookScript)
}

// removeOwnedSidecar deletes the sidecar only when its anchored header proves
// roast wrote it. A symlink, a foreign file, or anything unreadable is left
// exactly where it is — the mirror of writeSidecar's refusal to overwrite.
func removeOwnedSidecar(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.HasPrefix(string(body), hookHeader) {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// A manually wired foreign hook can also use a shared sidecar. Refresh only
// an existing regular file whose header proves Roast ownership; never create
// a new sidecar or overwrite a foreign file while disabling enrollment.
func refreshOwnedSidecar(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.HasPrefix(string(body), hookHeader) && string(body) != HookScript {
		return writeSidecar(path)
	}
	return nil
}

// writeFileAtomic writes body to path via a temp file in the same directory
// plus a rename, so git never sees a half-written hook.
//
// os.CreateTemp, not a predictable `path + ".roast-tmp"` (roast finding
// R-306969): core.hooksPath can point at a directory inside the working tree
// (`.githooks` is a common convention), so a checked-out branch can contain a
// file at the temp path. os.WriteFile follows a symlink there and truncates
// its target — an arbitrary-file-overwrite primitive handed to whatever repo
// you happened to run `roast gate enable` in. CreateTemp opens O_EXCL with an
// unpredictable name, which cannot follow anything.
func writeFileAtomic(path, body string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".roast-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeds
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// CreateTemp makes the file 0600; the rename carries the temp file's mode
	// onto path, so set the real mode before renaming.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// resolveHookPath resolves a file under the repo's hooks directory, honouring
// core.hooksPath (git's own --git-path does that for us) and returning an
// absolute path. In a linked worktree git resolves hooks against the common
// git dir, which is correct: hooks are shared across a repo's worktrees.
func resolveHookPath(repo, name string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--git-path", "hooks/"+name).Output()
	if err != nil {
		return "", fmt.Errorf("resolve hooks path in %s: %w", repo, err)
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", fmt.Errorf("resolve hooks path in %s: git returned nothing", repo)
	}
	if !filepath.IsAbs(p) {
		// --git-path prints relative to the git command's working directory,
		// which is repo because of -C.
		p = filepath.Join(repo, p)
	}
	return p, nil
}

func gitConfigGet(repo, key string) string {
	out, err := exec.Command("git", "-C", repo, "config", "--local", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitConfigSet(repo, key, value string) error {
	cmd := exec.Command("git", "-C", repo, "config", "--local", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitConfigUnset(repo, key string) error {
	return exec.Command("git", "-C", repo, "config", "--local", "--unset", key).Run()
}
