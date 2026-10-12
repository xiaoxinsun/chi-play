package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Run with:  go test -run TestProxyTimeout -v
//
// Two servers:
//
//	front: Timeout(500ms) -> proxyHandler -> HTTP call ->
//	back:  /slow/{ms} (the same slowHandler as the real app)
//
// The question: when front's deadline passes, does the outgoing HTTP call
// actually stop? We check by watching whether back's request context gets
// cancelled (which happens when the client closes the connection).
func TestProxyTimeout(t *testing.T) {
	var cancelled sync.Map // ms path param -> true, set when back saw its client go away

	back := chi.NewRouter()
	back.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if r.Context().Err() != nil { // slowHandler bailed out on ctx.Done()
				cancelled.Store(chi.URLParam(r, "ms"), true)
			}
		})
	})
	back.Get("/slow/{ms}", slowHandler)
	backSrv := httptest.NewServer(back)
	t.Cleanup(backSrv.Close) // Cleanup, not defer: parallel subtests outlive this function body

	front := chi.NewRouter()
	front.Use(middleware.Timeout(deadline)) // deadline = 500ms, from timeout_test.go
	front.Get("/proxy/{ms}", proxyHandler(backSrv.URL))
	front.Get("/proxy-noctx/{ms}", proxyNoCtxHandler(backSrv.URL))

	tests := []struct {
		name          string
		path          string
		ms            string
		wantStatus    int // -1: either 200 or 504 (race)
		minElapsed    time.Duration
		maxElapsed    time.Duration
		wantUpstreamX bool // expect the upstream call to have been aborted
	}{
		{"1 within deadline (100ms)", "/proxy/100", "100", 200, 100 * time.Millisecond, 300 * time.Millisecond, false},
		{"2 close to deadline (450ms)", "/proxy/450", "450", 200, 450 * time.Millisecond, 495 * time.Millisecond, false},
		// 3. A race again (see timeout_test.go). Either outcome is fine.
		{"3 at deadline (500ms)", "/proxy/500", "500", -1, 500 * time.Millisecond, 600 * time.Millisecond, false},
		// 4. Deadline passes mid-call: client aborts, 504 at ~500ms, and the
		// upstream sees its client disconnect.
		{"4 after deadline, ctx-aware client (900ms)", "/proxy/900", "900", 504, 500 * time.Millisecond, 700 * time.Millisecond, true},
		// 4b. Client built without the context: no abort, waits all 900ms.
		{"4b after deadline, client ignores ctx (900ms)", "/proxy-noctx/901", "901", 200, 900 * time.Millisecond, 1100 * time.Millisecond, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			start := time.Now()
			front.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			elapsed := time.Since(start)

			t.Logf("status=%d elapsed=%v body=%q", rec.Code, elapsed.Round(time.Millisecond), rec.Body.String())

			if tc.wantStatus == -1 {
				if rec.Code != 200 && rec.Code != 504 {
					t.Errorf("status = %d, want 200 or 504", rec.Code)
				}
			} else if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if elapsed < tc.minElapsed || elapsed > tc.maxElapsed {
				t.Errorf("elapsed = %v, want between %v and %v", elapsed, tc.minElapsed, tc.maxElapsed)
			}

			if tc.wantUpstreamX {
				// The disconnect reaches the upstream asynchronously; poll briefly.
				deadlineAt := time.Now().Add(time.Second)
				for time.Now().Before(deadlineAt) {
					if _, ok := cancelled.Load(tc.ms); ok {
						t.Logf("upstream saw the client disconnect (call was really aborted)")
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Errorf("upstream never saw its request cancelled; the call was not aborted")
			}
		})
	}
}
