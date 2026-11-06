package runupload

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/config"
)

const autoUploadTimeout = 20 * time.Second

// MaybeAutoUpload performs the opt-in, best-effort post-run upload. It never
// changes review output or exit status; failures are left pending for sync.
func MaybeAutoUpload(cfg config.Config, recordDir, runID string) {
	if !cfg.Upload.Enabled || recordDir == "" || !validRunID(runID) {
		return
	}
	if err := ensureEnrolledExcept(recordDir, runID); err != nil {
		markSetupFailure(recordDir, runID, uploadDestination(cfg), err)
		return
	}
	resolved, err := cfg.ResolveUpload()
	if err != nil {
		markSetupFailure(recordDir, runID, uploadDestination(cfg), err)
		return
	}
	store, err := NewStore(resolved)
	if err != nil {
		markSetupFailure(recordDir, runID, uploadDestination(cfg), err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoUploadTimeout)
	defer cancel()
	_ = uploadRun(ctx, store, recordDir, runID, false)
}

func uploadDestination(cfg config.Config) string {
	if cfg.Upload.Backend == "presigned" {
		return strings.TrimSuffix(cfg.Upload.Endpoint, "/")
	}
	return strings.TrimSuffix(cfg.Upload.Endpoint, "/") + "/" + cfg.Upload.Bucket + "/" + strings.Trim(cfg.Upload.Prefix, "/")
}

func markSetupFailure(recordDir, runID, destination string, cause error) {
	previous, _ := readState(recordDir, runID)
	_ = pendingOutcome(recordDir, runID, previous, destination, fmt.Errorf("initialize upload: %w", cause))
}
