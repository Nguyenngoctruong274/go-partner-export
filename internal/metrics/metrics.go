// Package metrics is a small Prometheus-compatible registry: counters,
// gauges and histograms with labels, exposed in the text format. It has
// no dependency because the service needs a dozen series, not a client
// library, and because the exposition format is simple enough to own.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics and renders them.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	name() string
	write(w io.Writer)
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

func (r *Registry) register(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.metrics {
		if existing.name() == m.name() {
			panic("metrics: duplicate metric " + m.name())
		}
	}
	r.metrics = append(r.metrics, m)
}

// Handler serves the registry in the Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// Write renders every metric.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// labelSet is a sorted, rendered label set used as a map key.
type labelSet string

func newLabelSet(names, values []string) labelSet {
	if len(names) != len(values) {
		panic(fmt.Sprintf("metrics: %d label names but %d values", len(names), len(values)))
	}
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i := range names {
		parts[i] = names[i] + `="` + escape(values[i]) + `"`
	}
	return labelSet(strings.Join(parts, ","))
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func sortedKeys[V any](m map[labelSet]V) []labelSet {
	keys := make([]labelSet, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func writeHeader(w io.Writer, name, help, typ string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeSample(w io.Writer, name string, ls labelSet, suffix string, v float64) {
	if ls == "" {
		_, _ = fmt.Fprintf(w, "%s%s %s\n", name, suffix, format(v))
		return
	}
	_, _ = fmt.Fprintf(w, "%s%s{%s} %s\n", name, suffix, ls, format(v))
}

func format(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

// Counter only goes up.
type Counter struct {
	n, help string
	labels  []string
	mu      sync.Mutex
	values  map[labelSet]float64
}

// NewCounter registers a counter with the given label names.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{n: name, help: help, labels: labels, values: map[labelSet]float64{}}
	r.register(c)
	return c
}

// Add increases the series by v (v >= 0).
func (c *Counter) Add(v float64, labelValues ...string) {
	if v < 0 {
		panic("metrics: counter decreased")
	}
	ls := newLabelSet(c.labels, labelValues)
	c.mu.Lock()
	c.values[ls] += v
	c.mu.Unlock()
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Value returns the series' current value.
func (c *Counter) Value(labelValues ...string) float64 {
	ls := newLabelSet(c.labels, labelValues)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[ls]
}

func (c *Counter) name() string { return c.n }

func (c *Counter) write(w io.Writer) {
	writeHeader(w, c.n, c.help, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ls := range sortedKeys(c.values) {
		writeSample(w, c.n, ls, "", c.values[ls])
	}
}

// Gauge can go up and down.
type Gauge struct {
	n, help string
	labels  []string
	mu      sync.Mutex
	values  map[labelSet]float64
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{n: name, help: help, labels: labels, values: map[labelSet]float64{}}
	r.register(g)
	return g
}

// Set replaces the series' value.
func (g *Gauge) Set(v float64, labelValues ...string) {
	ls := newLabelSet(g.labels, labelValues)
	g.mu.Lock()
	g.values[ls] = v
	g.mu.Unlock()
}

// Value returns the series' current value.
func (g *Gauge) Value(labelValues ...string) float64 {
	ls := newLabelSet(g.labels, labelValues)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.values[ls]
}

func (g *Gauge) name() string { return g.n }

func (g *Gauge) write(w io.Writer) {
	writeHeader(w, g.n, g.help, "gauge")
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ls := range sortedKeys(g.values) {
		writeSample(w, g.n, ls, "", g.values[ls])
	}
}

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	n, help string
	labels  []string
	bounds  []float64
	mu      sync.Mutex
	series  map[labelSet]*histSeries
}

type histSeries struct {
	counts []uint64 // per bound, non-cumulative
	sum    float64
	count  uint64
}

// NewHistogram registers a histogram with the given upper bounds (sorted
// ascending; +Inf is added).
func (r *Registry) NewHistogram(name, help string, bounds []float64, labels ...string) *Histogram {
	if !sort.Float64sAreSorted(bounds) {
		panic("metrics: histogram bounds must be sorted")
	}
	h := &Histogram{n: name, help: help, labels: labels, bounds: bounds, series: map[labelSet]*histSeries{}}
	r.register(h)
	return h
}

// Observe records v.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	ls := newLabelSet(h.labels, labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[ls]
	if s == nil {
		s = &histSeries{counts: make([]uint64, len(h.bounds))}
		h.series[ls] = s
	}
	for i, b := range h.bounds {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// Count returns how many observations the series has.
func (h *Histogram) Count(labelValues ...string) uint64 {
	ls := newLabelSet(h.labels, labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.series[ls]; s != nil {
		return s.count
	}
	return 0
}

func (h *Histogram) name() string { return h.n }

func (h *Histogram) write(w io.Writer) {
	writeHeader(w, h.n, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ls := range sortedKeys(h.series) {
		s := h.series[ls]
		var cum uint64
		for i, b := range h.bounds {
			cum += s.counts[i]
			writeSample(w, h.n, withLE(ls, format(b)), "_bucket", float64(cum))
		}
		writeSample(w, h.n, withLE(ls, "+Inf"), "_bucket", float64(s.count))
		writeSample(w, h.n, ls, "_sum", s.sum)
		writeSample(w, h.n, ls, "_count", float64(s.count))
	}
}

func withLE(ls labelSet, le string) labelSet {
	if ls == "" {
		return labelSet(`le="` + le + `"`)
	}
	return ls + labelSet(`,le="`+le+`"`)
}
