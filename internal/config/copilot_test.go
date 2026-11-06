package config

import "testing"

func TestCopilotDefaultsAndCredentials(t *testing.T) {
	cfg := Defaults()
	profile := cfg.InferenceProfiles["finder"]
	profile.Protocol = "copilot"
	profile.APIKey = "must-not-be-used"
	cfg.InferenceProfiles["finder"] = profile
	t.Setenv("OPENAI_API_KEY", "must-not-be-used")
	t.Setenv("OPENAI_BASE_URL", "https://must-not-be-used.example/v1")
	t.Setenv("ROAST_FINDER_API_KEY", "must-not-be-used")
	cfg, err := normalize(cfg)
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.Model != "" || role.BaseURL != "" || role.APIKey != "" || role.Credential.Kind != CredentialCopilot {
		t.Fatalf("unexpected default Copilot role: %+v", role)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROAST_FINDER_MODEL", "chosen-model")
	role, err = cfg.ResolveRole("finder")
	if err != nil || role.Model != "chosen-model" {
		t.Fatalf("model override: %+v, %v", role, err)
	}
}

func TestCopilotProtocolOverrides(t *testing.T) {
	cfg := Defaults()
	t.Setenv("OPENAI_API_KEY", "openai-key")
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	protocol := "copilot"
	role = OverrideEndpoint("finder", role, nil, &protocol)
	if role.APIKey != "" || role.BaseURL != "" || role.Credential.Kind != CredentialCopilot {
		t.Fatalf("API credentials followed Copilot override: %+v", role)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	protocol = "responses"
	role = OverrideEndpoint("finder", role, nil, &protocol)
	if role.APIKey != "openai-key" || role.BaseURL != defaultOpenAIBaseURL {
		t.Fatalf("API provider not restored: %+v", role)
	}
}

func TestCopilotRejectsEndpoint(t *testing.T) {
	cfg := Defaults()
	profile := cfg.InferenceProfiles["finder"]
	profile.Protocol, profile.BaseURL = "copilot", "https://example.com/v1"
	cfg.InferenceProfiles["finder"] = profile
	if _, err := normalize(cfg); err == nil {
		t.Fatal("Copilot profiles must not accept endpoints")
	}
}
