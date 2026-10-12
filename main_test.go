package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Because a chi router is just an http.Handler, testing needs no server:
// build the router and call ServeHTTP with an httptest recorder.
func do(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoutes(t *testing.T) {
	h := NewRouter()

	tests := []struct {
		name         string
		method, path string
		body         string
		hdr          []string
		wantStatus   int
		wantBody     string // substring
	}{
		{"root", "GET", "/", "", nil, 200, "hello, chi!"},
		{"url param", "GET", "/hello/gopher", "", nil, 200, "hello, gopher!"},
		{"regex ok", "GET", "/square/7", "", nil, 200, `"square":49`},
		{"regex mismatch", "GET", "/square/abc", "", nil, 404, "no such route"},
		{"wildcard", "GET", "/files/a/b/c.txt", "", nil, 200, "a/b/c.txt"},
		{"query", "GET", "/greet?name=bill", "", nil, 200, "hi bill"},
		{"panic recovered", "GET", "/panic", "", nil, 500, ""},
		{"todo list", "GET", "/todos", "", nil, 200, "say hello to chi"},
		{"todo get", "GET", "/todos/1", "", nil, 200, `"id":1`},
		{"todo missing", "GET", "/todos/99", "", nil, 404, "todo not found"},
		{"todo bad id", "GET", "/todos/xyz", "", nil, 400, "id must be a number"},
		{"todo create", "POST", "/todos", `{"title":"x"}`, nil, 201, `"title":"x"`},
		{"todo create bad", "POST", "/todos", `nope`, nil, 400, "title"},
		{"admin no key", "GET", "/admin/stats", "", nil, 401, "X-API-Key"},
		{"admin with key", "GET", "/admin/stats", "", []string{"X-API-Key", "secret"}, 200, `"todos"`},
		{"mounted", "GET", "/v2/ping", "", nil, 200, "pong from v2"},
		{"405", "PATCH", "/", "", nil, 405, "method not allowed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.body, tc.hdr...)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want substring %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestTodoDelete(t *testing.T) {
	h := NewRouter() // fresh store per router
	if rec := do(t, h, "DELETE", "/todos/1", ""); rec.Code != 204 {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if rec := do(t, h, "GET", "/todos/1", ""); rec.Code != 404 {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}
