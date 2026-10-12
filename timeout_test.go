package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Run with:  go test -run TestTimeout -v
//
// A router with ONLY the Timeout middleware and the two demo handlers, using a
// short deadline so the test is quick. The handlers are the same ones the real
// app serves at /slow/{ms} and /stubborn/{ms}.
const deadline = 500 * time.Millisecond

func timeoutRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Timeout(deadline))
	r.Get("/slow/{ms}", slowHandler)
	r.Get("/stubborn/{ms}", stubbornHandler)
	return r
}

func TestTimeout(t *testing.T) {
	h := timeoutRouter()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		minElapsed time.Duration // response can't come back faster than this
		maxElapsed time.Duration // ...nor slower than this
	}{
		{
			// 1. Well inside the deadline: normal 200, returns in ~100ms.
			name:       "1 within deadline (100ms of 500ms)",
			path:       "/slow/100",
			wantStatus: 200,
			minElapsed: 100 * time.Millisecond,
			maxElapsed: 300 * time.Millisecond,
		},
		{
			// 2. Cutting it fine: 50ms to spare, still a 200.
			name:       "2 close to deadline (450ms of 500ms)",
			path:       "/slow/450",
			wantStatus: 200,
			minElapsed: 450 * time.Millisecond,
			maxElapsed: 495 * time.Millisecond,
		},
		{
			// 3. Dead heat: the work timer and the deadline fire at (nearly)
			// the same instant, so the select in slowHandler is a race. The
			// deadline was set first, but cancelling a context takes an extra
			// goroutine hop, whereas the work timer's channel is signalled
			// directly -- so in practice this tends to come back 200. Treat
			// it as a coin flip: either result is "correct", which is the
			// lesson. Don't build logic that depends on winning this race.
			name:       "3 at deadline (500ms of 500ms)",
			path:       "/slow/500",
			wantStatus: -1, // 200 or 504 both acceptable; logged below
			minElapsed: 500 * time.Millisecond,
			maxElapsed: 600 * time.Millisecond,
		},
		{
			// 4. Past the deadline: handler notices ctx.Done() at 500ms and
			// bails out. 504 comes back at ~500ms, NOT after the 900ms of work.
			name:       "4 after deadline (900ms of 500ms)",
			path:       "/slow/900",
			wantStatus: 504,
			minElapsed: 500 * time.Millisecond,
			maxElapsed: 700 * time.Millisecond,
		},
		{
			// 4b. Same, but the handler ignores the context. The timeout can't
			// interrupt it: it runs all 900ms and writes a 200. (The
			// middleware's attempt to write 504 afterwards is a no-op because
			// the header is already sent.)
			name:       "4b after deadline, handler ignores ctx (900ms of 500ms)",
			path:       "/stubborn/900",
			wantStatus: 200,
			minElapsed: 900 * time.Millisecond,
			maxElapsed: 1100 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // all cases sleep, so run them concurrently

			req := httptest.NewRequest("GET", tc.path, nil)
			rec := httptest.NewRecorder()

			start := time.Now()
			h.ServeHTTP(rec, req)
			elapsed := time.Since(start)

			t.Logf("status=%d elapsed=%v body=%q", rec.Code, elapsed.Round(time.Millisecond), rec.Body.String())

			if tc.wantStatus != -1 && rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == -1 && rec.Code != 200 && rec.Code != 504 {
				t.Errorf("status = %d, want 200 or 504", rec.Code)
			}
			if elapsed < tc.minElapsed || elapsed > tc.maxElapsed {
				t.Errorf("elapsed = %v, want between %v and %v", elapsed, tc.minElapsed, tc.maxElapsed)
			}
		})
	}
}
