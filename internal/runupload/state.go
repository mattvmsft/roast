package runupload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Status string

const (
	StatusPending              Status = "pending"
	StatusUploaded             Status = "uploaded"
	StatusSkippedPreEnrollment Status = "skipped_pre_enrollment"
	stateDirName                      = "upload-state"
)

// State is local outbox bookkeeping. It lives outside the immutable run
// directory so it can never enter or invalidate bundle.tar.zst.
type State struct {
	Status      Status    `json:"status"`
	Destination string    `json:"destination,omitempty"`
	ObjectKey   string    `json:"object_key,omitempty"`
	ETag        string    `json:"etag,omitempty"`
	VersionID   string    `json:"version_id,omitempty"`
	Digest      string    `json:"digest,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	Attempts    int       `json:"attempts,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func validRunID(runID string) bool {
	return runID != "" && filepath.Base(runID) == runID && runID != "."
}

func statePath(recordDir, runID string) (string, error) {
	if !validRunID(runID) {
		return "", fmt.Errorf("invalid run id %q", runID)
	}
	return filepath.Join(recordDir, stateDirName, runID+".json"), nil
}

func readState(recordDir, runID string) (*State, error) {
	path, err := statePath(recordDir, runID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func writeState(recordDir, runID string, state *State) error {
	path, err := statePath(recordDir, runID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	state.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+runID+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
