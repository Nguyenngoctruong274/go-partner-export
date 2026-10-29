package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

func TestExportObserver(t *testing.T) {
	r := New()
	e := NewExport(r)
	at := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return at }

	e.StageDone(export.StageSelect, 20*time.Millisecond, nil)
	e.StageDone(export.StagePush, 3*time.Second, errors.New("down"))
	e.RunDone(export.Run{State: export.StateFailed, Rows: 10, Bytes: 100, Attempts: 1})
	if e.LastSuccess.Value() != 0 {
		t.Error("failed run counted as a success")
	}
	e.StageDone(export.StagePush, 2*time.Second, nil)
	e.RunDone(export.Run{State: export.StatePushed, Rows: 12, Bytes: 120, Attempts: 2})

	if e.Runs.Value("failed") != 1 || e.Runs.Value("pushed") != 1 {
		t.Errorf("runs: failed=%v pushed=%v", e.Runs.Value("failed"), e.Runs.Value("pushed"))
	}
	if e.StageFailures.Value(export.StagePush) != 1 || e.StageFailures.Value(export.StageSelect) != 0 {
		t.Errorf("stage failures: %v", e.StageFailures.Value(export.StagePush))
	}
	if e.StageDuration.Count(export.StagePush) != 2 || e.StageDuration.Count(export.StageSelect) != 1 {
		t.Error("stage durations not observed")
	}
	if e.LastRows.Value() != 12 || e.LastBytes.Value() != 120 || e.LastAttempts.Value() != 2 {
		t.Errorf("last run gauges: rows=%v bytes=%v attempts=%v", e.LastRows.Value(), e.LastBytes.Value(), e.LastAttempts.Value())
	}
	if e.LastSuccess.Value() != float64(at.Unix()) {
		t.Errorf("last success = %v", e.LastSuccess.Value())
	}
	var sb strings.Builder
	r.Write(&sb)
	for _, want := range []string{
		`export_runs_total{state="pushed"} 1`,
		`export_stage_failures_total{stage="push"} 1`,
		`export_stage_duration_seconds_bucket{stage="push",le="2.5"} 1`,
		`export_last_success_timestamp_seconds 1793235600`,
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("exposition lacks %q:\n%s", want, sb.String())
		}
	}
}
