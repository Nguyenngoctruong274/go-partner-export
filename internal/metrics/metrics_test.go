package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCounterGaugeHistogramExposition(t *testing.T) {
	r := New()
	c := r.NewCounter("runs_total", "Runs by state.", "state")
	g := r.NewGauge("last_success_timestamp_seconds", "Unix time of the last success.")
	h := r.NewHistogram("stage_seconds", "Stage durations.", []float64{0.1, 1}, "stage")

	c.Inc("pushed")
	c.Add(2, "failed")
	c.Inc("pushed")
	g.Set(1793239323)
	h.Observe(0.05, "push")
	h.Observe(0.5, "push")
	h.Observe(5, "push")

	if c.Value("pushed") != 2 || c.Value("failed") != 2 || c.Value("none") != 0 {
		t.Errorf("counter values: pushed=%v failed=%v", c.Value("pushed"), c.Value("failed"))
	}
	if g.Value() != 1793239323 {
		t.Errorf("gauge = %v", g.Value())
	}
	if h.Count("push") != 3 || h.Count("other") != 0 {
		t.Errorf("histogram count = %d", h.Count("push"))
	}

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	want := `# HELP runs_total Runs by state.
# TYPE runs_total counter
runs_total{state="failed"} 2
runs_total{state="pushed"} 2
# HELP last_success_timestamp_seconds Unix time of the last success.
# TYPE last_success_timestamp_seconds gauge
last_success_timestamp_seconds 1793239323
# HELP stage_seconds Stage durations.
# TYPE stage_seconds histogram
stage_seconds_bucket{stage="push",le="0.1"} 1
stage_seconds_bucket{stage="push",le="1"} 2
stage_seconds_bucket{stage="push",le="+Inf"} 3
stage_seconds_sum{stage="push"} 5.55
stage_seconds_count{stage="push"} 3
`
	if got := rec.Body.String(); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelEscapingAndFloatFormat(t *testing.T) {
	r := New()
	c := r.NewCounter("c", "help", "msg")
	c.Add(0.5, `a "quoted" \ line`+"\n")
	var sb strings.Builder
	r.Write(&sb)
	if !strings.Contains(sb.String(), `c{msg="a \"quoted\" \\ line\n"} 0.5`) {
		t.Errorf("exposition:\n%s", sb.String())
	}
	if format(1e20) != "1e+20" || format(3) != "3" || format(-2.5) != "-2.5" {
		t.Errorf("format: %s %s %s", format(1e20), format(3), format(-2.5))
	}
}

func TestPanicsOnMisuse(t *testing.T) {
	r := New()
	r.NewCounter("dup", "")
	expectPanic(t, "duplicate", func() { r.NewGauge("dup", "") })
	expectPanic(t, "unsorted bounds", func() { r.NewHistogram("h", "", []float64{1, 0.1}) })
	c := r.NewCounter("c", "", "a")
	expectPanic(t, "label count", func() { c.Inc("x", "y") })
	expectPanic(t, "negative add", func() { c.Add(-1, "x") })
}

func expectPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", name)
		}
	}()
	fn()
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	c := r.NewCounter("c", "", "k")
	h := r.NewHistogram("h", "", []float64{1}, "k")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Inc("k" + string(rune('a'+i%3)))
				h.Observe(float64(j), "k")
			}
		}(i)
	}
	wg.Wait()
	if total := c.Value("ka") + c.Value("kb") + c.Value("kc"); total != 5000 {
		t.Errorf("total = %v", total)
	}
	if h.Count("k") != 5000 {
		t.Errorf("count = %d", h.Count("k"))
	}
}
