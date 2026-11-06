package record

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMetaAndArtifactsRoundTripWithoutSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	meta := Meta{
		RunID: "20260919T120000Z-1234abcd", Repo: "/tmp/repo",
		Roles:   map[string]RoleMeta{"finder": {BaseURL: "https://example.test/v1", Model: "model", API: "responses"}},
		Digests: DigestMeta{InputKind: "diff", Input: Digest("content")},
	}
	if _, err := WriteMetaRequired(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteArtifactRequired(meta.RunID, "input", "content"); err != nil {
		t.Fatal(err)
	}
	got, runDir, err := ReadMeta("20260919T120000Z")
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != meta.RunID || got.Roles["finder"].BaseURL != "https://example.test/v1" {
		t.Fatalf("meta = %#v", got)
	}
	content, err := ReadArtifact(runDir, "input")
	if err != nil || content != "content" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "input.gz")); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureEnvNeverRecordsAPIKeys(t *testing.T) {
	t.Setenv("ROAST_MAX_TOTAL_TOKENS", "123")
	t.Setenv("ROAST_FINDER_API_KEY", "secret-finder")
	t.Setenv("ROAST_VERIFIER_API_KEY", "secret-verifier")
	t.Setenv("ROAST_FUTURE_SETTING", "possibly-secret")
	values, names := CaptureEnv()
	if values["ROAST_MAX_TOTAL_TOKENS"] != "123" {
		t.Fatalf("values = %#v", values)
	}
	for name, value := range values {
		if name == "ROAST_FINDER_API_KEY" || name == "ROAST_VERIFIER_API_KEY" || value == "secret-finder" || value == "secret-verifier" || value == "possibly-secret" {
			t.Fatalf("secret value captured: %s=%q", name, value)
		}
	}
	for _, name := range names {
		if name == "ROAST_FINDER_API_KEY" || name == "ROAST_VERIFIER_API_KEY" {
			t.Fatalf("secret name captured: %v", names)
		}
	}
}

func TestRoleMetaReadsLegacyAPIField(t *testing.T) {
	if got := (RoleMeta{API: "responses"}).WireProtocol(); got != "responses" {
		t.Fatalf("wire protocol = %q", got)
	}
	if got := (RoleMeta{Protocol: "anthropic", API: "responses"}).WireProtocol(); got != "anthropic" {
		t.Fatalf("wire protocol = %q", got)
	}
}

func TestCaptureEnvRedactsConfiguredCredentialSource(t *testing.T) {
	t.Setenv("ROAST_DEADLINE", "secret-used-as-credential")
	values, names := CaptureEnv("ROAST_DEADLINE")
	if _, ok := values["ROAST_DEADLINE"]; ok || slices.Contains(names, "ROAST_DEADLINE") {
		t.Fatalf("configured credential source was captured: values=%v names=%v", values, names)
	}
}
