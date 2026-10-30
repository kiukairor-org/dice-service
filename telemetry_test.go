package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type fakeCollector struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
	ctypes []string
}

func (f *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	f.paths = append(f.paths, r.URL.Path)
	f.ctypes = append(f.ctypes, r.Header.Get("Content-Type"))
	f.mu.Unlock()
}

type otlpPayload struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID    string `json:"traceId"`
				SpanID     string `json:"spanId"`
				Name       string `json:"name"`
				Kind       int    `json:"kind"`
				Attributes []struct {
					Key   string         `json:"key"`
					Value map[string]any `json:"value"`
				} `json:"attributes"`
				Status struct {
					Code int `json:"code"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func serve(t *testing.T, tr *tracer, method, path string) {
	t.Helper()
	h := tracingMiddleware(tr, newMux())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

func TestSpanExported(t *testing.T) {
	fc := &fakeCollector{}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	tr := newTracer(srv.URL, "demo", "v1")

	serve(t, tr, http.MethodGet, "/version")
	serve(t, tr, http.MethodGet, "/nope")
	tr.flush()

	if len(fc.bodies) != 1 || fc.paths[0] != "/v1/traces" || fc.ctypes[0] != "application/json" {
		t.Fatalf("unexpected requests: %v %v", fc.paths, fc.ctypes)
	}
	var p otlpPayload
	if err := json.Unmarshal(fc.bodies[0], &p); err != nil {
		t.Fatal(err)
	}
	res := map[string]string{}
	for _, a := range p.ResourceSpans[0].Resource.Attributes {
		res[a.Key] = a.Value.StringValue
	}
	if res["service.name"] != "demo" || res["service.version"] != "v1" {
		t.Fatalf("resource = %v", res)
	}
	spans := p.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	s := spans[0]
	if s.Name != "GET /version" || s.Kind != 2 || len(s.TraceID) != 32 || len(s.SpanID) != 16 || s.Status.Code != 0 {
		t.Fatalf("span = %+v", s)
	}
	got := map[string]map[string]any{}
	for _, a := range s.Attributes {
		got[a.Key] = a.Value
	}
	if got["http.request.method"]["stringValue"] != "GET" || got["http.route"]["stringValue"] != "/version" ||
		got["url.path"]["stringValue"] != "/version" || got["http.response.status_code"]["intValue"] != "200" {
		t.Fatalf("attrs = %v", got)
	}
	if spans[1].Name != "GET (unmatched)" {
		t.Fatalf("404 span name = %q", spans[1].Name)
	}
}

func TestServerErrorMarkedError(t *testing.T) {
	fc := &fakeCollector{}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	tr := newTracer(srv.URL, "demo", "v1")

	h := tracingMiddleware(tr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	tr.flush()

	var p otlpPayload
	if err := json.Unmarshal(fc.bodies[0], &p); err != nil {
		t.Fatal(err)
	}
	if code := p.ResourceSpans[0].ScopeSpans[0].Spans[0].Status.Code; code != 2 {
		t.Fatalf("status code = %d, want 2", code)
	}
}

func TestHealthzNotTraced(t *testing.T) {
	fc := &fakeCollector{}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	tr := newTracer(srv.URL, "demo", "v1")

	serve(t, tr, http.MethodGet, "/healthz")
	tr.flush()
	if len(fc.bodies) != 0 {
		t.Fatalf("healthz was exported: %d requests", len(fc.bodies))
	}
}

func TestTracingOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if tr := tracerFromEnv(); tr != nil {
		t.Fatal("tracer should be nil without endpoint")
	}
	rec := httptest.NewRecorder()
	tracingMiddleware(nil, newMux()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUnreachableCollectorAndFullQueueDoNotBlock(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	tr := newTracer(url, "demo", "v1")

	for i := 0; i < spanQueueSize+10; i++ {
		serve(t, tr, http.MethodGet, "/version")
	}
	if len(tr.queue) != spanQueueSize {
		t.Fatalf("queue len = %d", len(tr.queue))
	}
	tr.flush() // must return despite unreachable collector
}
