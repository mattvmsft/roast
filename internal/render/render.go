// Package render converts a completed review run into its two output formats:
// JSON on stdout (consumed by hooks and tooling) and Markdown on stderr
// (human-readable progress and summary).
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/review"
	"github.com/bluesky-social/roast/internal/textsafe"
)

// SizeEntry records the byte contribution of one file to the diff payload.
type SizeEntry struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

// Report is everything needed to render one review run.
type Report struct {
	Result        *review.Result
	Mode          string
	FilesReviewed []string
	Task          string
	RunID         string
	// Budgets is the run's budget provenance: every ceiling in force this run
	// (value + default/flag/env source) plus the ledger's reconciled spend,
	// so eval comparisons cannot silently mix budget regimes.
	Budgets           map[string]any
	Quick             bool
	Verified          bool
	DiffTruncated     bool
	OriginalDiffBytes int
	SizeBreakdown     []SizeEntry
	Usage             map[string]llm.Usage
	// Prior maps a finding fingerprint to an optional human note.
	Prior map[string]string
	// PromptPacks is the set of language addenda active for this run
	// (from review.ClassifyLangs or --lang), for post-hoc dimension analysis.
	PromptPacks []string

	// Survey coverage contract (populated only for survey scope). A clean
	// survey must NOT be machine-indistinguishable from a clean full review,
	// so these fields make the triage nature and completeness legible to any
	// consumer. Empty ReviewKind means a normal (diff) review — the fields
	// are omitted from output entirely.
	ReviewKind        string // "survey_triage" for survey scope, else ""
	Completion        string // "partial" | "complete"
	ManifestFileCount int    // number of files in the manifest (len(sc.Files))
	StopReason        string // "converged" | "budget_exhausted" | "incomplete_no_submission"

}

