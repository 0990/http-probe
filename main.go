package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultConfigPath    = "config.json"
	defaultListenAddr    = ":9108"
	defaultMetricsPath   = "/metrics"
	defaultProbeInterval = 30 * time.Second
	defaultProbeTimeout  = 20 * time.Second
	defaultMethod        = http.MethodPost
	maxResponseBodyBytes = 2 * 1024 * 1024
)

type config struct {
	ListenAddr    string         `json:"listen_addr"`
	MetricsPath   string         `json:"metrics_path"`
	ProbeInterval string         `json:"probe_interval"`
	ProbeTimeout  string         `json:"probe_timeout"`
	Targets       []targetConfig `json:"targets"`
}

type targetConfig struct {
	Name                 string            `json:"name"`
	Method               string            `json:"method"`
	URL                  string            `json:"url"`
	ProbeInterval        string            `json:"probe_interval"`
	ProbeTimeout         string            `json:"probe_timeout"`
	Headers              map[string]string `json:"headers"`
	Body                 json.RawMessage   `json:"body"`
	ExpectedBodyContains string            `json:"expected_body_contains"`
}

type runtimeConfig struct {
	ListenAddr  string
	MetricsPath string
	Targets     []target
}

type target struct {
	Name                 string
	Method               string
	URL                  string
	ProbeInterval        time.Duration
	ProbeTimeout         time.Duration
	Headers              map[string]string
	Body                 []byte
	ExpectedBodyContains string
}

type probeResult struct {
	up                 bool
	statusCode         int
	duration           time.Duration
	bodyMatch          bool
	lastRunUnix        int64
	lastSuccessUnix    int64
	successes          uint64
	failureReason      string
	errorMessage       string
	httpVersion        float64
	contentLength      float64
	bodyLength         float64
	ssl                bool
	earliestCertExpiry int64
	redirects          int
	lastModifiedUnix   int64
	phaseDurations     map[string]time.Duration
}

type probeMetrics struct {
	httpProbeUp           *prometheus.GaugeVec
	probeSuccess          *prometheus.GaugeVec
	duration              *prometheus.GaugeVec
	httpDuration          *prometheus.GaugeVec
	httpStatusCode        *prometheus.GaugeVec
	httpContentLength     *prometheus.GaugeVec
	httpBodyLength        *prometheus.GaugeVec
	httpBodyMatch         *prometheus.GaugeVec
	failedDueToRegex      *prometheus.GaugeVec
	httpSSL               *prometheus.GaugeVec
	sslEarliestCertExpiry *prometheus.GaugeVec
	httpVersion           *prometheus.GaugeVec
	httpRedirects         *prometheus.GaugeVec
	httpLastModified      *prometheus.GaugeVec
	lastRun               *prometheus.GaugeVec
	lastSuccess           *prometheus.GaugeVec
	successTotal          *prometheus.CounterVec
	failureTotal          *prometheus.CounterVec
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	configPath := flag.String("config", defaultConfigPath, "path to config json")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	//registry.MustRegister(collectors.NewGoCollector())
	//registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := newProbeMetrics(registry)
	for _, target := range cfg.Targets {
		metrics.initTarget(target.URL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, target := range cfg.Targets {
		go runProbeLoop(ctx, metrics, target)
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
	}()

	slog.Info("http-probe listening",
		"listen_addr", cfg.ListenAddr,
		"metrics_path", cfg.MetricsPath,
		"targets", len(cfg.Targets),
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (runtimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return runtimeConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}

	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	metricsPath := strings.TrimSpace(cfg.MetricsPath)
	if metricsPath == "" {
		metricsPath = defaultMetricsPath
	}
	if !strings.HasPrefix(metricsPath, "/") {
		return runtimeConfig{}, fmt.Errorf("metrics_path must start with /")
	}

	probeInterval, err := parseDurationOrDefault(cfg.ProbeInterval, defaultProbeInterval)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf("probe_interval: %w", err)
	}
	if probeInterval <= 0 {
		return runtimeConfig{}, fmt.Errorf("probe_interval must be greater than 0")
	}

	probeTimeout, err := parseDurationOrDefault(cfg.ProbeTimeout, defaultProbeTimeout)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf("probe_timeout: %w", err)
	}
	if probeTimeout <= 0 {
		return runtimeConfig{}, fmt.Errorf("probe_timeout must be greater than 0")
	}

	if len(cfg.Targets) == 0 {
		return runtimeConfig{}, fmt.Errorf("targets is required")
	}

	seenURLs := make(map[string]struct{}, len(cfg.Targets))
	targets := make([]target, 0, len(cfg.Targets))
	for i, raw := range cfg.Targets {
		t, err := normalizeTarget(raw)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("targets[%d]: %w", i, err)
		}
		t.ProbeInterval, err = parseDurationOrDefault(raw.ProbeInterval, probeInterval)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("targets[%d].probe_interval: %w", i, err)
		}
		if t.ProbeInterval <= 0 {
			return runtimeConfig{}, fmt.Errorf("targets[%d].probe_interval must be greater than 0", i)
		}
		t.ProbeTimeout, err = parseDurationOrDefault(raw.ProbeTimeout, probeTimeout)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("targets[%d].probe_timeout: %w", i, err)
		}
		if t.ProbeTimeout <= 0 {
			return runtimeConfig{}, fmt.Errorf("targets[%d].probe_timeout must be greater than 0", i)
		}
		if _, ok := seenURLs[t.URL]; ok {
			return runtimeConfig{}, fmt.Errorf("targets[%d]: duplicate url %q", i, t.URL)
		}
		seenURLs[t.URL] = struct{}{}
		targets = append(targets, t)
	}

	return runtimeConfig{
		ListenAddr:  listenAddr,
		MetricsPath: metricsPath,
		Targets:     targets,
	}, nil
}

