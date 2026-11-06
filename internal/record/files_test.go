package record

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/review"
)

func TestPrivateRecordPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not describe Windows ACLs")
	}
	const runID = "permissions-test"
	tests := []struct {
		name  string
		file  string
		write func(*testing.T)
	}{
		{"metadata", MetaFile, func(t *testing.T) {
			if _, err := WriteMetaRequired(Meta{RunID: runID}); err != nil {
				t.Fatal(err)
			}
		}},
		{"input", "input.gz", func(t *testing.T) {
			if _, err := WriteArtifactRequired(runID, "input", "private input"); err != nil {
				t.Fatal(err)
			}
		}},
		{"report", "report.md", func(t *testing.T) {
			if _, err := WriteReportRequired(runID, "private report"); err != nil {
				t.Fatal(err)
			}
		}},
		{"responses", ProviderResponsesFile, func(t *testing.T) {
			if err := NewResponseRecorder(runID).Append("finder", "responses", "model", "application/json", []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		}},
		{"findings", "findings.jsonl", func(t *testing.T) {
			spill := OpenFindingSpill(runID)
			if spill == nil {
				t.Fatal("no finding sink")
			}
			spill(review.WireFinding{})
		}},
	}
	for _, tt := range tests {
		for _, existing := range []bool{false, true} {
			name := "new"
			if existing {
				name = "existing"
			}
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("ROAST_RECORD_DIR", dir)
				// An explicitly configured root may belong to other programs.
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(dir, "runs")
				runDir := filepath.Join(root, runID)
				path := filepath.Join(runDir, tt.file)
				if existing {
					if err := os.MkdirAll(runDir, 0o755); err != nil {
						t.Fatal(err)
					}
					for _, dir := range []string{root, runDir} {
						if err := os.Chmod(dir, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					for _, path := range []string{path, path + ".tmp"} {
						if err := os.WriteFile(path, []byte("old artifact\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						if err := os.Chmod(path, 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				tt.write(t)
				assertMode(t, path, 0o600)
				assertMode(t, runDir, 0o700)
				assertMode(t, root, 0o700)
				assertMode(t, dir, 0o755)
			})
		}
	}
}

func TestPrivateMonthlyRecordPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not describe Windows ACLs")
	}
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	path := filepath.Join(dir, "runs-"+time.Now().Format("2006-01")+".jsonl")
	Record(makeReport(nil), "/repo", nil)
	assertMode(t, path, 0o600)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	Record(makeReport(nil), "/repo", nil)
	assertMode(t, path, 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
