package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadResolvesNamedProfilesAndRoleEnvironment(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  primary:
    endpoint: https://finder.example/v1
    api_key_env: FINDER_PROFILE_KEY
    model: finder-file
    protocol: chat
  verify:
    model: verifier-file
roles:
  finder:
    profile: primary
  verifier:
    profile: verify
review:
  confidence_floor: 0.7
`)
	t.Setenv("FINDER_PROFILE_KEY", "profile-key")
	t.Setenv("ROAST_FINDER_MODEL", "finder-env")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if finder.Profile != "primary" || finder.Model != "finder-env" || finder.APIKey != "profile-key" || finder.BaseURL != "https://finder.example/v1" || finder.Protocol != "chat" {
		t.Fatalf("finder role = %#v", finder)
	}
	verifier, err := cfg.ResolveRole("verifier")
	if err != nil {
		t.Fatal(err)
	}
	if verifier.Model != "verifier-file" || verifier.APIKey != "openai-key" || verifier.Protocol != "responses" {
		t.Fatalf("verifier role = %#v", verifier)
	}
	if cfg.Review.ConfidenceFloor != 0.7 {
		t.Fatalf("confidence floor = %v", cfg.Review.ConfidenceFloor)
	}
	if len(cfg.InferenceProfiles) != 2 {
		t.Fatalf("profiles = %#v, want exactly the two configured profiles", cfg.InferenceProfiles)
	}
}

func TestResolveRoleUsesNativeAnthropicDefaults(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  claude:
    model: claude-test
    protocol: anthropic
roles:
  finder:
    profile: claude
`)
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.BaseURL != "https://api.anthropic.com/v1" || role.APIKey != "anthropic-key" || role.Protocol != "anthropic" {
		t.Fatalf("finder role = %#v", role)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRoleIgnoresLegacyGlobalOverrides(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  finder:
    model: finder-model
roles:
  finder:
    profile: finder
`)
	t.Setenv("ROAST_API_KEY", "wrong-key")
	t.Setenv("ROAST_BASE_URL", "https://wrong.example/v1")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_BASE_URL", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "openai-key" || role.BaseURL != DefaultBaseURL("responses") {
		t.Fatalf("legacy globals changed role: %#v", role)
	}
}

func TestLoadRejectsProfileCredentialAmbiguity(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  bad:
    model: model
    api_key: literal
    api_key_env: MODEL_KEY
roles:
  finder:
    profile: bad
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected ambiguous credential configuration to be rejected")
	}
}

func TestValidateRoleRejectsEmbeddedCredentials(t *testing.T) {
	err := ValidateRole("finder", ResolvedRole{
		Profile: "finder", BaseURL: "https://user:password@example.com/v1", APIKey: "key",
		Model: "model", Protocol: "chat", MaxOutputTokens: 100,
	})
	if err == nil {
		t.Fatal("expected embedded credentials to be rejected")
	}
}

func TestResolveFinderFlipSelectsNamedProfile(t *testing.T) {
	path := writeConfig(t, `version: 1
inference_profiles:
  primary:
    endpoint: https://openai.example/v1
    api_key_env: OPENAI_TEST_KEY
    model: gpt-author
    protocol: responses
    max_output_tokens: 1000
  independent:
    endpoint: https://anthropic.example/v1
    api_key_env: ANTHROPIC_TEST_KEY
    model: claude-reviewer
    protocol: anthropic
    max_output_tokens: 2000
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
`)
	t.Setenv("OPENAI_TEST_KEY", "openai-key")
	t.Setenv("ANTHROPIC_TEST_KEY", "anthropic-key")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.ResolveFinderFlip(finder, "openai/gpt-author")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Profile != "independent" || got.Role.Model != "claude-reviewer" || got.Role.Protocol != "anthropic" || got.Role.APIKey != "anthropic-key" || got.Role.MaxOutputTokens != 2000 {
		t.Fatalf("resolved role = %#v", got.Role)
	}
	if got.Note == "" || got.Warn != "" {
		t.Fatalf("note=%q warn=%q", got.Note, got.Warn)
	}
}

func TestResolveFinderFlipRejectsSameFamilyTarget(t *testing.T) {
	cfg := Config{InferenceProfiles: map[string]InferenceProfile{
		"same": {Model: "o3-reviewer", Protocol: "responses", BaseURL: defaultOpenAIBaseURL, MaxOutputTokens: 100},
	}}
	finder := ResolvedRole{Model: "gpt-finder", AuthorRoutes: map[string]string{"openai": "same"}}
	got, err := cfg.ResolveFinderFlip(finder, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Model != finder.Model || got.Warn == "" {
		t.Fatalf("result = %#v", got)
	}
}

func TestResolveFinderFlipSelectsExactAuthorRoute(t *testing.T) {
	cfg := Config{InferenceProfiles: map[string]InferenceProfile{
		"bar": {Model: "claude-reviewer", Protocol: "anthropic", BaseURL: defaultAnthropicBaseURL, MaxOutputTokens: 100},
	}}
	finder := ResolvedRole{
		Model: "gpt-default", AuthorRoutes: map[string]string{"foo-model": "bar"},
	}
	got, err := cfg.ResolveFinderFlip(finder, "FOO-MODEL")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role.Profile != "bar" || got.Role.Model != "claude-reviewer" || got.Role.Protocol != "anthropic" {
		t.Fatalf("result = %#v", got)
	}
}

func TestInferFamily(t *testing.T) {
	for input, want := range map[string]string{
		"anthropic/claude-sonnet": "anthropic",
		"gpt-5":                   "openai",
		"gemini-2.5-pro":          "google",
		"mistral-large":           "mistral",
		"grok-4":                  "xai",
	} {
		if got := InferFamily(input); got != want {
			t.Errorf("InferFamily(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadAndResolveS3CompatibleUpload(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  enabled: true
  backend: s3
  endpoint: https://objects.example.test/
  bucket: roast-data
  prefix: team/runs/
  region: auto
  access_key_env: TEST_UPLOAD_ACCESS
  secret_key_env: TEST_UPLOAD_SECRET
  session_token_env: TEST_UPLOAD_SESSION
  path_style: true
`)
	t.Setenv("TEST_UPLOAD_ACCESS", "access")
	t.Setenv("TEST_UPLOAD_SECRET", "secret")
	t.Setenv("TEST_UPLOAD_SESSION", "session")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveUpload()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "https://objects.example.test" || resolved.Bucket != "roast-data" || resolved.Prefix != "team/runs" || resolved.Region != "auto" || !resolved.PathStyle {
		t.Fatalf("resolved upload = %#v", resolved)
	}
	if resolved.AccessKey != "access" || resolved.SecretKey != "secret" || resolved.SessionToken != "session" {
		t.Fatal("resolved upload credentials do not match their explicit environment sources")
	}
}