func parseDurationOrDefault(raw string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	return time.ParseDuration(raw)
}

func normalizeTarget(raw targetConfig) (target, error) {
	method := strings.ToUpper(strings.TrimSpace(raw.Method))
	if method == "" {
		method = defaultMethod
	}

	url := strings.TrimSpace(os.ExpandEnv(raw.URL))
	if url == "" {
		return target{}, fmt.Errorf("url is required")
	}

	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = url
	}

	expected := os.ExpandEnv(raw.ExpectedBodyContains)
	if expected == "" {
		return target{}, fmt.Errorf("expected_body_contains is required")
	}

	headers := make(map[string]string, len(raw.Headers))
	for key, value := range raw.Headers {
		key = strings.TrimSpace(key)
		if key == "" {
			return target{}, fmt.Errorf("headers contains empty key")
		}
		headers[key] = os.ExpandEnv(value)
	}

	body := bytes.TrimSpace(raw.Body)
	if len(body) > 0 {
		body = []byte(os.ExpandEnv(string(body)))
	}

	return target{
		Name:                 name,
		Method:               method,
		URL:                  url,
		Headers:              headers,
		Body:                 body,
		ExpectedBodyContains: expected,
	}, nil
}

func runProbeLoop(ctx context.Context, metrics *probeMetrics, target target) {
	runOnce := func() {
		result := probe(ctx, target, target.ProbeTimeout)
		metrics.observe(target.URL, result)
		if result.up {
			logProbeSuccess(target, result)
			return
		}
		logProbeFailure(target, result)
	}

	runOnce()

	ticker := time.NewTicker(target.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func logProbeSuccess(target target, result probeResult) {
	slog.Info("probe success", probeLogAttrs(target, result)...)
}

func logProbeFailure(target target, result probeResult) {
	attrs := append(probeLogAttrs(target, result),
		slog.String("reason", result.failureReason),
		slog.String("error", result.errorMessage),
	)
	if isProbeWarning(result.failureReason) {
		slog.Warn("probe failed", attrs...)
		return
	}
	slog.Error("probe failed", attrs...)
}

func probeLogAttrs(target target, result probeResult) []any {
	return []any{
		slog.String("target", target.Name),
		slog.String("url", target.URL),
		slog.Int("status", result.statusCode),
		slog.Float64("duration_seconds", result.duration.Seconds()),
	}
}

func isProbeWarning(reason string) bool {
	return reason == "bad_status" || reason == "body_mismatch"
}

func probe(ctx context.Context, target target, timeout time.Duration) probeResult {
	start := time.Now()
	result := probeResult{
		lastRunUnix:    time.Now().Unix(),
		phaseDurations: make(map[string]time.Duration),
	}

	var body io.Reader
	if len(target.Body) > 0 {
		body = bytes.NewReader(target.Body)
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var trace requestTrace
	requestCtx = httptrace.WithClientTrace(requestCtx, trace.clientTrace())

	req, err := http.NewRequestWithContext(requestCtx, target.Method, target.URL, body)
	if err != nil {
		result.duration = time.Since(start)
		result.setError("request_build_error", err)
		return result
	}

	for key, value := range target.Headers {
		req.Header.Set(key, value)
	}

	redirects := 0
	client := &http.Client{
		Timeout:   timeout,
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			redirects++
			return nil
		},
	}

	resp, err := client.Do(req)
	result.redirects = redirects
	if err != nil {
		end := time.Now()
		result.duration = end.Sub(start)
		result.phaseDurations = trace.phaseDurations(start, end)
		result.setError("request_error", err)
		return result
	}
	defer resp.Body.Close()

	result.statusCode = resp.StatusCode
	result.httpVersion = httpVersion(resp)
	result.contentLength = float64(resp.ContentLength)
	result.ssl = resp.TLS != nil
	result.earliestCertExpiry = earliestCertExpiry(resp)
	result.lastModifiedUnix = lastModified(resp)

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	end := time.Now()
	result.duration = end.Sub(start)
	result.phaseDurations = trace.phaseDurations(start, end)
	if err != nil {
		result.setError("body_read_error", err)
		return result
	}
	result.bodyLength = float64(len(responseBody))
	if result.contentLength < 0 {
		result.contentLength = result.bodyLength
	}

	result.bodyMatch = strings.Contains(string(responseBody), target.ExpectedBodyContains)
	if resp.StatusCode != http.StatusOK {
		result.setFailure("bad_status", probeFailureMessage("bad_status", resp.StatusCode, responseBody))
		return result
	}
	if !result.bodyMatch {
		result.setFailure("body_mismatch", probeFailureMessage("body_mismatch", resp.StatusCode, responseBody))
		return result
	}

	result.up = true
	result.successes = 1
	result.lastSuccessUnix = time.Now().Unix()
	return result
}

func (r *probeResult) setError(reason string, err error) {
	r.failureReason = reason
	if err == nil {
		r.errorMessage = reason
		return
	}
	r.errorMessage = fmt.Sprintf("%s: %v", reason, err)
}

func (r *probeResult) setFailure(reason string, message string) {
	r.failureReason = reason
	r.errorMessage = message
}

func probeFailureMessage(reason string, statusCode int, responseBody []byte) string {
	return fmt.Sprintf("%s: status=%d response_body=%s", reason, statusCode, string(responseBody))
}

type requestTrace struct {
	dnsStart          time.Time
	dnsDone           time.Time
	connectStart      time.Time
	connectDone       time.Time
	tlsStart          time.Time
	tlsDone           time.Time
	wroteRequest      time.Time
	firstResponseByte time.Time
}

func (t *requestTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			setIfZero(&t.dnsStart, time.Now())
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			setIfZero(&t.dnsDone, time.Now())
		},
		ConnectStart: func(_, _ string) {
			setIfZero(&t.connectStart, time.Now())
		},
		ConnectDone: func(_, _ string, _ error) {
			setIfZero(&t.connectDone, time.Now())
		},
		TLSHandshakeStart: func() {
			setIfZero(&t.tlsStart, time.Now())
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			setIfZero(&t.tlsDone, time.Now())
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			setIfZero(&t.wroteRequest, time.Now())
		},
		GotFirstResponseByte: func() {
			setIfZero(&t.firstResponseByte, time.Now())
		},
	}
}

