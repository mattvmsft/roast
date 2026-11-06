package scope

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DefaultMaxFileDiffBytes is the per-file diff ceiling Build applies when
// BuildOpts.MaxFileDiffBytes is zero.
const DefaultMaxFileDiffBytes = 1_000_000

// sectionOmittedMarker ends a file's diff section that was cut at the per-file
// ceiling. Inside a hunk a line starting with `\` is diff metadata (git's own
// "\ No newline at end of file"), so the grounder and the rename parser skip
// it as content, while the finder still reads why the hunk stops short.
const sectionOmittedMarker = "\\ roast: the rest of this file's diff is omitted (over %d bytes); use read_file to inspect the file\n"

// gitPatchOut runs a patch-producing git command and returns its stdout with
// every file section — a "diff --git" header up to the next one — cut at
// maxSection bytes. The output is streamed, so a huge file costs at most one
// section's ceiling in memory rather than its whole diff. cut names the
// sections that were shortened.
func gitPatchOut(repo string, maxSection int, args ...string) (out string, cut []string, err error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	out, cut, readErr := capSections(stdout, maxSection)
	if readErr != nil {
		// Report the read failure, not the kill it forces on Wait.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", nil, readErr
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		return "", nil, fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return out, cut, nil
}

// capSections copies a unified diff from r, cutting each file section at
// maxSection bytes and appending sectionOmittedMarker where it cut.
func capSections(r io.Reader, maxSection int) (string, []string, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var out strings.Builder
	var cut []string
	var header string // current section's "diff --git" line, for the cut list
	used := 0         // bytes kept from the current section
	dropping := false // the current section hit its ceiling
	atLineStart := true
	finish := func() {
		if dropping {
			if !strings.HasSuffix(out.String(), "\n") {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, sectionOmittedMarker, maxSection)
			cut = append(cut, sectionName(header))
		}
	}
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if atLineStart && bytes.HasPrefix(frag, []byte("diff --git ")) {
				finish()
				line := frag
				if i := bytes.IndexByte(line, '\n'); i >= 0 {
					line = line[:i]
				}
				header, used, dropping = string(line), 0, false
			}
			if !dropping {
				take := min(len(frag), maxSection-used)
				out.Write(frag[:take])
				used += take
				if take < len(frag) {
					dropping = true
				}
			}
			atLineStart = frag[len(frag)-1] == '\n'
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			finish()
			return out.String(), cut, nil
		default:
			return "", nil, err
		}
	}
}

// sectionName reads the new-side path from a "diff --git a/P b/P" header,
// falling back to the raw header for renames and quoted paths.
func sectionName(header string) string {
	pair := strings.TrimPrefix(header, "diff --git ")
	if n := (len(pair) - 5) / 2; len(pair) >= 7 && (len(pair)-5)%2 == 0 &&
		pair[:2] == "a/" && pair[2+n:] == " b/"+pair[2:2+n] {
		return pair[2 : 2+n]
	}
	return pair
}

// truncationNote is the operator-facing note for sections gitPatchOut or
// synthesizeNewFileDiff cut.
func truncationNote(notes, cut []string, maxSection int) []string {
	if len(cut) == 0 {
		return notes
	}
	return append(notes, fmt.Sprintf("diff for %d file(s) cut at %d bytes each: %s", len(cut), maxSection, listSome(cut, 10)))
}

func listSome(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + fmt.Sprintf(", and %d more", len(items)-limit)
}
