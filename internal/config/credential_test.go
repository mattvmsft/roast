package config

import (
	"fmt"
	"strings"
	"testing"
)

const (
	ambientGateway = "https://ambient-gateway.example/v1"
	customEndpoint = "https://template-endpoint.example/v1"
)

// TestCredentialAndEndpointPrecedence enumerates every combination of
// profile endpoint, profile credential, ambient provider base URL, role-scoped
// key, and protocol, and checks each against the documented rule.
func TestCredentialAndEndpointPrecedence(t *testing.T) {
	type expectation struct {
		endpoint string
		key      string
		kind     string
	}
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		keyVar, baseVar := providerAPIKeyEnv(protocol), providerBaseURLEnv(protocol)
		for _, endpoint := range []string{"unset", "default", "custom", "ambient"} {
			for _, credential := range []string{"none", "api_key_env", "api_key"} {
				for _, ambientBase := range []bool{false, true} {
					for _, roleKey := range []bool{false, true} {
						if endpoint == "ambient" && !ambientBase {
							continue
						}
						name := fmt.Sprintf("%s/endpoint=%s/credential=%s/%s=%v/role_key=%v", protocol, endpoint, credential, baseVar, ambientBase, roleKey)
						t.Run(name, func(t *testing.T) {
							clearProviderEnv(t)
							t.Setenv(keyVar, "ambient-key")
							t.Setenv("PROFILE_KEY", "profile-key")
							if ambientBase {
								t.Setenv(baseVar, ambientGateway)
							}
							if roleKey {
								t.Setenv("ROAST_FINDER_API_KEY", "role-key")
							}
							profile := fmt.Sprintf("    model: m\n    protocol: %s\n", protocol)
							switch endpoint {
							case "default":
								profile += "    endpoint: " + DefaultBaseURL(protocol) + "\n"
							case "custom":
								profile += "    endpoint: " + customEndpoint + "\n"
							case "ambient":
								profile += "    endpoint: " + ambientGateway + "\n"
							}
							switch credential {
							case "api_key_env":
								profile += "    api_key_env: PROFILE_KEY\n"
							case "api_key":
								profile += "    api_key: literal-key\n"
							}
							role := resolveFinderProfile(t, profile)

							var want expectation
							switch endpoint {
							case "unset":
								want.endpoint = DefaultBaseURL(protocol)
								if ambientBase {
									want.endpoint = ambientGateway
								}
							case "default":
								want.endpoint = DefaultBaseURL(protocol)
							case "custom":
								want.endpoint = customEndpoint
							case "ambient":
								want.endpoint = ambientGateway
							}
							switch {
							case roleKey:
								want.key, want.kind = "role-key", CredentialRoleEnv
							case credential == "api_key_env":
								want.key, want.kind = "profile-key", CredentialProfileEnv
							case credential == "api_key":
								want.key, want.kind = "literal-key", CredentialProfileKey
							case endpoint != "custom":
								// Default endpoint, or the base URL the same
								// environment pairs with the key.
								want.key, want.kind = "ambient-key", CredentialProviderEnv
							default:
								want.kind = CredentialNone
							}
							if role.BaseURL != want.endpoint || role.APIKey != want.key || role.Credential.Kind != want.kind {
								t.Fatalf("got endpoint=%q key=%q kind=%q, want endpoint=%q key=%q kind=%q",
									role.BaseURL, role.APIKey, role.Credential.Kind, want.endpoint, want.key, want.kind)
							}
							if want.kind == CredentialNone && role.Credential.Withheld != keyVar {
								t.Fatalf("withheld = %q, want %q", role.Credential.Withheld, keyVar)
							}
						})
					}
				}
			}
		}
	}
}

func TestTemplateProfileWithoutCredentialNeverReceivesAmbientKey(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "personal-anthropic-key")
	t.Setenv("OPENAI_API_KEY", "personal-openai-key")
	t.Setenv("ANTHROPIC_BASE_URL", ambientGateway)
	for _, protocol := range []string{"anthropic", "responses"} {
		role := resolveFinderProfile(t, fmt.Sprintf("    endpoint: %s\n    model: m\n    protocol: %s\n", customEndpoint, protocol))
		if role.BaseURL != customEndpoint || role.APIKey != "" {
			t.Fatalf("%s: endpoint=%q key=%q", protocol, role.BaseURL, role.APIKey)
		}
		err := ValidateRole("finder", role)
		if err == nil || !strings.Contains(err.Error(), "is not sent to "+customEndpoint) || !strings.Contains(err.Error(), "ROAST_FINDER_API_KEY") {
			t.Fatalf("%s: unexpected validation error %v", protocol, err)
		}
	}
}

