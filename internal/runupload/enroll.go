package runupload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const enrollmentFile = "upload-enrolled.json"

type enrollment struct {
	EnrolledAt time.Time `json:"enrolled_at"`
}

// EnsureEnrolled establishes the consent boundary for manual synchronization:
// runs which predate opt-in are not uploaded unless --include-existing is used.
func EnsureEnrolled(recordDir string) error {
	return ensureEnrolled(recordDir, "")
}

func ensureEnrolledExcept(recordDir, currentRun string) error {
	return ensureEnrolled(recordDir, currentRun)
}

func ensureEnrolled(recordDir, currentRun string) error {
	marker := filepath.Join(recordDir, enrollmentFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat upload enrollment: %w", err)
	}

	entries, err := os.ReadDir(filepath.Join(recordDir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("list run directories: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == currentRun {
			continue
		}
		state, err := readState(recordDir, entry.Name())
		if err != nil || state != nil {
			continue
		}
		if err := writeState(recordDir, entry.Name(), &State{
			Status: StatusSkippedPreEnrollment,
			Reason: "run predates upload opt-in; pass --include-existing to roast runs sync to upload it",
		}); err != nil {
			return fmt.Errorf("mark pre-existing run %s: %w", entry.Name(), err)
		}
	}

	data, err := json.Marshal(enrollment{EnrolledAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(recordDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(recordDir, ".upload-enrolled-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, marker); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
