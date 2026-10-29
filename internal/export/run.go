package export

import "time"

// State is how far a run got. A run moves forward through the stages and
// stops at Failed with the stage that broke; a re-run of the day starts
// over from Pending.
type State string

// Run states, in pipeline order.
const (
	StatePending  State = "pending"  // row created, nothing done yet
	StateBuilt    State = "built"    // rows selected and the CSV written
	StateArchived State = "archived" // copy stored in the blob store, link signed
	StatePushed   State = "pushed"   // delivered to the partner's SFTP
	StateFailed   State = "failed"   // stopped; Stage and Error say where and why
)

// Stages name the pipeline steps, for Run.Stage and for metrics.
const (
	StageSelect  = "select"
	StageBuild   = "build"
	StageArchive = "archive"
	StageSign    = "sign"
	StagePush    = "push"
	StageRecord  = "record"
)

// Run is the record of one export of one day.
type Run struct {
	Day  Day
	From time.Time
	To   time.Time
	// State is the last completed stage, or Failed.
	State State
	// Stage is the stage that failed when State is Failed; otherwise the
	// last stage that completed.
	Stage string
	Error string
	Rows  int
	Bytes int64
	// FileName is the partner-facing name; BlobKey the archived copy;
	// DownloadURL the signed link to it; RemotePath where it landed on SFTP.
	FileName    string
	BlobKey     string
	DownloadURL string
	RemotePath  string
	// Attempts counts how many times this day was run.
	Attempts   int
	StartedAt  time.Time
	FinishedAt time.Time // zero while the run is in progress
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateBuilt, StateArchived, StatePushed, StateFailed:
		return true
	}
	return false
}

// Final reports whether the run is over.
func (s State) Final() bool {
	return s == StatePushed || s == StateFailed
}
