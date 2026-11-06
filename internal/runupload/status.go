package runupload

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type RunSummary struct {
	RunID  string
	Status string
	Detail string
}

func ListStatus(recordDir string) ([]RunSummary, error) {
	entries, err := os.ReadDir(filepath.Join(recordDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var summaries []RunSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := readState(recordDir, entry.Name())
		summary := RunSummary{RunID: entry.Name()}
		switch {
		case err != nil:
			summary.Status, summary.Detail = "corrupt_state", err.Error()
		case state == nil:
			summary.Status = "never_attempted"
		default:
			summary.Status, summary.Detail = string(state.Status), stateDetail(state)
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].RunID < summaries[j].RunID })
	return summaries, nil
}

func stateDetail(state *State) string {
	switch state.Status {
	case StatusUploaded:
		return fmt.Sprintf("destination=%s object=%s", state.Destination, state.ObjectKey)
	case StatusPending:
		return fmt.Sprintf("destination=%s attempts=%d last_error=%s", state.Destination, state.Attempts, state.LastError)
	case StatusSkippedPreEnrollment:
		return state.Reason
	default:
		return ""
	}
}