func (t requestTrace) phaseDurations(start, end time.Time) map[string]time.Duration {
	phases := map[string]time.Duration{
		"resolve":    0,
		"connect":    0,
		"tls":        0,
		"processing": 0,
		"transfer":   0,
		"total":      end.Sub(start),
	}
	if !t.dnsStart.IsZero() && !t.dnsDone.IsZero() {
		phases["resolve"] = t.dnsDone.Sub(t.dnsStart)
	}
	if !t.connectStart.IsZero() && !t.connectDone.IsZero() {
		phases["connect"] = t.connectDone.Sub(t.connectStart)
	}
	if !t.tlsStart.IsZero() && !t.tlsDone.IsZero() {
		phases["tls"] = t.tlsDone.Sub(t.tlsStart)
	}
	if !t.wroteRequest.IsZero() && !t.firstResponseByte.IsZero() {
		phases["processing"] = t.firstResponseByte.Sub(t.wroteRequest)
	}
	if !t.firstResponseByte.IsZero() {
		phases["transfer"] = end.Sub(t.firstResponseByte)
	}
	return phases
}

func setIfZero(target *time.Time, value time.Time) {
	if target.IsZero() {
		*target = value
	}
}

func httpVersion(resp *http.Response) float64 {
	switch {
	case resp.ProtoMajor == 2:
		return 2
	case resp.ProtoMajor == 1 && resp.ProtoMinor == 1:
		return 1.1
	case resp.ProtoMajor == 1:
		return 1
	default:
		return 0
	}
}

