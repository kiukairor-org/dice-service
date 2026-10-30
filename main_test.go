package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func get(t *testing.T, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]string
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid JSON: %v", path, err)
		}
	}
	return rec, body
}

func TestRoot(t *testing.T) {
	t.Setenv("APP_NAME", "demo")
	rec, body := get(t, "/")
	if rec.Code != http.StatusOK || body["app"] != "demo" || body["message"] != "hello" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestHealthz(t *testing.T) {
	rec, body := get(t, "/healthz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestVersion(t *testing.T) {
	t.Setenv("APP_NAME", "demo")
	old := version
	version = "abc123"
	defer func() { version = old }()
	rec, body := get(t, "/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if len(body) != 2 || body["app"] != "demo" || body["version"] != "abc123" {
		t.Fatalf("got %v", body)
	}
}

func TestUnknownPath(t *testing.T) {
	if rec, _ := get(t, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func roll(t *testing.T, path string) (int, map[string]int) {
	t.Helper()
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s: content-type %q", path, ct)
	}
	var body map[string]int
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid JSON: %v", path, err)
		}
	}
	return rec.Code, body
}

func TestRollDefault(t *testing.T) {
	code, body := roll(t, "/roll")
	if code != http.StatusOK || len(body) != 2 || body["sides"] != 6 || body["result"] < 1 || body["result"] > 6 {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestRollBoundaries(t *testing.T) {
	for _, n := range []int{2, 100} {
		code, body := roll(t, "/roll?sides="+strconv.Itoa(n))
		if code != http.StatusOK || body["sides"] != n || body["result"] < 1 || body["result"] > n {
			t.Fatalf("sides=%d: got %d %v", n, code, body)
		}
	}
}

func TestRollResultWithinRange(t *testing.T) {
	for _, n := range []int{2, 6, 100} {
		for i := 0; i < 500; i++ {
			_, body := roll(t, "/roll?sides="+strconv.Itoa(n))
			if body["result"] < 1 || body["result"] > n {
				t.Fatalf("sides=%d: result %d out of range", n, body["result"])
			}
		}
	}
}

func TestRollMockedSource(t *testing.T) {
	old := randIntn
	defer func() { randIntn = old }()
	for _, tc := range []struct{ draw, want int }{{0, 1}, {19, 20}} {
		randIntn = func(n int) int {
			if n != 20 {
				t.Fatalf("randIntn(%d), want 20", n)
			}
			return tc.draw
		}
		code, body := roll(t, "/roll?sides=20")
		if code != http.StatusOK || body["sides"] != 20 || body["result"] != tc.want {
			t.Fatalf("draw %d: got %d %v", tc.draw, code, body)
		}
	}
}

func TestRollInvalid(t *testing.T) {
	for _, q := range []string{"1", "0", "-3", "101", "1000", "", "abc", "3.5", "%20", "99999999999999999999"} {
		if code, _ := roll(t, "/roll?sides="+q); code != http.StatusBadRequest {
			t.Errorf("sides=%q: got %d, want 400", q, code)
		}
	}
}