// ToJSON returns the machine-readable JSON payload written to stdout.
// Shape matches the Python v2 to_json output; per-finding fingerprint and
// prior-sighting annotations are added when present.
func ToJSON(r Report) string {
	type findingOut struct {
		review.Finding
		Fingerprint   string `json:"fingerprint,omitempty"`
		PriorSighting string `json:"prior_sighting,omitempty"`
	}

	marshalFindings := func(fs []review.Finding) []findingOut {
		out := make([]findingOut, len(fs))
		for i, f := range fs {
			fo := findingOut{Finding: f}
			fp := review.Fingerprint(f)
			fo.Fingerprint = fp
			if r.Prior != nil {
				if note, ok := r.Prior[fp]; ok {
					fo.PriorSighting = note
				}
			}
			out[i] = fo
		}
		return out
	}

	payload := map[string]any{
		"findings":         marshalFindings(r.Result.Findings),
		"dropped_findings": marshalFindings(r.Result.Dropped),
		"assessment":       r.Result.Assessment,
		"mode":             r.Mode,
		"quick":            r.Quick,
		"verified":         r.Verified,
		"files_reviewed":   r.FilesReviewed,
		"notes":            r.Result.Notes,
		"usage":            r.Usage,
		"prompt_packs":     r.PromptPacks,
	}
	if r.RunID != "" {
		payload["run_id"] = r.RunID
	}
	// Cutoff state must survive into machine-readable output even outside
	// survey scope (where completion/stop_reason don't render): a non-survey
	// interactive cutoff with retained findings reads FINDINGS_PRESENT and
	// would otherwise be indistinguishable from a completed review. The
	// survey block below owns the stop_reason key when ReviewKind is set.
	if r.Result.Incomplete {
		payload["incomplete"] = true
	}
	if r.Result.StopReason != "" && r.ReviewKind == "" {
		payload["stop_reason"] = r.Result.StopReason
	}
	if r.Budgets != nil {
		payload["budgets"] = r.Budgets
	}
	if r.DiffTruncated {
		payload["diff_truncated"] = true
		payload["original_diff_bytes"] = r.OriginalDiffBytes
		payload["size_breakdown"] = r.SizeBreakdown
	}
	if r.ReviewKind != "" {
		payload["review_kind"] = r.ReviewKind
		payload["completion"] = r.Completion
		payload["manifest_file_count"] = r.ManifestFileCount
		payload["stop_reason"] = r.StopReason
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// ToMarkdown returns the human-readable Markdown summary written to stderr.
// Findings are sorted severity-descending then confidence-descending. When
// showDropped is true a dropped-findings section is appended. Finding text is
// escaped for terminals (see renderFinding); ToJSON needs no such pass because
// encoding/json escapes control characters itself.
func ToMarkdown(r Report, showDropped bool) string {
	res := r.Result
	var lines []string

	if r.ReviewKind != "" {
		lines = append(lines, fmt.Sprintf(
			"> Survey triage — manifest contains %d files, completion=%s, stop=%s",
			r.ManifestFileCount, r.Completion, r.StopReason,
		))
		lines = append(lines, "")
	}

	nRefuted := 0
	for _, f := range res.Dropped {
		if f.DropReason == review.DropRefutedByVerifier {
			nRefuted++
		}
	}
	nMalformed := len(res.Dropped) - nRefuted

	if len(res.Findings) == 0 {
		switch {
		case res.Assessment == review.AssessmentNoDiff:
			lines = append(lines, "## Nothing to review in this scope — no review ran")
			lines = append(lines, "")
			lines = append(lines, "No model was called, so this is NOT a clean review. Already committed? Try `--base <ref>`; use `--staged` before committing or `--scope repo` for a repository survey.")
		// Cutoff first: an incomplete cutoff carries BOTH markers and must
		// name the specific stop reason, not generic budget exhaustion.
		case res.StopReason == "deadline" || res.StopReason == "token_budget":
			// Zero findings before the cutoff — a truncated review must
			// never read as clean.
			lines = append(lines, fmt.Sprintf("## Roast cut off early (%s) — no findings before the cutoff", res.StopReason))
			lines = append(lines, "")
			lines = append(lines, "The review was cut short by a run ceiling; this is NOT a clean review.")
		case res.Assessment == review.AssessmentIncompleteNoSubmission:
			lines = append(lines, "## Roast incomplete — no findings were submitted")
			lines = append(lines, "")
			lines = append(lines, "The interactive review exhausted its budget before submitting; this is NOT a clean review.")
		default:
			lines = append(lines, "## Roast complete — no findings")
			lines = append(lines, "")
			msg := "The adversary found nothing to complain about."
			if nRefuted > 0 {
				msg += fmt.Sprintf(" (%d candidate finding(s) were refuted by the cross-model verifier.)", nRefuted)
			}
			lines = append(lines, msg)
		}
	} else {
		if res.StopReason != "" {
			// Retained/wrap-up findings from any cutoff are partial — the
			// human summary must say so, not read as a completed review.
			lines = append(lines, fmt.Sprintf("## Roast cut off early (%s) — partial findings follow", res.StopReason), "")
		}
		suffix := ""
		if r.Verified {
			suffix = " (cross-model verified)"
		}
		lines = append(lines, fmt.Sprintf("## Roast: %d finding(s)%s", len(res.Findings), suffix))
	}
	if nMalformed > 0 {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("*(%d candidate(s) dropped by schema gates; --show-dropped to see them.)*", nMalformed))
	}
	lines = append(lines, "")

	sorted := make([]review.Finding, len(res.Findings))
	copy(sorted, res.Findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := sorted[i].EffectiveSeverity().Rank(), sorted[j].EffectiveSeverity().Rank()
		if ri != rj {
			return ri > rj
		}
		return sorted[i].Confidence > sorted[j].Confidence
	})

	for i, f := range sorted {
		lines = append(lines, renderFinding(i+1, f, false, r.Prior)...)
	}

	if showDropped && len(res.Dropped) > 0 {
		lines = append(lines, "## Dropped findings (--show-dropped)")
		lines = append(lines, "")
		for i, f := range res.Dropped {
			lines = append(lines, renderFinding(i+1, f, true, r.Prior)...)
		}
	}
	return strings.Join(lines, "\n")
}