func earliestCertExpiry(resp *http.Response) int64 {
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return 0
	}

	earliest := resp.TLS.PeerCertificates[0].NotAfter
	for _, cert := range resp.TLS.PeerCertificates[1:] {
		if cert.NotAfter.Before(earliest) {
			earliest = cert.NotAfter
		}
	}
	return earliest.Unix()
}

func lastModified(resp *http.Response) int64 {
	raw := strings.TrimSpace(resp.Header.Get("Last-Modified"))
	if raw == "" {
		return 0
	}
	parsed, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

func newProbeMetrics(registry *prometheus.Registry) *probeMetrics {
	m := &probeMetrics{
		httpProbeUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "http_probe_up",
			Help: "Whether the latest probe succeeded. 1 means available, 0 means unavailable.",
		}, []string{"target"}),
		probeSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_success",
			Help: "Displays whether or not the probe was a success.",
		}, []string{"target"}),
		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_duration_seconds",
			Help: "Returns how long the probe took to complete in seconds.",
		}, []string{"target"}),
		httpDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_duration_seconds",
			Help: "Duration of probe HTTP phases in seconds.",
		}, []string{"target", "phase"}),
		httpStatusCode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_status_code",
			Help: "Response HTTP status code. 0 means no response.",
		}, []string{"target"}),
		httpContentLength: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_content_length",
			Help: "Length of the HTTP response body in bytes. Uses bytes read when Content-Length is absent.",
		}, []string{"target"}),
		httpBodyLength: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_uncompressed_body_length",
			Help: "Length of the uncompressed HTTP response body read by the probe in bytes.",
		}, []string{"target"}),
		httpBodyMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_body_match",
			Help: "Whether the latest HTTP response body contained the configured expected string.",
		}, []string{"target"}),
		failedDueToRegex: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_failed_due_to_regex",
			Help: "Whether the probe failed because the response body did not contain the expected string.",
		}, []string{"target"}),
		httpSSL: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_ssl",
			Help: "Indicates if SSL was used for the final redirect.",
		}, []string{"target"}),
		sslEarliestCertExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_ssl_earliest_cert_expiry",
			Help: "Returns earliest SSL cert expiry in Unix time.",
		}, []string{"target"}),
		httpVersion: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_version",
			Help: "Returns the version of HTTP of the probe response.",
		}, []string{"target"}),
		httpRedirects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_redirects",
			Help: "The number of redirects followed by the probe.",
		}, []string{"target"}),
		httpLastModified: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_last_modified_timestamp_seconds",
			Help: "Returns the Last-Modified HTTP response header in Unix time, or 0 if unavailable.",
		}, []string{"target"}),
		lastRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "http_probe_last_run_timestamp_seconds",
			Help: "Unix timestamp of the latest probe attempt.",
		}, []string{"target"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "http_probe_last_success_timestamp_seconds",
			Help: "Unix timestamp of the latest successful probe.",
		}, []string{"target"}),
		successTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_probe_success_total",
			Help: "Total successful probes.",
		}, []string{"target"}),
		failureTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_probe_failure_total",
			Help: "Total failed probes by reason.",
		}, []string{"target", "reason"}),
	}

	registry.MustRegister(
		m.httpProbeUp,
		m.probeSuccess,
		m.duration,
		m.httpDuration,
		m.httpStatusCode,
		m.httpContentLength,
		m.httpBodyLength,
		m.httpBodyMatch,
		m.failedDueToRegex,
		m.httpSSL,
		m.sslEarliestCertExpiry,
		m.httpVersion,
		m.httpRedirects,
		m.httpLastModified,
		m.lastRun,
		m.lastSuccess,
		m.successTotal,
		m.failureTotal,
	)
	return m
}

