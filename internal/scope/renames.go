package scope

import (
	"fmt"
	"strings"
)

// compactRenames shrinks moved files in a --no-renames diff. A move shows up
// there as a whole-file deletion plus a whole-file addition; a refactor that
// moves hundreds of files buries its real edits under megabytes of both and
// gets truncated by Fit. This asks git which of the diff's own deleted and
// added paths are renames of each other and swaps each such pair for git's
// rename section: a "rename from/rename to" header plus hunks for only the
// lines that changed.
//
// Excludes still apply to both sides, which is why the package otherwise
// diffs with --no-renames. Git's rename detection can pair a path across an
// exclude boundary, so a pair is accepted only when its deleted side and its
// added side both appear in diffText, which is already past every pathspec
// exclude and linguist marker. A file can therefore never be paired with, or
// surface through, an excluded path.
//
// ref and spec are the revision and pathspec arguments diffText was built
// with, and maxSection its per-file ceiling. It returns the new diff and the
// number of pairs collapsed; on any git error it returns the input
// unchanged, since the uncompacted diff is still correct.
func compactRenames(repo string, ref, spec []string, diffText string, maxSection int) (string, int) {
	sections := splitSections(diffText)
	var deleted, added []string
	for _, s := range sections {
		switch s.kind {
		case sectionDeleted:
			deleted = append(deleted, s.path)
		case sectionAdded:
			added = append(added, s.path)
		}
	}
	if len(deleted) == 0 || len(added) == 0 {
		return diffText, 0
	}
	args := func(extra ...string) []string {
		a := append(append(append([]string{"diff", "-M"}, PatchFlags...), extra...), ref...)
		return append(a, spec...)
	}
	status, err := gitOut(repo, args("--name-status", "-z")...)
	if err != nil {
		return diffText, 0
	}
	isDeleted, isAdded := toSet(deleted), toSet(added)
	paired := make(map[string]struct{})
	accepted := make(map[[2]string]struct{})
	// Walk whole records: a status field, then two paths for a rename or copy
	// (R<score>, C<score>) and one path otherwise. Matching "R" on any field
	// would read a path such as README.md as a status.
	fields := splitNUL(status)
	for i := 0; i < len(fields); {
		code := fields[i]
		if !strings.HasPrefix(code, "R") && !strings.HasPrefix(code, "C") {
			i += 2
			continue
		}
		if i+2 >= len(fields) {
			break
		}
		from, to := fields[i+1], fields[i+2]
		i += 3
		if !strings.HasPrefix(code, "R") {
			continue
		}
		if _, ok := isDeleted[from]; !ok {
			continue
		}
		if _, ok := isAdded[to]; !ok {
			continue
		}
		accepted[[2]string{from, to}] = struct{}{}
	}
	if len(accepted) == 0 {
		return diffText, 0
	}
	full, _, err := gitPatchOut(repo, maxSection, args()...)
	if err != nil {
		return diffText, 0
	}
	// Keep only the accepted pairs' rename sections from git's -M diff. A
	// pair whose section can't be found keeps its original delete and add.
	var renamed strings.Builder
	for _, sec := range splitSections(full) {
		from, to, ok := renameOf(sec.text)
		if _, want := accepted[[2]string{from, to}]; !ok || !want {
			continue
		}
		paired[from], paired[to] = struct{}{}, struct{}{}
		renamed.WriteString(sec.text)
		if !strings.HasSuffix(sec.text, "\n") {
			renamed.WriteString("\n")
		}
	}
	if len(paired) == 0 {
		return diffText, 0
	}

	var out strings.Builder
	for _, s := range sections {
		if _, ok := paired[s.path]; ok && s.kind != sectionOther {
			continue
		}
		out.WriteString(s.text)
	}
	if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
		out.WriteString("\n")
	}
	out.WriteString(renamed.String())
	return out.String(), len(paired) / 2
}

// renameOf reads the "rename from"/"rename to" lines of one -M section.
// Quoted paths are not decoded; such a pair is simply left uncollapsed.
func renameOf(section string) (from, to string, ok bool) {
	for line := range strings.SplitSeq(section, "\n") {
		if strings.HasPrefix(line, "@@") {
			break
		}
		if v, found := strings.CutPrefix(line, "rename from "); found {
			from = v
		} else if v, found := strings.CutPrefix(line, "rename to "); found {
			to = v
		}
	}
	if from == "" || to == "" || strings.HasPrefix(from, `"`) || strings.HasPrefix(to, `"`) {
		return "", "", false
	}
	return from, to, true
}

type sectionKind int

const (
	sectionOther sectionKind = iota
	sectionDeleted
	sectionAdded
)

type diffSection struct {
	text string
	path string
	kind sectionKind
}

// splitSections cuts a --no-renames diff at its "diff --git" headers. A
// section is a deletion or addition only when its header names one unquoted
// path on both sides (which --no-renames guarantees) and it carries git's
// "deleted file mode"/"new file mode" line before any hunk. Anything else,
// including quoted paths and leading text before the first header, is kept
// verbatim and never paired.
func splitSections(diffText string) []diffSection {
	var sections []diffSection
	start := 0
	for start < len(diffText) {
		next := strings.Index(diffText[start+1:], "\ndiff --git ")
		end := len(diffText)
		if next != -1 {
			end = start + 1 + next + 1
		}
		sections = append(sections, classify(diffText[start:end]))
		start = end
	}
	return sections
}

func classify(text string) diffSection {
	s := diffSection{text: text}
	header, rest, _ := strings.Cut(text, "\n")
	pair, ok := strings.CutPrefix(header, "diff --git ")
	if !ok || strings.HasPrefix(pair, `"`) || (len(pair)-5)%2 != 0 || len(pair) < 7 {
		return s
	}
	n := (len(pair) - 5) / 2
	path := pair[2 : 2+n]
	if pair[:2] != "a/" || pair[2+n:] != " b/"+path {
		return s
	}
	for line := range strings.SplitSeq(rest, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "--- "):
			return s
		case strings.HasPrefix(line, "deleted file mode "):
			s.path, s.kind = path, sectionDeleted
			return s
		case strings.HasPrefix(line, "new file mode "):
			s.path, s.kind = path, sectionAdded
			return s
		}
	}
	return s
}

func renameNote(notes []string, renamed int) []string {
	if renamed == 0 {
		return notes
	}
	return append(notes, fmt.Sprintf("collapsed %d moved file(s) to git's rename form (both paths in scope)", renamed))
}

func toSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}
