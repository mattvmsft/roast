package context

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bluesky-social/roast/internal/scope"
)

// NewQuoteGrounder returns Validate's grounding check: it reports whether a
// finding's code_quote appears verbatim — modulo whitespace — in something
// the finder could actually have seen for the file it cites: the file's
// content from the selected immutable tree (or live worktree for a zero Tree),
// the file's own section of the scope diff (where a quote of
// deleted code legitimately lives), or the file's base..HEAD delta when
// rangeSpec is non-empty (branch survey, where git_diff exposes deltas that
// are in no local file and not in sc.Diff).
//
// The corpus is exactly the set of bytes the finder could have seen — no wider,
// no narrower. Wider fails open on confabulation; narrower drops genuine
// findings. Every source added to a review has to be reflected here, and every
// source narrowed has to be narrowed here in the same change.
//
// Callers must pass the PRE-Fit diff: scope.Fit truncates sc.Diff in place,
// and a quote from beyond the truncation point is still genuinely grounded.
//
// Matching collapses every whitespace run to a single space on both sides so
// formatting-only differences do not reject otherwise verbatim quotes.
// Corpora are keyed by the cited file so real diff content cannot ground a
// finding anchored to an unrelated or fabricated path.
func NewQuoteGrounder(repo string, tree Tree, diffText, rangeSpec string) func(file, quote string) bool {
	diffCorpora := splitDiffByFile(diffText)

	fileCache := make(map[string]string)
	// One tree per scope, matching what the reviewer was actually served.
	// For a pinned corpus that is the selected tree's blob and ONLY that
	// blob: NumberedSnippet, read_file and grep_repo all resolve against
	// the same tree (context.go), so no context source can serve later
	// worktree content and admitting it would ground a
	// quote of code the finder could not have seen. That is not
	// fail-open safety margin, it is a fail-open hole — the one
	// direction this check exists to close. A live read obeys the same
	// corpus rules as read_file (resolveLive).
	//
	// A file up to groundingCacheBytes is normalized whole and cached, since
	// several findings often cite one file. A larger one is searched as a
	// stream on each call: read_file can page through all of it, so it stays
	// groundable, but holding it normalized in memory would let one huge
	// repository file cost gigabytes.
	fileMatch := func(file, nq string) bool {
		if c, ok := fileCache[file]; ok {
			return strings.Contains(c, nq)
		}
		size, open := fileSource(repo, tree, file)
		if open == nil {
			fileCache[file] = ""
			return false
		}
		r, done := open()
		if r == nil {
			fileCache[file] = ""
			return false
		}
		defer done()
		if size > groundingCacheBytes {
			return streamContains(r, nq)
		}
		b, err := io.ReadAll(io.LimitReader(r, groundingCacheBytes))
		if err != nil {
			return false
		}
		norm := normalizeWS(string(b))
		fileCache[file] = norm
		return strings.Contains(norm, nq)
	}

	rangeCache := make(map[string]string)
	rangeCorpus := func(file string) string {
		if rangeSpec == "" {
			return ""
		}
		if c, ok := rangeCache[file]; ok {
			return c
		}
		var norm string
		// Same single-file guard as gitDiffTool: the path comes from model
		// output, so reject pathspec magic before it reaches git, and `--`
		// keeps a filename from being read as a revision.
		if !strings.HasPrefix(file, ":") && !strings.ContainsAny(file, "*?[]\n") && !strings.HasSuffix(file, "/") {
			if _, ok := ResolveUnderRepo(repo, file); ok {
				args := append(append([]string{"-C", repo, "diff", "--no-renames"}, scope.PatchFlags...), rangeSpec, "--", file)
				if out, ok := cappedOutput(exec.Command("git", args...), groundingCacheBytes); ok {
					// Re-key through the per-file parser and take only the
					// exact cited path: a directory pathspec (`internal`,
					// `.`) expands to every changed descendant, and merging
					// that subtree into one corpus would let any descendant's
					// content ground a directory-cited finding. Sections key
					// to real file paths, so a directory key is empty.
					norm = splitDiffByFile(out)[file]
				}
			}
		}
		rangeCache[file] = norm
		return norm
	}

	return func(file, quote string) bool {
		nq := normalizeWS(quote)
		// A NUL in the quote could bridge the sentinels that keep sides,
		// hunks, and corpora from concatenating; no genuine quote contains
		// one (JSON can smuggle an escaped NUL), so reject outright.
		if nq == "" || strings.ContainsRune(nq, '\x00') {
			return false
		}
		if fileMatch(file, nq) {
			return true
		}
		if strings.Contains(diffCorpora[file], nq) {
			return true
		}
		return strings.Contains(rangeCorpus(file), nq)
	}
}

// groundingCacheBytes is the largest file the grounder holds normalized in
// memory, and the most of one survey range diff it reads.
const groundingCacheBytes = 8 << 20