func TestLoadAndResolvePresignedUpload(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  enabled: true
  backend: presigned
  endpoint: https://uploads.example.test/
  token_env: TEST_BROKER_TOKEN
`)
	t.Setenv("TEST_BROKER_TOKEN", "broker-token")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveUpload()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Backend != "presigned" || resolved.Endpoint != "https://uploads.example.test" || resolved.Token != "broker-token" {
		t.Fatalf("resolved upload = %#v", resolved)
	}
}

func TestUploadNeverUsesProviderCredentialChain(t *testing.T) {
	path := writeConfig(t, `version: 1
upload:
  endpoint: https://objects.example.test
  bucket: roast-data
`)
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("ROAST_UPLOAD_ACCESS_KEY", "")
	t.Setenv("ROAST_UPLOAD_SECRET_KEY", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveUpload(); err == nil {
		t.Fatal("expected explicit Roast upload credentials; AWS environment credentials must not be consulted")
	}
}

func TestLoadMigratesUnversionedNamedProfiles(t *testing.T) {
	path := writeConfig(t, `inference_profiles:
  old:
    base_url: https://draft.example/v1
    model: draft-model
    protocol: chat
roles:
  finder:
    profile: old
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.InferenceProfiles["old"].BaseURL != "https://draft.example/v1" {
		t.Fatalf("migrated config = %#v", cfg)
	}
}