// renderFinding formats one finding as a Markdown block. prior maps
// fingerprint → human note; when the finding's fingerprint has an entry it is
// appended as "seen previously" line.
func renderFinding(i int, f review.Finding, dropped bool, prior map[string]string) []string {
	sev := textsafe.Line(string(f.Severity))
	if sev == "" {
		sev = "INFO"
	}
	if eff := f.EffectiveSeverity(); eff != f.Severity {
		sev = fmt.Sprintf("%s (finder claimed %s — verifier recalibrated)", textsafe.Line(string(eff)), sev)
	}
	confStr := fmt.Sprintf(" · conf %.2f", f.Confidence)
	tag := ""
	if dropped {
		reason := f.DropReason
		if reason == "" {
			reason = "?"
		}
		tag = fmt.Sprintf("[DROPPED:%s] ", textsafe.Line(reason))
	}
	path := f.File
	if path == "" {
		path = "?"
	}
	lnStr := "?"
	if len(f.Lines) == 1 {
		lnStr = fmt.Sprintf("%d", f.Lines[0])
	} else if len(f.Lines) > 1 {
		lnStr = fmt.Sprintf("%d-%d", f.Lines[0], f.Lines[len(f.Lines)-1])
	}

	desc := textsafe.Line(strings.TrimSpace(f.Description))
	var out []string
	// Stable alias over ordinal: "R-8d8d19" survives re-runs and
	// re-ordering, "finding #2" does not. Ordinal fallback only for findings
	// predating identity assignment (old run records).
	handle := textsafe.Line(f.Alias)
	if handle == "" {
		handle = fmt.Sprintf("%d.", i)
	}
	out = append(out, fmt.Sprintf("### %s %s[%s%s] %s", handle, tag, sev, confStr, desc))
	out = append(out, "- **File:** "+inlineCode(textsafe.Line(path)+":"+lnStr))
	if f.CWE != "" {
		out = append(out, fmt.Sprintf("- **CWE:** %s", textsafe.Line(f.CWE)))
	}
	out = append(out, fmt.Sprintf("- **Reasoning:** %s", listItemText(f.Reasoning, "  ")))

	if f.Verdict != nil {
		v := f.Verdict
		vStatus := textsafe.Line(string(v.Status))
		vConfStr := fmt.Sprintf(" (conf %.2f)", v.Confidence)
		verifierName := textsafe.Line(v.Verifier)
		if verifierName == "" {
			verifierName = "?"
		}
		out = append(out, fmt.Sprintf("- **Verifier (%s):** %s%s", verifierName, vStatus, vConfStr))
		if v.Grounding != "" {
			out = append(out, fmt.Sprintf("  - grounding: %s", listItemText(v.Grounding, "    ")))
		}
	}

	for _, pair := range [][2]string{{"Current", f.CodeQuote}, {"Proposed fix", f.FixedCode}} {
		label, block := pair[0], strings.TrimRight(textsafe.Text(pair[1]), "\n")
		if block != "" {
			fence := codeFence(block)
			out = append(out, fmt.Sprintf("- **%s:**", label))
			out = append(out, "  "+fence)
			for _, ln := range strings.Split(block, "\n") {
				out = append(out, "  "+ln)
			}
			out = append(out, "  "+fence)
		} else if dropped {
			out = append(out, fmt.Sprintf("- **%s:** *(not provided)*", label))
		}
	}

	// Prior-sighting annotation.
	fp := review.Fingerprint(f)
	if prior != nil {
		if note, ok := prior[fp]; ok {
			out = append(out, fmt.Sprintf("- **Seen previously:** %s", textsafe.Line(note)))
		}
	}

	out = append(out, "")
	return out
}

// listItemText sanitizes a multi-line field and indents each continuation
// line, so model-written text cannot leave its list item and pose as a
// report heading.
func listItemText(s, indent string) string {
	return strings.ReplaceAll(textsafe.Text(strings.TrimSpace(s)), "\n", "\n"+indent)
}

// codeFence returns a backtick fence longer than any backtick run in content,
// so a quoted ``` line cannot close the block early.
func codeFence(content string) string {
	return strings.Repeat("`", max(3, longestBacktickRun(content)+1))
}

// inlineCode wraps s in a code span whose delimiter outruns any backtick run
// inside it, padding with spaces when s starts or ends with a backtick.
func inlineCode(s string) string {
	delim := strings.Repeat("`", longestBacktickRun(s)+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return delim + " " + s + " " + delim
	}
	return delim + s + delim
}

func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}