// fileSource locates file in the reviewed corpus and returns its size and an
// opener, or a nil opener when the file is not in the corpus. The opener
// returns a nil reader on failure, and a cleanup to call when done.
func fileSource(repo string, tree Tree, file string) (int64, func() (io.Reader, func())) {
	if tree.Pinned() {
		if stagedPathIsMagic(file) {
			return 0, nil
		}
		spec := tree.OID + ":" + file
		out, err := exec.Command("git", "-C", repo, "cat-file", "-s", spec).Output()
		if err != nil {
			return 0, nil
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0, nil
		}
		return size, func() (io.Reader, func()) {
			cmd := exec.Command("git", "-C", repo, "cat-file", "blob", spec)
			stdout, err := cmd.StdoutPipe()
			if err != nil || cmd.Start() != nil {
				return nil, nil
			}
			return stdout, func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
	}
	p, ok := resolveLive(repo, file)
	if !ok {
		return 0, nil
	}
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return 0, nil
	}
	return info.Size(), func() (io.Reader, func()) {
		f, err := os.Open(p)
		if err != nil {
			return nil, nil
		}
		return f, func() { _ = f.Close() }
	}
}

// cappedOutput runs cmd and returns at most limit bytes of its stdout; ok is
// false when the command fails. Output past limit is not read: the process is
// killed instead of drained.
func cappedOutput(cmd *exec.Cmd, limit int64) (string, bool) {
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return "", false
	}
	b, _ := io.ReadAll(io.LimitReader(stdout, limit))
	var extra [1]byte
	truncated := false
	if n, _ := stdout.Read(extra[:]); n > 0 {
		truncated = true
		_ = cmd.Process.Kill()
	}
	if err := cmd.Wait(); err != nil && !truncated {
		return "", false
	}
	return string(b), true
}

// streamContains reports whether the whitespace-normalized content of r
// contains nq, which must already be normalized. It holds one read buffer plus
// a tail of len(nq) bytes, so a match spanning two reads is still found.
func streamContains(r io.Reader, nq string) bool {
	var n wsNormalizer
	buf := make([]byte, 64*1024)
	needle := []byte(nq)
	var window bytes.Buffer
	for {
		k, err := r.Read(buf)
		n.write(&window, string(buf[:k]), err != nil)
		if bytes.Contains(window.Bytes(), needle) {
			return true
		}
		if err != nil {
			return false
		}
		if keep := len(needle); window.Len() > keep {
			window.Next(window.Len() - keep)
		}
	}
}

// splitDiffByFile parses a unified diff into whitespace-normalized per-file
// corpora, keyed by both old and new path so quotes of deleted code key to
// the finding's file. Most sections name one path on both sides (the scope
// diffs with --no-renames); a collapsed move names its old and new paths,
// and keying both lets a quote ground under either. A section whose paths cannot be parsed even after
// quote-decoding contributes nothing: unattributable content in a shared
// residual would let real diff content ground a finding cited to any path,
// and per-file isolation outranks the (git-diff-shouldn't-produce-this) risk
// of failing closed on an unparseable section.
func splitDiffByFile(diffText string) map[string]string {
	corpora := make(map[string]string)
	if diffText == "" {
		return corpora
	}
	add := func(key, norm string) {
		if norm == "" {
			return
		}
		// \x00 survives normalization (it is not whitespace) and prevents a
		// quote from matching across a section boundary.
		if prev := corpora[key]; prev != "" {
			corpora[key] = prev + " \x00 " + norm
		} else {
			corpora[key] = norm
		}
	}
	// A section runs from one "diff --git " line to the next; text before
	// the first names no path and is skipped like any unattributable text.
	for rest := diffText; rest != ""; {
		section := rest
		if i := strings.Index(rest, "\ndiff --git "); i >= 0 {
			section, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = ""
		}
		paths := sectionPaths(section)
		if len(paths) == 0 {
			continue
		}
		norm := normalizedDiffSides(section)
		for _, p := range paths {
			add(p, norm)
		}
	}
	return corpora
}

