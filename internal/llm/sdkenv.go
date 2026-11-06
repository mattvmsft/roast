package llm

import (
	"net/http"
	"os"
	"strings"

	"github.com/charmbracelet/openai-go/option"
)

// The provider SDKs under Fantasy read ambient configuration whenever a client
// is constructed (DefaultClientOptions in anthropic-sdk-go and openai-go), and
// Fantasy does not opt out. Roast's resolved configuration is the only source
// of request credentials, headers, and base URLs, so New removes what that
// environment can add. Fantasy's explicit API key, base URL, and User-Agent
// options apply after the SDK defaults: the configured key wins over
// ANTHROPIC_API_KEY, OPENAI_API_KEY, and OPENAI_ADMIN_KEY, the configured base
// URL over both *_BASE_URL variables and a profile's base_url, and each also
// replaces a custom header with its name. captureTransport sets Roast's
// session header last.

// sdkEnvironmentHeaders returns the headers that protocol's SDK can add from
// the environment and that Roast never configures for it:
//
//   - anthropic: Authorization (ANTHROPIC_AUTH_TOKEN; Roast authenticates
//     Anthropic requests with X-Api-Key alone), Anthropic-Workspace-Id (an
//     Anthropic profile selected by ANTHROPIC_PROFILE or the default profile,
//     or ANTHROPIC_WORKSPACE_ID filling one), and every header named in
//     ANTHROPIC_CUSTOM_HEADERS except X-Api-Key and User-Agent.
//   - chat and responses: OpenAI-Organization (OPENAI_ORG_ID), OpenAI-Project
//     (OPENAI_PROJECT_ID), and every header named in OPENAI_CUSTOM_HEADERS
//     except Authorization and User-Agent.
//
// A header named in a *_CUSTOM_HEADERS variable is removed even if the SDK
// also sets it, because the request no longer records which value came from
// the environment.
func sdkEnvironmentHeaders(protocol string) []string {
	names := []string{"OpenAI-Organization", "OpenAI-Project"}
	custom, credential := "OPENAI_CUSTOM_HEADERS", "Authorization"
	if protocol == "anthropic" {
		names = []string{"Authorization", "Anthropic-Workspace-Id"}
		custom, credential = "ANTHROPIC_CUSTOM_HEADERS", "X-Api-Key"
	}
	// Both SDKs parse one "Name: value" pair per line.
	for _, line := range strings.Split(os.Getenv(custom), "\n") {
		name, _, ok := strings.Cut(line, ":")
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if ok && name != credential && name != "User-Agent" {
			names = append(names, name)
		}
	}
	return names
}

// deleteHeaders returns openai-go options that run after the SDK's environment
// defaults and Fantasy's own options.
func deleteHeaders(names []string) []option.RequestOption {
	opts := make([]option.RequestOption, 0, len(names))
	for _, name := range names {
		opts = append(opts, option.WithHeaderDel(name))
	}
	return opts
}
