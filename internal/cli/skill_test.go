package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillInstallAndStatusCommands(t *testing.T) {
	parent := t.TempDir()
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"skill", "install", "--dir", parent}, &stdout, &stderr)
	if err != nil || code != ExitClean {
		t.Fatalf("install code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), filepath.Join(parent, "roast")) {
		t.Fatalf("install output = %q", stdout.String())
	}

	stdout.Reset()
	code, err = ExecuteContext(t.Context(), []string{"skill", "status", "--dir", parent}, &stdout, &stderr)
	if err != nil || code != ExitClean || !strings.HasPrefix(stdout.String(), "custom\tup_to_date\t") {
		t.Fatalf("status code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
}