// sectionPaths reads the paths one diff section's headers name.
func sectionPaths(section string) []string {
	var paths []string
	inHunk := false
	for l := range strings.SplitSeq(section, "\n") {
		if strings.HasPrefix(l, "@@") {
			inHunk = true
		}
		// Header parsing only outside hunks: a deleted content line reading
		// `-- a/other.go` renders in a hunk as `--- a/other.go` and must not
		// key this section to an unrelated path. (A partially-parseable
		// header pair is fine to accept: outside collapsed moves old and new
		// name the same path, and a move's two paths both belong to it.)
		//
		// `--- /dev/null` (add) and `+++ /dev/null` (delete) yield no path
		// and contribute no key, as intended.
		if inHunk {
			continue
		}
		if p, ok := cutDiffPath(l, "--- ", "a/"); ok {
			paths = append(paths, p)
		} else if p, ok := cutDiffPath(l, "+++ ", "b/"); ok {
			if len(paths) == 0 || paths[len(paths)-1] != p {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// cutDiffPath extracts the repo-relative path from a `--- a/…` or `+++ b/…`
// header line. core.quotePath (default true) makes git wrap a path holding
// special or non-ASCII bytes in C-style double quotes — `--- "a/f\303\266.go"`
// — which strconv.Unquote decodes (git's escapes are a subset of Go's).
func cutDiffPath(line, marker, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(line, marker)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(rest, `"`) {
		u, err := strconv.Unquote(rest)
		if err != nil {
			return "", false
		}
		rest = u
	}
	return strings.CutPrefix(rest, prefix)
}

// normalizedDiffSides converts diff text into a whitespace-normalized
// grounding corpus with the old and new sides of each hunk kept separate:
// context lines belong to both sides, deletions to the old, additions to the
// new. Merging the sides into one stream would both admit hybrid quotes that
// span a -/+ interleave (content that existed in neither file version) and
// reject a legitimate old-side multiline quote whose lines have new-side
// lines interleaved between them. Sides and hunks are joined with a \x00
// sentinel (not whitespace, so it survives normalization) so a quote cannot
// match across those boundaries. Non-hunk lines (headers) contribute nothing.
func normalizedDiffSides(s string) string {
	var sides sidesBuilder
	// Neither side can outgrow its input. Sizing both up front costs twice
	// the section once, where growing a large builder in steps costs several
	// times the final size.
	sides.old.Grow(len(s))
	sides.new.Grow(len(s))
	for l := range strings.SplitSeq(s, "\n") {
		sides.line(l)
	}
	return sides.corpus()
}

// sidesBuilder is normalizedDiffSides fed one line at a time. Each side is
// normalized as it streams in, so a large diff is held once, normalized,
// rather than raw and then again normalized.
type sidesBuilder struct {
	old, new   strings.Builder
	oldN, newN wsNormalizer
	// oldAny/newAny record that a hunk line reached the side, even a blank
	// one that normalizes to nothing: a hunk sentinel follows any content.
	oldAny, newAny bool
	inHunk         bool
}

func (s *sidesBuilder) line(l string) {
	switch {
	case strings.HasPrefix(l, "@@"):
		s.sentinel(&s.old, &s.oldN, s.oldAny)
		s.sentinel(&s.new, &s.newN, s.newAny)
		s.inHunk = true
		return
	case strings.HasPrefix(l, "diff --git "):
		s.inHunk = false
		return
	}
	if !s.inHunk || l == "" {
		return
	}
	text := l[1:]
	switch l[0] {
	case ' ':
		s.oldN.writeLine(&s.old, text)
		s.newN.writeLine(&s.new, text)
		s.oldAny, s.newAny = true, true
	case '-':
		s.oldN.writeLine(&s.old, text)
		s.oldAny = true
	case '+':
		s.newN.writeLine(&s.new, text)
		s.newAny = true
	}
	// Anything else inside a hunk ("\ No newline at end of file") is
	// diff metadata, not file content — skipped.
}

func (s *sidesBuilder) sentinel(b *strings.Builder, n *wsNormalizer, any bool) {
	if any {
		n.write(b, "\n\x00\n", false)
	}
}

func (s *sidesBuilder) corpus() string {
	s.oldN.write(&s.old, "", true)
	s.newN.write(&s.new, "", true)
	o, n := s.old.String(), s.new.String()
	switch {
	case o == "":
		return n
	case n == "":
		return o
	default:
		return o + " \x00 " + n
	}
}

// normalizeWS collapses every whitespace run to a single space and trims
// both ends: strings.Join(strings.Fields(s), " "), without the per-word slice
// headers that made a large diff cost several times its size.
func normalizeWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var n wsNormalizer
	n.write(&b, s, true)
	return b.String()
}

// wsNormalizer is normalizeWS over a stream of chunks. Whitespace is
// unicode.IsSpace, as in strings.Fields; bytes that are not valid UTF-8 are
// kept as they are.
type wsNormalizer struct {
	carry   string // incomplete UTF-8 sequence held from the previous chunk
	started bool   // a non-space rune has been written
	space   bool   // whitespace seen since the last non-space rune
}

// wsSink receives normalized text: a strings.Builder for a corpus, or a
// reused bytes.Buffer for a streaming search.
type wsSink interface {
	WriteByte(byte) error
	WriteString(string) (int, error)
}

// write normalizes s onto b. final marks the last chunk, so a trailing
// incomplete UTF-8 sequence is emitted rather than held for more input.
func (n *wsNormalizer) write(b wsSink, s string, final bool) {
	if n.carry != "" {
		s, n.carry = n.carry+s, ""
	}
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c == ' ' || (c >= '\t' && c <= '\r') {
				n.space = true
			} else {
				n.emit(b, s[i:i+1])
			}
			i++
			continue
		}
		if !final && !utf8.FullRuneInString(s[i:]) {
			n.carry = s[i:]
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			n.space = true
		} else {
			n.emit(b, s[i:i+size])
		}
		i += size
	}
}

func (n *wsNormalizer) writeLine(b wsSink, text string) {
	n.write(b, text, false)
	n.write(b, "\n", false)
}

func (n *wsNormalizer) emit(b wsSink, s string) {
	if n.space && n.started {
		_ = b.WriteByte(' ')
	}
	n.space, n.started = false, true
	_, _ = b.WriteString(s)
}
