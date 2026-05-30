/*
Copyright 2026 The Hearth Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package gateway is the Hearth data plane: an OpenAI-compatible reverse proxy that
// sits in front of one LLMService. It buffers requests while the backend is cold,
// applies bounded-queue backpressure, and exposes the pending-request count as the
// demand signal the scaler turns into a KEDA scale-from-zero decision.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Environment variables read by ConfigFromEnv (and set by the operator-rendered Deployment).
const (
	EnvBackendURL        = "HEARTH_BACKEND_URL"
	EnvMaxQueue          = "HEARTH_MAX_QUEUE"
	EnvActivationTimeout = "HEARTH_ACTIVATION_TIMEOUT"
	EnvListenAddr        = "HEARTH_LISTEN_ADDR"

	DefaultListenAddr = ":8080"
	QueuePath         = "/hearth/queue"
	MetricsPath       = "/metrics"
)

type Config struct {
	BackendURL        string
	MaxQueue          int
	ActivationTimeout time.Duration
	RetryInterval     time.Duration
}

func ConfigFromEnv() Config {
	cfg := Config{BackendURL: os.Getenv(EnvBackendURL)}
	if v, err := strconv.Atoi(os.Getenv(EnvMaxQueue)); err == nil {
		cfg.MaxQueue = v
	}
	if d, err := time.ParseDuration(os.Getenv(EnvActivationTimeout)); err == nil {
		cfg.ActivationTimeout = d
	}
	return cfg
}

type metrics struct {
	registry   *prometheus.Registry
	pending    prometheus.Gauge
	requests   *prometheus.CounterVec
	rejections *prometheus.CounterVec
	coldStart  prometheus.Histogram
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hearth_gateway_pending", Help: "Requests admitted and waiting or in flight (the scaler's demand signal)."}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hearth_gateway_requests_total", Help: "Responses by HTTP status code."}, []string{"code"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hearth_gateway_rejections_total", Help: "Rejected requests by reason."}, []string{"reason"}),
		coldStart: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "hearth_gateway_activation_wait_seconds", Help: "Time spent holding a request until the backend was ready.",
			Buckets: []float64{0.01, 0.1, 1, 5, 15, 30, 60, 120, 300}}),
	}
	m.registry.MustRegister(m.pending, m.requests, m.rejections, m.coldStart)
	return m
}

type Gateway struct {
	cfg     Config
	backend *url.URL
	proxy   *httputil.ReverseProxy
	sem     chan struct{}
	pending atomic.Int64
	m       *metrics
	probe   *http.Client
	now     func() time.Time
}

func New(cfg Config) (*Gateway, error) {
	u, err := url.Parse(cfg.BackendURL)
	if err != nil {
		return nil, err
	}
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 100
	}
	if cfg.ActivationTimeout <= 0 {
		cfg.ActivationTimeout = 5 * time.Minute
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 500 * time.Millisecond
	}
	return &Gateway{
		cfg:     cfg,
		backend: u,
		proxy:   httputil.NewSingleHostReverseProxy(u),
		sem:     make(chan struct{}, cfg.MaxQueue),
		m:       newMetrics(),
		probe:   &http.Client{Timeout: 2 * time.Second},
		now:     time.Now,
	}, nil
}

func (g *Gateway) Pending() int64 { return g.pending.Load() }

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc(QueuePath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"pending": g.pending.Load()})
	})
	mux.Handle(MetricsPath, promhttp.HandlerFor(g.m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", g.serve)
	return mux
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	// Bounded-queue backpressure: reject rather than buffer to OOM.
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	default:
		g.m.rejections.WithLabelValues("queue_full").Inc()
		g.m.requests.WithLabelValues("429").Inc()
		w.Header().Set("Retry-After", "5")
		http.Error(w, "gateway queue full", http.StatusTooManyRequests)
		return
	}

	// Demand signal for the scaler, raised for the whole hold-and-serve window.
	g.m.pending.Set(float64(g.pending.Add(1)))
	defer func() { g.m.pending.Set(float64(g.pending.Add(-1))) }()

	waitStart := g.now()
	if !g.waitForBackend(r.Context()) {
		g.m.rejections.WithLabelValues("activation_timeout").Inc()
		g.m.requests.WithLabelValues("503").Inc()
		w.Header().Set("Retry-After", "10")
		http.Error(w, "backend not ready (activation timeout)", http.StatusServiceUnavailable)
		return
	}
	g.m.coldStart.Observe(g.now().Sub(waitStart).Seconds())

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	g.proxy.ServeHTTP(rec, r)
	g.m.requests.WithLabelValues(strconv.Itoa(rec.status)).Inc()
}

// waitForBackend blocks until the backend is ready, the request is canceled, or the
// activation timeout elapses (cold-start hold).
func (g *Gateway) waitForBackend(ctx context.Context) bool {
	deadline := g.now().Add(g.cfg.ActivationTimeout)
	for {
		if g.backendReady(ctx) {
			return true
		}
		if g.now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(g.cfg.RetryInterval):
		}
	}
}

func (g *Gateway) backendReady(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.backend.String()+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := g.probe.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// statusRecorder captures the proxied response status for metrics while passing
// through streaming (SSE) flushes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
