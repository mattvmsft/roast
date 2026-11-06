package record

import (
	"fmt"
	"os"
	"path/filepath"
)

// makeRunDir protects the managed runs subtree, including directories created
// by older versions. Do not chmod the configured data root or its ancestors:
// ROAST_RECORD_DIR may name an existing directory shared with other programs.
func makeRunDir(dir, runID string) (string, error) {
	if runID == "" || filepath.Base(runID) != runID || runID == "." {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	root := filepath.Join(dir, "runs")
	runDir := filepath.Join(root, runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", fmt.Errorf("create run directory %s: %w", runDir, err)
	}
	for _, path := range []string{root, runDir} {
		if err := os.Chmod(path, 0o700); err != nil {
			return "", fmt.Errorf("protect run directory %s: %w", path, err)
		}
	}
	return runDir, nil
}

// openPrivateFile also tightens existing files before writing. Creation modes
// alone do not change the permissions of artifacts from older versions.
func openPrivateFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func writePrivateFile(path string, data []byte) error {
	f, err := openPrivateFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
