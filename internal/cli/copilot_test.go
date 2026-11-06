package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/config"
)

func TestCopilotSharedAndSeparateModelFlags(t *testing.T) {
	for _, test := range []struct {
		name, shared, finder, verifier string
		wantFinder, wantVerifier       string
	}{
		{name: "defaults"},
		{name: "both", shared: "shared", wantFinder: "shared", wantVerifier: "shared"},
		{name: "finder", finder: "finder", wantFinder: "finder"},
		{name: "verifier", verifier: "verifier", wantVerifier: "verifier"},
		{name: "separate", finder: "finder", verifier: "verifier", wantFinder: "finder", wantVerifier: "verifier"},
		{name: "precedence", shared: "shared", finder: "finder", wantFinder: "finder", wantVerifier: "shared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"review", "--copilot"}
			for flag, value := range map[string]string{"--model": test.shared, "--finder-model": test.finder, "--verifier-model": test.verifier} {
				if value != "" {
					args = append(args, flag, value)
				}
			}
			var flags reviewFlags
			reviewCmd := &cobra.Command{Use: "review"}
			addReviewFlags(reviewCmd, &flags)
			if err := reviewCmd.ParseFlags(args[1:]); err != nil {
				t.Fatal(err)
			}
			changed := reviewCmd.Flags().Changed
			applySharedFlags(changed, &flags)
			cfg := config.Defaults()
			for name, roleFlags := range map[string]roleFlags{"finder": flags.finder, "verifier": flags.verifier} {
				role, err := cfg.ResolveRole(name)
				if err != nil {
					t.Fatal(err)
				}
				role = applyRoleFlags(cfg, sharedRoleChanged(changed, &flags), name, role, roleFlags)
				want := test.wantFinder
				if name == "verifier" {
					want = test.wantVerifier
				}

				if role.Protocol != "copilot" || role.Model != want || role.APIKey != "" || role.BaseURL != "" {
					t.Fatalf("%s role = %+v, want model %q", name, role, want)
				}
				if err := config.ValidateRole(name, role); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAPIProtocolFlagRestoresCredentialAfterCopilotEnv(t *testing.T) {
	for _, name := range []string{"finder", "verifier"} {
		for _, test := range []struct {
			name, profileProtocol, protocol, endpoint, key, keyEnv, roleKey, wantKey string
			envEndpoint, flagEndpoint                                                string
		}{
			{name: "inline", profileProtocol: "responses", protocol: "responses", key: "profile-key", wantKey: "profile-key"},
			{name: "environment", profileProtocol: "responses", protocol: "chat", keyEnv: "TEST_PROFILE_KEY", wantKey: "profile-env-key"},
			{name: "anthropic", profileProtocol: "anthropic", protocol: "anthropic", key: "anthropic-profile-key", wantKey: "anthropic-profile-key"},
			{name: "explicit openai default", profileProtocol: "responses", protocol: "responses", endpoint: config.DefaultBaseURL("responses"), key: "profile-key", wantKey: "profile-key"},
			{name: "explicit anthropic default", profileProtocol: "anthropic", protocol: "anthropic", endpoint: config.DefaultBaseURL("anthropic"), key: "profile-key", wantKey: "profile-key"},
			{name: "role precedence", profileProtocol: "responses", protocol: "responses", key: "profile-key", roleKey: "role-key", wantKey: "role-key"},
			{name: "shared gateway different protocol", profileProtocol: "responses", protocol: "anthropic", key: "gateway-key", wantKey: "gateway-key"},
			{name: "different public provider", profileProtocol: "responses", protocol: "anthropic", endpoint: config.DefaultBaseURL("responses"), key: "must-not-follow"},
			{name: "environment gateway", profileProtocol: "responses", protocol: "anthropic", endpoint: config.DefaultBaseURL("responses"), envEndpoint: "https://env-gateway.example/v1", key: "gateway-key", wantKey: "gateway-key"},
			{name: "environment public endpoint", profileProtocol: "responses", protocol: "anthropic", envEndpoint: config.DefaultBaseURL("responses"), roleKey: "role-key", wantKey: "role-key"},
			{name: "environment anthropic endpoint", profileProtocol: "anthropic", protocol: "responses", envEndpoint: config.DefaultBaseURL("anthropic"), roleKey: "role-key", wantKey: "role-key"},
			{name: "flag gateway", profileProtocol: "responses", protocol: "anthropic", endpoint: config.DefaultBaseURL("responses"), flagEndpoint: "https://flag-gateway.example/v1", key: "gateway-key", wantKey: "gateway-key"},
			{name: "native copilot", profileProtocol: "copilot", protocol: "responses", key: "must-not-follow"},
		} {
			t.Run(name+"/"+test.name, func(t *testing.T) {
				t.Setenv("ROAST_"+strings.ToUpper(name)+"_PROTOCOL", "copilot")
				t.Setenv("ROAST_"+strings.ToUpper(name)+"_API_KEY", test.roleKey)
				t.Setenv("ROAST_"+strings.ToUpper(name)+"_BASE_URL", test.envEndpoint)
				t.Setenv("OPENAI_API_KEY", "")
				t.Setenv("ANTHROPIC_API_KEY", "")
				t.Setenv("OPENAI_BASE_URL", "")
				t.Setenv("ANTHROPIC_BASE_URL", "")
				t.Setenv("TEST_PROFILE_KEY", "profile-env-key")
				cfg := config.Defaults()
				profile := cfg.InferenceProfiles[name]
				profile.Protocol, profile.Model = test.profileProtocol, "model"
				if test.profileProtocol != "copilot" {
					profile.BaseURL = "https://gateway.example/v1"
				}
				if test.endpoint != "" {
					profile.BaseURL = test.endpoint
				}
				profile.APIKey, profile.APIKeyEnv = test.key, test.keyEnv
				cfg.InferenceProfiles[name] = profile
				role, err := cfg.ResolveRole(name)
				if err != nil {
					t.Fatal(err)
				}
				if role.APIKey != "" || role.Protocol != "copilot" {
					t.Fatalf("Copilot env selection not applied: %+v", role)
				}
				changed := func(flag string) bool {
					return flag == name+"-protocol" || (test.flagEndpoint != "" && flag == name+"-base-url")
				}
				flags := roleFlags{protocol: test.protocol, baseURL: test.flagEndpoint}
				role = applyRoleFlags(cfg, changed, name, role, flags)
				if role.Protocol != test.protocol || role.APIKey != test.wantKey {
					t.Fatalf("API flag lost credential precedence: %+v", role)
				}
				t.Setenv("ROAST_"+strings.ToUpper(name)+"_PROTOCOL", "")
				direct, err := cfg.ResolveRole(name)
				if err != nil {
					t.Fatal(err)
				}
				direct = applyRoleFlags(cfg, changed, name, direct, flags)
				if role.BaseURL != direct.BaseURL || role.APIKey != direct.APIKey || role.Credential != direct.Credential {
					t.Fatalf("Copilot transition changed API routing: got %+v, direct API override %+v", role, direct)
				}
				if test.wantKey != "" {
					if err := config.ValidateRole(name, role); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