func TestRoleEnvironmentOverrides(t *testing.T) {
	for name, test := range map[string]struct {
		env          map[string]string
		profile      string
		wantEndpoint string
		wantProtocol string
		wantKey      string
	}{
		"role base URL keeps profile credential": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "PROFILE_KEY": "profile-key", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "profile-key",
		},
		"role base URL never receives ambient key": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "",
		},
		"role base URL with role key": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "ROAST_FINDER_API_KEY": "role-key", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "role-key",
		},
		"role base URL wins over ambient base URL": {
			env:          map[string]string{"ROAST_FINDER_BASE_URL": "https://role.example/v1", "OPENAI_BASE_URL": ambientGateway, "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: "https://role.example/v1", wantProtocol: "responses", wantKey: "",
		},
		"protocol override crosses providers from the default endpoint": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_API_KEY": "anthropic", "OPENAI_API_KEY": "openai"},
			profile:      "    model: m\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"profile credential does not follow a provider crossing": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "PROFILE_KEY": "openai-profile", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"provider crossing then ambient anthropic base URL": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_BASE_URL": ambientGateway, "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n",
			wantEndpoint: ambientGateway, wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"protocol override on a custom endpoint keeps the profile credential": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "PROFILE_KEY": "gateway", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "gateway",
		},
		"protocol override on a custom endpoint never sends an ambient key": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "anthropic", "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "",
		},
		"protocol override within one provider keeps the profile credential": {
			env:          map[string]string{"ROAST_FINDER_PROTOCOL": "chat", "PROFILE_KEY": "profile", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: DefaultBaseURL("chat"), wantProtocol: "chat", wantKey: "profile",
		},
		"configured endpoint ignores ambient base URL": {
			env:          map[string]string{"ANTHROPIC_BASE_URL": ambientGateway, "PROFILE_KEY": "profile"},
			profile:      "    model: m\n    protocol: anthropic\n    endpoint: " + customEndpoint + "\n    api_key_env: PROFILE_KEY\n",
			wantEndpoint: customEndpoint, wantProtocol: "anthropic", wantKey: "profile",
		},
		"ambient base URL is protocol scoped": {
			env:          map[string]string{"OPENAI_BASE_URL": ambientGateway, "ANTHROPIC_API_KEY": "anthropic"},
			profile:      "    model: m\n    protocol: anthropic\n",
			wantEndpoint: DefaultBaseURL("anthropic"), wantProtocol: "anthropic", wantKey: "anthropic",
		},
		"endpoint comparison ignores a trailing slash": {
			env:          map[string]string{"OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n    endpoint: " + DefaultBaseURL("responses") + "/\n",
			wantEndpoint: DefaultBaseURL("responses") + "/", wantProtocol: "responses", wantKey: "ambient",
		},
		"role model and output limit": {
			env:          map[string]string{"ROAST_FINDER_MODEL": "env-model", "ROAST_FINDER_MAX_OUTPUT_TOKENS": "77", "OPENAI_API_KEY": "ambient"},
			profile:      "    model: m\n",
			wantEndpoint: DefaultBaseURL("responses"), wantProtocol: "responses", wantKey: "ambient",
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearProviderEnv(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			role := resolveFinderProfile(t, test.profile)
			if role.BaseURL != test.wantEndpoint || role.Protocol != test.wantProtocol || role.APIKey != test.wantKey {
				t.Fatalf("got endpoint=%q protocol=%q key=%q source=%v", role.BaseURL, role.Protocol, role.APIKey, role.Credential)
			}
			if name == "role model and output limit" && (role.Model != "env-model" || role.MaxOutputTokens != 77) {
				t.Fatalf("role overrides not applied: %#v", role)
			}
		})
	}
}

