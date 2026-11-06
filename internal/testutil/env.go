package testutil

import (
	"os"
	"strings"
)

// ambientPrefixes are environment families that select inference endpoints,
// credentials, or Roast behavior, and that provider SDKs also read directly.
var ambientPrefixes = []string{"ANTHROPIC_", "OPENAI_", "ROAST_"}

// ClearAmbientEnvironment unsets every ANTHROPIC_*, OPENAI_*, and ROAST_*
// variable so tests never inherit a developer's endpoints, credentials, or
// Roast overrides. Call it from TestMain before m.Run; tests that need one of
// these variables set it with t.Setenv.
func ClearAmbientEnvironment() {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		for _, prefix := range ambientPrefixes {
			if strings.HasPrefix(strings.ToUpper(name), prefix) {
				_ = os.Unsetenv(name)
				break
			}
		}
	}
}
