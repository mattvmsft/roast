package context

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListFilesGlobstarMatchesEveryDepth(t *testing.T) {
	// A repository, because the live corpus is Git's view of the working tree.
	repo := initRepo(t)
	for _, name := range []string{"root_test.go", "one/one_test.go", "one/two/two_test.go", "one/two/code.go"} {
		full := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result := listFilesTool(repo, map[string]any{"pattern": "**/*_test.go"}, Tree{})
	matches, ok := result["matches"].([]string)
	if !ok {
		t.Fatalf("matches = %#v", result["matches"])
	}
	for _, want := range []string{"root_test.go", "one/one_test.go", "one/two/two_test.go"} {
		if !slices.Contains(matches, want) {
			t.Errorf("matches %v do not contain %q", matches, want)
		}
	}
	if slices.Contains(matches, "one/two/code.go") {
		t.Errorf("matches unexpectedly contain non-test file: %v", matches)
	}
}
