package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	spanQueueSize = 1024
	flushInterval = 5 * time.Second
	exportTimeout = 3 * time.Second
	maxBatch      = 512
)

type attr struct {
	Key   string    `json:"key"`
	Value attrValue `json:"value"`
}

// attrValue holds one OTLP AnyValue; int64 values are encoded as strings.
type attrValue struct {
	String *string `json:"stringValue,omitempty"`
	Int    *string `json:"intValue,omitempty"`
}

func strAttr(k, v string) attr { return attr{k, attrValue{String: &v}} }
func intAttr(k string, v int) attr {
	s := strconv.Itoa(v)
	return attr{k, attrValue{Int: &s}}
}

type spanStatus struct {
	Code int `json:"code"`
}

type span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []attr     `json:"attributes"`
	Status            spanStatus `json:"status"`
}

type tracer struct {
	url     string
	service string
	version string
	client  *http.Client
	queue   chan span
}

// newTracer returns nil (tracing off) when endpoint is empty.
func newTracer(endpoint, service, version string) *tracer {
	if endpoint == "" {
		return nil
	}
	return &tracer{
		url:     strings.TrimRight(endpoint, "/") + "/v1/traces",
		service: service,
		version: version,
		client:  &http.Client{Timeout: exportTimeout},
		queue:   make(chan span, spanQueueSize),
	}
}

// tracerFromEnv builds a tracer from OTEL_* env vars and starts its exporter.
func tracerFromEnv() *tracer {
	service := os.Getenv("OTEL_SERVICE_NAME")
	if service == "" {
		service = appName()
	}
	t := newTracer(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), service, version)
	if t != nil {
		go func() {
			for range time.Tick(flushInterval) {
				t.flush()
			}
		}()
	}
	return t
}

// record enqueues a span, dropping it if the queue is full.
func (t *tracer) record(s span) {
	select {
	case t.queue <- s:
	default:
	}
}

// flush exports everything currently queued. Errors are ignored.
func (t *tracer) flush() {
	for {
		var batch []span
	drain:
		for len(batch) < maxBatch {
			select {
			case s := <-t.queue:
				batch = append(batch, s)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			return
		}
		t.export(batch)
	}
}

func (t *tracer) export(batch []span) {
	payload := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []attr{
			strAttr("service.name", t.service),
			strAttr("service.version", t.version),
		}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]string{"name": "dice-service"},
			"spans": batch,
		}},
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

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

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tracingMiddleware records one SERVER span per request, skipping /healthz.
// A nil tracer returns next unchanged.
func tracingMiddleware(t *tracer, next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		end := time.Now()
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		// The mux sets r.Pattern on the request it routes; empty on 404.
		name := r.Method + " (unmatched)"
		attrs := []attr{strAttr("http.request.method", r.Method)}
		if r.Pattern != "" {
			name = r.Pattern
			route := r.Pattern
			if _, after, ok := strings.Cut(route, " "); ok {
				route = after
			}
			attrs = append(attrs, strAttr("http.route", route))
		}
		attrs = append(attrs, strAttr("url.path", r.URL.Path), intAttr("http.response.status_code", rec.status))

		s := span{
			TraceID:           randHex(16),
			SpanID:            randHex(8),
			Name:              name,
			Kind:              2,
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(end.UnixNano(), 10),
			Attributes:        attrs,
		}
		if rec.status >= 500 {
			s.Status.Code = 2
		}
		t.record(s)
	})
}
