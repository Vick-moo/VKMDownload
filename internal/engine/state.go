package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// File suffixes for the working set. The sidecar sits next to the destination
// rather than in a global state directory so that a download is fully
// self-describing: move the pair together and resuming still works.
const (
	PartSuffix  = ".vkmdownload.part"
	StateSuffix = ".vkmdownload.json"
)

// StateVersion is bumped whenever the on-disk layout changes, so that a stale
// sidecar written by an older build is ignored rather than misread.
const StateVersion = 1

// State is the resumable description of an in-flight download.
type State struct {
	Version int    `json:"version"`
	URL     string `json:"url"`
	// FinalURL is the URL after redirects, which is what range requests must
	// be replayed against.
	FinalURL string `json:"final_url"`

	Size int64 `json:"size"`
	// ETag and LastModified are the validators captured at probe time. They
	// are re-checked on resume, and ETag is the preferred If-Range value.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	// NoRange records that the server does not support byte ranges, which
	// makes resume impossible no matter what the segment table says.
	NoRange bool `json:"no_range,omitempty"`

	Segments []SegState `json:"segments"`

	TempPath   string `json:"temp_path"`
	TargetPath string `json:"target_path"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SegState is one persisted segment.
type SegState struct {
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	Written int64 `json:"written"`
	Done    bool  `json:"done"`
}

// Validator returns the best available If-Range / resume validator, preferring
// ETag because it is unambiguous, and falling back to Last-Modified.
func (st *State) Validator() string {
	if st.ETag != "" {
		return st.ETag
	}
	return st.LastModified
}

// saveState writes the state atomically: a temporary file is written and then
// renamed over the target. A crash therefore leaves either the old complete
// sidecar or the new complete one, never a half-written file that would fail
// to parse on resume.
//
// The temporary name carries the pid plus an atomic counter (the same scheme
// moveFile uses) rather than a fixed ".tmp" suffix. Two writers used to share
// one temporary path, so their writes interleaved, the rename raced, and the
// loser left a half-written file behind that failed to parse — or silently
// replaced the winner's good sidecar. Any failure removes the temporary file so
// no debris is left for the next run to trip over.
func saveState(path string, st *State) error {
	st.UpdatedAt = time.Now()
	// Compact JSON: the sidecar is machine state rewritten many times a
	// second at high throughput, not a document humans edit.
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.vkmdownload-%d-%d.tmp", path, os.Getpid(), nextTmpID())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Keep the transient file hidden too: Explorer should not flash a helper
	// artefact in the user's download folder.
	hideFile(tmp)
	// On Windows os.Rename maps to MoveFileEx with MOVEFILE_REPLACE_EXISTING,
	// so this does replace an existing sidecar.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	hideFile(path)
	return nil
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	if st.Version != StateVersion {
		return nil, fmt.Errorf("state file %s has version %d, want %d", path, st.Version, StateVersion)
	}
	return &st, nil
}

// matches reports whether a persisted state is still usable for the freshly
// probed resource.
//
// The checks are deliberately strict. A validator or length change means the
// bytes already on disk belong to a different representation of the resource,
// and stitching them together with new bytes would produce a corrupt file that
// looks perfectly plausible.
//
// A validator that was absent before and present now (or the reverse) is treated
// as a mismatch rather than as "nothing to compare". The cost of being wrong is
// asymmetric: discarding a usable partial file costs one re-download, while
// trusting a stale one publishes a file with a hole in it that has exactly the
// right length.
func (st *State) matches(p *ProbeResult) bool {
	if st.Size != p.Size {
		return false
	}
	// The validators and the identity both have to agree. A different URL (or
	// a changed redirect target) with the same length and no validators would
	// otherwise let one resource's bytes resume into another.
	if st.FinalURL != "" && p.FinalURL != "" {
		if st.FinalURL != p.FinalURL {
			return false
		}
	} else if st.URL != p.URL {
		return false
	}
	if st.ETag != p.ETag {
		return false
	}
	if st.LastModified != p.LastModified {
		return false
	}
	return true
}
