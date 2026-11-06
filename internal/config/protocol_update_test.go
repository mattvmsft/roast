package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const compatibilityConfig = `version: 1
# Keep this local configuration, not the template's defaults.
inference_profiles:
  finder:
    endpoint: https://models.example/v1
    model: claude-opus-5
    protocol: chat # keep the comment
    api_key: local-secret
    max_output_tokens: 8192
  verifier:
    endpoint: https://models.example/v1
    model: gpt-custom
    protocol: responses
    api_key_env: TEST_VERIFIER_KEY
  alternative:
    endpoint: https://models.example/v1/
    model: anthropic/claude-sonnet-5
    protocol: responses
roles:
  finder:
    profile: finder
    pin: true
    author_routes:
      openai: alternative
  verifier:
    profile: verifier
review:
  confidence_floor: 0.7
  excludes: [local-notes.md]
prompts:
  verifier: local-verifier.md
upload:
  enabled: false
  backend: presigned
  endpoint: https://local-upload.example
  token: local-upload-secret
`

func nativeTemplate() string {
	raw := strings.Replace(provisionTemplate, "model: finder-model", "model: claude-new-default", 1)
	return strings.Replace(raw, "protocol: chat", "protocol: anthropic", 1)
}

func TestProvisionUpdatesOnlyMatchingProfileProtocols(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("local credentials reached the template server")
		}
		_, _ = w.Write([]byte(nativeTemplate()))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(compatibilityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || !needs {
		t.Fatalf("needs update=%v, error=%v", needs, err)
	}
	// An existing v1 config must not be parsed as a legacy credential source,
	// even when the automatic enrollment flow passes the same path twice.
	if err := Provision(t.Context(), server.URL, path, path); err != nil {
		t.Fatal(err)
	}
	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"finder", "alternative"} {
		profile := before.InferenceProfiles[name]
		profile.Protocol = "anthropic"
		before.InferenceProfiles[name] = profile
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("protocol repair changed unrelated settings")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "# keep the comment") || !strings.Contains(string(raw), "# Keep this local") {
		t.Fatalf("local comments lost, error=%v", err)
	}
	backup, err := os.ReadFile(path + ".pre-protocol-update")
	if err != nil || string(backup) != compatibilityConfig {
		t.Fatalf("original config backup mismatch, error=%v", err)
	}
	for _, file := range []string{path, path + ".pre-protocol-update"} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config or backup permissions are not owner-only: %s, error=%v", file, err)
		}
	}
	if needs, err := NeedsProvision(path); err != nil || needs {
		t.Fatalf("repaired config still needs update=%v, error=%v", needs, err)
	}
}

func TestProtocolUpdateRequiresTemplateAuthorization(t *testing.T) {
	for name, template := range map[string]string{
		"different endpoint":           strings.ReplaceAll(nativeTemplate(), "https://models.example/v1", "https://other.example/v1"),
		"compatibility is intentional": provisionTemplate,
		"not an Anthropic model":       strings.Replace(nativeTemplate(), "claude-new-default", "gpt-default", 1),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ValidateProvisionDocument([]byte(template))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := updateProtocols([]byte(compatibilityConfig), cfg); err == nil || !strings.Contains(err.Error(), "no matching") {
				t.Fatalf("unauthorized protocol update: %v", err)
			}
		})
	}
}

func TestProtocolUpdatePreservesUnmatchedProfiles(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "    endpoint: https://models.example/v1/", "    endpoint: https://compat.example/v1/", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateProtocols([]byte(raw), template)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decode(updated)
	if err != nil || cfg.InferenceProfiles["alternative"].Protocol != "responses" || cfg.InferenceProfiles["finder"].Protocol != "anthropic" {
		t.Fatalf("unmatched compatibility endpoint changed: %v", err)
	}
}

func TestProtocolUpdateFailurePreservesOriginal(t *testing.T) {
	for _, failure := range []string{"backup mismatch", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(compatibilityConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			if failure == "backup mismatch" {
				if err := os.WriteFile(path+".pre-protocol-update", []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				link := filepath.Join(t.TempDir(), "link.yaml")
				if err := os.Symlink(path, link); err != nil {
					t.Skipf("cannot create symlink: %v", err)
				}
				path = link
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nativeTemplate())) }))
			defer server.Close()
			if err := Provision(t.Context(), server.URL, path, path); err == nil {
				t.Fatal("expected safe replacement failure")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != compatibilityConfig {
				t.Fatal("failed migration modified the original config")
			}
		})
	}
}

func TestProtocolUpdateRejectsAliasesThatChangeOtherSettings(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "protocol: chat # keep", "protocol: &shared chat # keep", 1)
	raw = strings.Replace(raw, "verifier: local-verifier.md", "verifier: *shared", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updateProtocols([]byte(raw), template); err == nil || !strings.Contains(err.Error(), "other settings") {
		t.Fatalf("unsafe alias migration not rejected: %v", err)
	}
}

func TestProtocolUpdateCanAddMissingProtocol(t *testing.T) {
	raw := strings.Replace(compatibilityConfig, "    protocol: chat # keep the comment\n", "", 1)
	template, err := ValidateProvisionDocument([]byte(nativeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateProtocols([]byte(raw), template)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decode(updated)
	if err != nil || cfg.InferenceProfiles["finder"].Protocol != "anthropic" {
		t.Fatalf("missing protocol not repaired: %v", err)
	}
}

func TestProtocolUpdateAcknowledgementTracksExactConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(compatibilityConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AcknowledgeProtocolUpdate(path); err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || needs {
		t.Fatalf("declined update prompts again: needs=%v error=%v", needs, err)
	}
	ack, err := os.ReadFile(path + ".protocol-update-ack")
	if err != nil || len(ack) != 65 || strings.Contains(string(ack), "secret") {
		t.Fatalf("invalid or secret-bearing acknowledgement: %v", err)
	}
	info, err := os.Stat(path + ".protocol-update-ack")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("acknowledgement is not private: %v", err)
	}
	if err := os.WriteFile(path, []byte(compatibilityConfig+"# user edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if needs, err := NeedsProvision(path); err != nil || !needs {
		t.Fatalf("config edit did not invalidate acknowledgement: needs=%v error=%v", needs, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nativeTemplate())) }))
	defer server.Close()
	if err := AcknowledgeProtocolUpdate(path); err != nil {
		t.Fatal(err)
	}
	if err := Provision(t.Context(), server.URL, path, ""); err != nil {
		t.Fatalf("acknowledgement blocked explicit config init: %v", err)
	}
}
