package review

import "testing"

func TestFingerprint_Stability(t *testing.T) {
	f := Finding{
		File:        "internal/foo/bar.go",
		CodeQuote:   "x  :=  1\n  y := 2",
		Description: "Potential nil deref on foo after bar returns",
	}
	// Same finding with collapsed whitespace must match.
	f2 := f
	f2.CodeQuote = "x := 1\ny := 2"
	if Fingerprint(f) != Fingerprint(f2) {
		t.Errorf("whitespace variations produced different fingerprints:\n  %s\n  %s",
			Fingerprint(f), Fingerprint(f2))
	}
}

func TestFingerprint_DescriptionIgnored(t *testing.T) {
	// H7: the description is the field the model rewords every run, so it is
	// not part of the similarity key at all — any rewording must match.
	f := Finding{File: "a.go", CodeQuote: "x := 1", Description: "nil deref in handler"}
	f2 := f
	f2.Description = "completely different wording of the same claim"
	if Fingerprint(f) != Fingerprint(f2) {
		t.Error("description must not affect the fingerprint")
	}
}

func TestFingerprint_DifferentFiles(t *testing.T) {
	f := Finding{File: "a.go", CodeQuote: "x := 1", Description: "d"}
	f2 := f
	f2.File = "b.go"
	if Fingerprint(f) == Fingerprint(f2) {
		t.Error("different files should produce different fingerprints")
	}
}

func TestFingerprint_DifferentCode(t *testing.T) {
	f := Finding{File: "a.go", CodeQuote: "x := 1", Description: "d"}
	f2 := f
	f2.CodeQuote = "y := 2"
	if Fingerprint(f) == Fingerprint(f2) {
		t.Error("different code_quotes should produce different fingerprints")
	}
}

func TestFingerprint_Length16(t *testing.T) {
	f := Finding{File: "a.go", CodeQuote: "x", Description: "d"}
	fp := Fingerprint(f)
	if len(fp) != 16 {
		t.Errorf("want 16 hex chars, got %d (%q)", len(fp), fp)
	}
}
