// Package metrics is the Prometheus endpoint of the API and the worker (API
// 0.61.0, FR-056, FR-065): request rate, errors and latency, the outbox
// backlog and dead letters, and the health of the background sweeps.
//
// It writes the Prometheus text exposition format itself (no client
// library): counters and histograms kept in this process, and gauges read
// when the endpoint is scraped. The endpoint is served on its own listener
// (METRICS_ADDR), never on the public API port, so the ingress never routes
// to it.
package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBuckets are the latency histogram buckets, in seconds.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Labels are label pairs, in a fixed order per family.
type Labels []string

func (l Labels) key() string { return strings.Join(l, "\x00") }

type counterFamily struct {
	help   string
	names  []string
	values map[string]float64
	labels map[string]Labels
}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

type histogramFamily struct {
	help    string
	names   []string
	buckets []float64
	values  map[string]*histogram
	labels  map[string]Labels
}

// GaugeFunc reads gauge samples at scrape time.
type GaugeFunc func(ctx context.Context) ([]Sample, error)

// Sample is one gauge value.
type Sample struct {
	Labels map[string]string
	Value  float64
}

type gaugeFamily struct {
	help string
	read GaugeFunc
}

// Registry holds the families of one process.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*counterFamily
	histograms map[string]*histogramFamily
	gauges     map[string]*gaugeFamily
	logger     *slog.Logger
}

// NewRegistry returns an empty registry.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{counters: map[string]*counterFamily{}, histograms: map[string]*histogramFamily{}, gauges: map[string]*gaugeFamily{}, logger: logger}
}

// Counter declares a counter family with its label names.
func (r *Registry) Counter(name, help string, labelNames ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.counters[name]; !ok {
		r.counters[name] = &counterFamily{help: help, names: labelNames, values: map[string]float64{}, labels: map[string]Labels{}}
	}
}

// Add adds v to a counter (declared with Counter; the values follow its label names).
func (r *Registry) Add(name string, v float64, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.counters[name]
	if !ok || len(labelValues) != len(f.names) {
		return
	}
	k := Labels(labelValues).key()
	f.values[k] += v
	f.labels[k] = append(Labels{}, labelValues...)
}

// Histogram declares a histogram family.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.histograms[name]; !ok {
		r.histograms[name] = &histogramFamily{help: help, names: labelNames, buckets: buckets, values: map[string]*histogram{}, labels: map[string]Labels{}}
	}
}

// Observe records one value in a histogram.
func (r *Registry) Observe(name string, v float64, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.histograms[name]
	if !ok || len(labelValues) != len(f.names) {
		return
	}
	k := Labels(labelValues).key()
	h := f.values[k]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(f.buckets))}
		f.values[k] = h
		f.labels[k] = append(Labels{}, labelValues...)
	}
	for i, b := range f.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// Gauge declares a gauge family read at scrape time.
func (r *Registry) Gauge(name, help string, read GaugeFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = &gaugeFamily{help: help, read: read}
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func labelText(names []string, values []string, extra ...string) string {
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, n, escape(values[i])))
	}
	parts = append(parts, extra...)
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Write writes every family in the text exposition format. A gauge whose
// read fails is left out (and logged); the others are still written.
func (r *Registry) Write(ctx context.Context, w io.Writer) error {
	r.mu.Lock()
	var b strings.Builder
	for _, name := range sortedKeys(r.counters) {
		f := r.counters[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, f.help, name)
		for _, k := range sortedKeys(f.values) {
			fmt.Fprintf(&b, "%s%s %s\n", name, labelText(f.names, f.labels[k]), num(f.values[k]))
		}
	}
	for _, name := range sortedKeys(r.histograms) {
		f := r.histograms[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, f.help, name)
		for _, k := range sortedKeys(f.values) {
			h, lv := f.values[k], f.labels[k]
			for i, bound := range f.buckets {
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name, labelText(f.names, lv, fmt.Sprintf(`le="%s"`, num(bound))), h.counts[i])
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", name, labelText(f.names, lv, `le="+Inf"`), h.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n%s_count%s %d\n", name, labelText(f.names, lv), num(h.sum), name, labelText(f.names, lv), h.count)
		}
	}
	gauges := make(map[string]*gaugeFamily, len(r.gauges))
	for k, v := range r.gauges {
		gauges[k] = v
	}
	r.mu.Unlock()

	for _, name := range sortedKeys(gauges) {
		g := gauges[name]
		samples, err := g.read(ctx)
		if err != nil {
			r.logger.WarnContext(ctx, "metrics: a gauge could not be read", slog.String("metric", name), slog.String("error", err.Error()))
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, g.help, name)
		for _, s := range samples {
			names := sortedKeys(s.Labels)
			values := make([]string, len(names))
			for i, n := range names {
				values[i] = s.Labels[n]
			}
			fmt.Fprintf(&b, "%s%s %s\n", name, labelText(names, values), num(s.Value))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves the registry (text/plain; version=0.0.4).
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := r.Write(ctx, w); err != nil {
			r.logger.WarnContext(ctx, "metrics: write failed", slog.String("error", err.Error()))
		}
	})
}