func (m *probeMetrics) initTarget(target string) {
	m.httpProbeUp.WithLabelValues(target).Set(0)
	m.probeSuccess.WithLabelValues(target).Set(0)
	m.duration.WithLabelValues(target).Set(0)
	for _, phase := range []string{"resolve", "connect", "tls", "processing", "transfer", "total"} {
		m.httpDuration.WithLabelValues(target, phase).Set(0)
	}
	m.httpStatusCode.WithLabelValues(target).Set(0)
	m.httpContentLength.WithLabelValues(target).Set(0)
	m.httpBodyLength.WithLabelValues(target).Set(0)
	m.httpBodyMatch.WithLabelValues(target).Set(0)
	m.failedDueToRegex.WithLabelValues(target).Set(0)
	m.httpSSL.WithLabelValues(target).Set(0)
	m.sslEarliestCertExpiry.WithLabelValues(target).Set(0)
	m.httpVersion.WithLabelValues(target).Set(0)
	m.httpRedirects.WithLabelValues(target).Set(0)
	m.httpLastModified.WithLabelValues(target).Set(0)
	m.lastRun.WithLabelValues(target).Set(0)
	m.lastSuccess.WithLabelValues(target).Set(0)
	m.successTotal.WithLabelValues(target).Add(0)
}

func (m *probeMetrics) observe(target string, result probeResult) {
	success := boolFloat(result.up)
	m.httpProbeUp.WithLabelValues(target).Set(success)
	m.probeSuccess.WithLabelValues(target).Set(success)
	m.duration.WithLabelValues(target).Set(result.duration.Seconds())
	for _, phase := range []string{"resolve", "connect", "tls", "processing", "transfer", "total"} {
		m.httpDuration.WithLabelValues(target, phase).Set(result.phaseDurations[phase].Seconds())
	}
	m.httpStatusCode.WithLabelValues(target).Set(float64(result.statusCode))
	m.httpContentLength.WithLabelValues(target).Set(result.contentLength)
	m.httpBodyLength.WithLabelValues(target).Set(result.bodyLength)
	m.httpBodyMatch.WithLabelValues(target).Set(boolFloat(result.bodyMatch))
	m.failedDueToRegex.WithLabelValues(target).Set(boolFloat(result.errorMessage == "body_mismatch"))
	m.httpSSL.WithLabelValues(target).Set(boolFloat(result.ssl))
	m.sslEarliestCertExpiry.WithLabelValues(target).Set(float64(result.earliestCertExpiry))
	m.httpVersion.WithLabelValues(target).Set(result.httpVersion)
	m.httpRedirects.WithLabelValues(target).Set(float64(result.redirects))
	m.httpLastModified.WithLabelValues(target).Set(float64(result.lastModifiedUnix))
	m.lastRun.WithLabelValues(target).Set(float64(result.lastRunUnix))
	if result.lastSuccessUnix > 0 {
		m.lastSuccess.WithLabelValues(target).Set(float64(result.lastSuccessUnix))
	}

	if result.up {
		m.successTotal.WithLabelValues(target).Inc()
		return
	}
	reason := result.failureReason
	if reason == "" {
		reason = "unknown"
	}
	m.failureTotal.WithLabelValues(target, reason).Inc()
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