func TestInvalidRoleMaxOutputTokensIsRejected(t *testing.T) {
	cfg, err := Load(writeConfig(t, `version: 1
inference_profiles:
  primary:
    model: gpt-primary
  independent:
    model: claude-reviewer
    protocol: anthropic
  verifier:
    model: v
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
  verifier:
    profile: verifier
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"lots", "0", "-5", "1.5", "1e3", " 7"} {
		t.Run(value, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("ROAST_FINDER_MAX_OUTPUT_TOKENS", value)
			_, err := cfg.ResolveRole("finder")
			if err == nil || !strings.Contains(err.Error(), "ROAST_FINDER_MAX_OUTPUT_TOKENS must be an integer >= 1") || !strings.Contains(err.Error(), fmt.Sprintf("%q", value)) {
				t.Fatalf("ResolveRole error = %v", err)
			}
			if _, err := cfg.ResolveFinderFlip(ResolvedRole{Model: "gpt-finder", AuthorRoutes: map[string]string{"openai": "independent"}}, "gpt-author"); err == nil {
				t.Fatal("author route resolution ignored the invalid override")
			}
			if _, err := cfg.ResolveRole("verifier"); err != nil {
				t.Fatalf("finder override affected the verifier: %v", err)
			}
		})
	}
	clearProviderEnv(t)
	t.Setenv("ROAST_FINDER_MAX_OUTPUT_TOKENS", "4096")
	role, err := cfg.ResolveRole("finder")
	if err != nil || role.MaxOutputTokens != 4096 {
		t.Fatalf("valid override: tokens=%d err=%v", role.MaxOutputTokens, err)
	}
}

func TestEnvironmentOnlyDefaultsHonorProviderBaseURL(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_BASE_URL", ambientGateway)
	t.Setenv("OPENAI_API_KEY", "ambient")
	t.Setenv("ROAST_FINDER_MODEL", "m")
	cfg, err := Load(writeConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if role.BaseURL != ambientGateway || role.APIKey != "ambient" || role.Credential.Kind != CredentialProviderEnv {
		t.Fatalf("environment-only role = %#v", role)
	}
	if cfg.InferenceProfiles["finder"].BaseURL != "" {
		t.Fatal("normalization must not turn an unset endpoint into a configured one")
	}
}

func TestAuthorRouteProfileUsesTheSameCredentialRule(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", ambientGateway)
	t.Setenv("ANTHROPIC_API_KEY", "ambient")
	cfg, err := Load(writeConfig(t, `version: 1
inference_profiles:
  primary:
    model: gpt-primary
  independent:
    endpoint: `+customEndpoint+`
    model: claude-reviewer
    protocol: anthropic
  verifier:
    model: v
roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
  verifier:
    profile: verifier
`))
	if err != nil {
		t.Fatal(err)
	}
	finder, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := cfg.ResolveFinderFlip(finder, "gpt-author")
	if err != nil {
		t.Fatal(err)
	}
	if flip.Role.Profile != "independent" || flip.Role.BaseURL != customEndpoint || flip.Role.APIKey != "" {
		t.Fatalf("author route = %#v", flip.Role)
	}
}

// TestEndpointOverridesMatchAcrossEnvironmentAndFlags checks that a command-line
// protocol or endpoint override lands on the same endpoint with the same
// credential as the equivalent ROAST_<ROLE>_* variable.
func TestEndpointOverridesMatchAcrossEnvironmentAndFlags(t *testing.T) {
	const gateway = "https://gateway.example/v1"
	for name, test := range map[string]struct {
		profile      string
		env          map[string]string
		protocol     string
		baseURL      string
		wantEndpoint string
		wantKey      string
	}{
		"unset endpoint follows the new provider's base URL": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "gateway-key"},
			protocol:     "anthropic",
			wantEndpoint: gateway, wantKey: "gateway-key",
		},
		"unset endpoint without a base URL goes to the provider default": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"unset endpoint leaves the previous provider's base URL": {
			profile:      "    model: m\n",
			env:          map[string]string{"OPENAI_BASE_URL": gateway, "OPENAI_API_KEY": "openai-key", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"explicit default endpoint with a trailing slash moves": {
			profile:      "    model: m\n    endpoint: " + DefaultBaseURL("responses") + "/\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: DefaultBaseURL("anthropic"), wantKey: "anthropic-key",
		},
		"explicit custom endpoint stays with its credential": {
			profile:      "    model: m\n    endpoint: " + customEndpoint + "\n    api_key_env: MY_GATEWAY\n",
			env:          map[string]string{"MY_GATEWAY": "gateway-profile", "ANTHROPIC_BASE_URL": gateway, "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol:     "anthropic",
			wantEndpoint: customEndpoint, wantKey: "gateway-profile",
		},
		"same provider protocol keeps the profile credential": {
			profile:      "    model: m\n    api_key_env: MY_OPENAI\n",
			env:          map[string]string{"MY_OPENAI": "openai-profile", "OPENAI_BASE_URL": gateway, "OPENAI_API_KEY": "openai-key"},
			protocol:     "chat",
			wantEndpoint: gateway, wantKey: "openai-profile",
		},
		"endpoint override with a protocol override keeps the profile credential": {
			profile:  "    model: m\n    api_key_env: MY_OPENAI\n",
			env:      map[string]string{"MY_OPENAI": "openai-profile", "ANTHROPIC_API_KEY": "anthropic-key"},
			protocol: "anthropic", baseURL: "https://chosen.example/v1",
			wantEndpoint: "https://chosen.example/v1", wantKey: "openai-profile",
		},
		"endpoint override never carries an ambient key": {
			profile:      "    model: m\n",
			env:          map[string]string{"OPENAI_API_KEY": "openai-key"},
			baseURL:      "https://chosen.example/v1",
			wantEndpoint: "https://chosen.example/v1", wantKey: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolve := func(path string) ResolvedRole {
				clearProviderEnv(t)
				for key, value := range test.env {
					t.Setenv(key, value)
				}
				var baseURL, protocol *string
				if test.protocol != "" {
					protocol = &test.protocol
				}
				if test.baseURL != "" {
					baseURL = &test.baseURL
				}
				if path == "environment" {
					if protocol != nil {
						t.Setenv("ROAST_FINDER_PROTOCOL", *protocol)
					}
					if baseURL != nil {
						t.Setenv("ROAST_FINDER_BASE_URL", *baseURL)
					}
					return resolveFinderProfile(t, test.profile)
				}
				return OverrideEndpoint("finder", resolveFinderProfile(t, test.profile), baseURL, protocol)
			}
			fromEnv, fromFlags := resolve("environment"), resolve("flags")
			for path, role := range map[string]ResolvedRole{"environment": fromEnv, "flags": fromFlags} {
				if role.BaseURL != test.wantEndpoint || role.APIKey != test.wantKey {
					t.Errorf("%s: endpoint=%q key=%q source=%v, want endpoint=%q key=%q", path, role.BaseURL, role.APIKey, role.Credential, test.wantEndpoint, test.wantKey)
				}
			}
			if fromEnv.BaseURL != fromFlags.BaseURL || fromEnv.Protocol != fromFlags.Protocol || fromEnv.APIKey != fromFlags.APIKey ||
				fromEnv.Credential != fromFlags.Credential || fromEnv.ExplicitEndpoint != fromFlags.ExplicitEndpoint {
				t.Fatalf("environment and flag overrides diverged:\n env:   %+v\n flags: %+v", fromEnv, fromFlags)
			}
		})
	}
}

func TestRebindCredential(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "openai")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic")
	ambient := ResolvedRole{BaseURL: DefaultBaseURL("responses"), Protocol: "responses", APIKey: "openai", Credential: CredentialSource{Kind: CredentialProviderEnv, Env: "OPENAI_API_KEY"}}
	profile := ResolvedRole{BaseURL: customEndpoint, Protocol: "responses", APIKey: "profile", Credential: CredentialSource{Kind: CredentialProfileEnv, Env: "PROFILE_KEY"}}
	for name, test := range map[string]struct {
		role     ResolvedRole
		endpoint string
		protocol string
		crossed  bool
		wantKey  string
	}{
		"ambient key to custom endpoint":     {ambient, customEndpoint, "responses", false, ""},
		"ambient key to other provider":      {ambient, DefaultBaseURL("anthropic"), "anthropic", true, "anthropic"},
		"ambient key protocol on gateway":    {ambient, DefaultBaseURL("responses"), "anthropic", false, ""},
		"profile key follows explicit flag":  {profile, "https://flag.example/v1", "responses", false, "profile"},
		"profile key stays with provider":    {profile, DefaultBaseURL("anthropic"), "anthropic", true, "anthropic"},
		"ambient key at the default address": {ambient, DefaultBaseURL("chat"), "chat", false, "openai"},
	} {
		t.Run(name, func(t *testing.T) {
			role := test.role
			role.BaseURL, role.Protocol = test.endpoint, test.protocol
			if got := rebindCredential("finder", role, test.crossed); got.APIKey != test.wantKey {
				t.Fatalf("key = %q from %v, want %q", got.APIKey, got.Credential, test.wantKey)
			}
		})
	}
}

func TestCredentialSourceNeverIncludesSecret(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("PROFILE_KEY", "super-secret-value")
	role := resolveFinderProfile(t, "    model: m\n    api_key_env: PROFILE_KEY\n")
	if got := role.Credential.String(); got != "PROFILE_KEY (profile api_key_env)" || strings.Contains(got, "super-secret-value") {
		t.Fatalf("credential source = %q", got)
	}
	literal := resolveFinderProfile(t, "    model: m\n    api_key: super-secret-value\n")
	if strings.Contains(literal.Credential.String(), "super-secret-value") {
		t.Fatal("literal api_key leaked into the credential description")
	}
}

// resolveFinderProfile resolves the finder role from one profile body.
func resolveFinderProfile(t *testing.T, profile string) ResolvedRole {
	t.Helper()
	cfg, err := Load(writeConfig(t, "version: 1\ninference_profiles:\n  finder:\n"+profile+"  verifier:\n    model: v\n"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	return role
}

// clearProviderEnv blanks the variables resolution reads so one subtest's
// settings cannot leak into the next.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
		"ROAST_FINDER_API_KEY", "ROAST_FINDER_BASE_URL", "ROAST_FINDER_PROTOCOL", "ROAST_FINDER_MODEL", "ROAST_FINDER_MAX_OUTPUT_TOKENS",
	} {
		t.Setenv(name, "")
	}
}