func TestLoadMigratesLegacyInlineRoles(t *testing.T) {
	path := writeConfig(t, `gateway:
  base_url: https://legacy-gateway.example/v1
  api_key: shared-legacy-key
upload:
  enabled: true
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
roles:
  finder:
    model: gpt-finder
    api: chat
    max_output_tokens: 1234
    author_flip:
      openai:
        model: claude-reviewer
        api: anthropic
  verifier:
    model: verifier-model
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if finder.Profile != "finder" || finder.BaseURL != "https://legacy-gateway.example/v1" || finder.APIKey != "shared-legacy-key" || finder.Protocol != "chat" || finder.MaxOutputTokens != 1234 {
		t.Fatalf("migrated finder = %#v", finder)
	}
	routeName := finder.AuthorRoutes["openai"]
	route := cfg.InferenceProfiles[routeName]
	if route.Model != "claude-reviewer" || route.Protocol != "anthropic" || route.BaseURL != "https://legacy-gateway.example/v1" || route.APIKey != "shared-legacy-key" || route.MaxOutputTokens != 1234 {
		t.Fatalf("migrated author route %q = %#v", routeName, route)
	}
	if cfg.Upload.Enabled {
		t.Fatal("legacy upload enrollment must not enable a new upload destination")
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"auto_install", "sourcebot", "retired-search.example", "discarded-retrieval-key"} {
		if strings.Contains(string(raw), retired) {
			t.Errorf("retired setting %q survived migration", retired)
		}
	}
}

func TestLoadRecognizesLegacyConfigWithoutExplicitRoles(t *testing.T) {
	path := writeConfig(t, `gateway:
  api_key: unused-legacy-key
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != CurrentVersion || cfg.Upload.Enabled {
		t.Fatalf("unexpected migrated config: %#v", cfg)
	}
}

func TestLoadLegacyConfigStillRejectsUnknownFields(t *testing.T) {
	for _, suffix := range []string{"unknown: true\n", "gate:\n  auto_instal: false\n", "sourcebot:\n  tokne: invalid\n"} {
		if _, err := Load(writeConfig(t, "gateway:\n  api_key: legacy-key\n"+suffix)); err == nil {
			t.Fatalf("unknown legacy setting was accepted: %q", suffix)
		}
	}
}

func TestLoadEnforcesVersionedSchema(t *testing.T) {
	for name, body := range map[string]string{
		"future version":       "version: 2\n",
		"zero version":         "version: 0\n",
		"old field in v1":      "version: 1\ninference_profiles:\n  old:\n    base_url: https://example.test/v1\n",
		"retired gate in v1":   "version: 1\ngate:\n  auto_install: false\n",
		"retired search in v1": "version: 1\nsourcebot:\n  url: https://retired-search.example\n",
		"multiple documents":   "version: 1\n---\nversion: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected config to be rejected")
			}
		})
	}
}

func TestLoadRejectsInvalidUploadConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"enabled without destination": "upload:\n  enabled: true\n",
		"plaintext remote endpoint":   "upload:\n  endpoint: http://objects.example.test\n  bucket: b\n",
		"endpoint path":               "upload:\n  endpoint: https://objects.example.test/base\n  bucket: b\n",
		"credential in endpoint":      "upload:\n  endpoint: https://user:secret@objects.example.test\n  bucket: b\n",
		"escaping prefix":             "upload:\n  endpoint: https://objects.example.test\n  bucket: b\n  prefix: ../runs\n",
		"bad env name":                "upload:\n  endpoint: https://objects.example.test\n  bucket: b\n  access_key_env: not-valid!\n",
		"presigned with bucket":       "upload:\n  backend: presigned\n  endpoint: https://uploads.example.test\n  bucket: b\n",
		"s3 with token":               "upload:\n  backend: s3\n  endpoint: https://objects.example.test\n  bucket: b\n  token_env: TOKEN\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected invalid upload configuration to be rejected")
			}
		})
	}
}

func TestLoadAllowsExplicitLocalPlaintextUpload(t *testing.T) {
	for name, body := range map[string]string{
		"loopback":               "upload:\n  endpoint: http://127.0.0.1:9000\n  bucket: b\n",
		"trusted network opt in": "upload:\n  endpoint: http://minio:9000\n  bucket: b\n  allow_insecure_http: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Fatalf("expected local plaintext endpoint to be accepted: %v", err)
			}
		})
	}
}