// ---------------------------------------------------------------------------
// The families every process uses
// ---------------------------------------------------------------------------

// Names.
const (
	HTTPRequests        = "rp365_http_requests_total"
	HTTPDuration        = "rp365_http_request_duration_seconds"
	SweepRuns           = "rp365_sweep_runs_total"
	SweepLastSuccess    = "rp365_sweep_last_success_timestamp_seconds"
	SweepLastRunSeconds = "rp365_sweep_last_run_duration_seconds"
)

// sweepState is the last outcome of each sweep, read by a gauge.
type sweepState struct {
	mu          sync.Mutex
	lastSuccess map[string]time.Time
	lastRun     map[string]float64
}

// Process is the registry of one binary with the shared families declared.
type Process struct {
	*Registry
	sweeps *sweepState
	now    func() time.Time
}

// NewProcess declares the HTTP and sweep families.
func NewProcess(logger *slog.Logger) *Process {
	r := NewRegistry(logger)
	r.Counter(HTTPRequests, "HTTP requests, by method, route pattern and status class.", "method", "route", "status")
	r.Histogram(HTTPDuration, "HTTP request latency in seconds, by method and route pattern.", DefaultBuckets, "method", "route")
	r.Counter(SweepRuns, "Background sweep runs, by sweep and result (ok or error).", "sweep", "result")
	p := &Process{Registry: r, sweeps: &sweepState{lastSuccess: map[string]time.Time{}, lastRun: map[string]float64{}}, now: time.Now}
	r.Gauge(SweepLastSuccess, "When each sweep last succeeded (Unix seconds).", func(context.Context) ([]Sample, error) {
		p.sweeps.mu.Lock()
		defer p.sweeps.mu.Unlock()
		out := []Sample{}
		for _, name := range sortedKeys(p.sweeps.lastSuccess) {
			out = append(out, Sample{Labels: map[string]string{"sweep": name}, Value: float64(p.sweeps.lastSuccess[name].Unix())})
		}
		return out, nil
	})
	r.Gauge(SweepLastRunSeconds, "How long each sweep's last run took, in seconds.", func(context.Context) ([]Sample, error) {
		p.sweeps.mu.Lock()
		defer p.sweeps.mu.Unlock()
		out := []Sample{}
		for _, name := range sortedKeys(p.sweeps.lastRun) {
			out = append(out, Sample{Labels: map[string]string{"sweep": name}, Value: p.sweeps.lastRun[name]})
		}
		return out, nil
	})
	return p
}

// SweepDone records one run of a sweep. A nil Process does nothing.
func (p *Process) SweepDone(name string, started time.Time, err error) {
	if p == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	p.Add(SweepRuns, 1, name, result)
	now := p.now()
	p.sweeps.mu.Lock()
	defer p.sweeps.mu.Unlock()
	p.sweeps.lastRun[name] = now.Sub(started).Seconds()
	if err == nil {
		p.sweeps.lastSuccess[name] = now
	}
}

// RouteFunc returns the route pattern of a served request ("" when none
// matched): chi's RoutePattern, never the raw path (bounded labels).
type RouteFunc func(r *http.Request) string

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Middleware counts every request and its latency. route is read after the
// request was served (the router has matched by then).
func (p *Process) Middleware(route RouteFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := p.now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			pattern := route(r)
			if pattern == "" {
				pattern = "unmatched"
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			method := r.Method
			switch method {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
			default:
				method = "OTHER"
			}
			p.Add(HTTPRequests, 1, method, pattern, strconv.Itoa(status/100)+"xx")
			p.Observe(HTTPDuration, p.now().Sub(start).Seconds(), method, pattern)
		})
	}
}

// Serve runs the metrics listener until ctx ends. addr "" disables it.
func Serve(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", h)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("metrics listener started (not public: keep it off the ingress)", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("metrics listener stopped", slog.String("error", err.Error()))
	}
}
