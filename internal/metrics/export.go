package metrics

import (
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

// Export holds the series that describe the export pipeline. It
// implements pipeline.Observer.
type Export struct {
	StageDuration *Histogram // export_stage_duration_seconds{stage}
	StageFailures *Counter   // export_stage_failures_total{stage}
	Runs          *Counter   // export_runs_total{state}
	LastRows      *Gauge     // export_last_run_rows
	LastBytes     *Gauge     // export_last_run_bytes
	LastSuccess   *Gauge     // export_last_success_timestamp_seconds
	LastAttempts  *Gauge     // export_last_run_attempts
	now           func() time.Time
}

// NewExport registers the pipeline series in r.
func NewExport(r *Registry) *Export {
	return &Export{
		StageDuration: r.NewHistogram("export_stage_duration_seconds", "Time spent in each pipeline stage.",
			[]float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300}, "stage"),
		StageFailures: r.NewCounter("export_stage_failures_total", "Stage executions that returned an error.", "stage"),
		Runs:          r.NewCounter("export_runs_total", "Finished runs by final state.", "state"),
		LastRows:      r.NewGauge("export_last_run_rows", "Rows in the most recent run."),
		LastBytes:     r.NewGauge("export_last_run_bytes", "Bytes in the most recent run's file."),
		LastSuccess:   r.NewGauge("export_last_success_timestamp_seconds", "Unix time of the last pushed run; alert when it is older than a day plus slack."),
		LastAttempts:  r.NewGauge("export_last_run_attempts", "Attempts the most recent run took."),
		now:           time.Now,
	}
}

// StageDone implements pipeline.Observer.
func (e *Export) StageDone(stage string, d time.Duration, err error) {
	e.StageDuration.Observe(d.Seconds(), stage)
	if err != nil {
		e.StageFailures.Inc(stage)
	}
}

// RunDone implements pipeline.Observer.
func (e *Export) RunDone(r export.Run) {
	e.Runs.Inc(string(r.State))
	e.LastRows.Set(float64(r.Rows))
	e.LastBytes.Set(float64(r.Bytes))
	e.LastAttempts.Set(float64(r.Attempts))
	if r.State == export.StatePushed {
		e.LastSuccess.Set(float64(e.now().Unix()))
	}
}
