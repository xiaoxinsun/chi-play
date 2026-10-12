// chi 101 — a guided tour of go-chi/chi in one file.
//
// Run:   go run .
// Try:   see the curl examples next to each route (or run `make demo`).
//
// What chi is: a tiny router that sits on top of net/http. A chi.Router IS an
// http.Handler, and every handler/middleware is plain net/http
// (func(w http.ResponseWriter, r *http.Request)). That's the whole trick —
// anything you know from net/http works, chi just adds routing conveniences.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	addr := ":3000"
	log.Printf("listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, NewRouter()))
}

// NewRouter builds the app. Keeping it in a function (instead of inlining in
// main) lets tests call it with httptest — see main_test.go.
func NewRouter() http.Handler {
	r := chi.NewRouter()

	// ---------------------------------------------------------------
	// 1. Middleware: wraps every route registered AFTER it on this router.
	//    Order matters — the first Use() is the outermost wrapper.
	//    chi ships a bunch in chi/middleware.
	// ---------------------------------------------------------------
	r.Use(middleware.RequestID) // adds X-Request-Id, stored in the request context
	r.Use(middleware.Logger)    // logs method, path, status, duration
	r.Use(middleware.Recoverer) // turns panics into 500s instead of killing the conn
	r.Use(middleware.Timeout(10 * time.Second))

	// ---------------------------------------------------------------
	// 2. Basic routes. r.Get/Post/Put/Delete/... take a pattern + handler.
	// ---------------------------------------------------------------

	// curl localhost:3000/
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello, chi!\n"))
	})

	// URL parameters: {name} matches one path segment.
	// curl localhost:3000/hello/gopher
	r.Get("/hello/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		w.Write([]byte("hello, " + name + "!\n"))
	})

	// Regex constraint: {id:[0-9]+} only matches digits, otherwise -> 404.
	// curl localhost:3000/square/7     (works)
	// curl localhost:3000/square/abc   (404)
	r.Get("/square/{n:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(chi.URLParam(r, "n"))
		writeJSON(w, http.StatusOK, map[string]int{"n": n, "square": n * n})
	})

	// Wildcard: {*} or "*" swallows the rest of the path.
	// curl localhost:3000/files/a/b/c.txt
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file path: " + chi.URLParam(r, "*") + "\n"))
	})

	// Query strings are NOT chi's job — use plain net/http.
	// curl 'localhost:3000/greet?name=bill'
	r.Get("/greet", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "stranger"
		}
		w.Write([]byte("hi " + name + "\n"))
	})

	// A panic, to show that Recoverer works.
	// curl -i localhost:3000/panic
	r.Get("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	// Timeout demo (see timeout_test.go). The global Timeout above is 10s.
	// curl -i localhost:3000/slow/2000        -> 200 after ~2s
	// curl -i localhost:3000/slow/15000       -> 504 at ~10s
	// curl -i localhost:3000/stubborn/15000   -> 200 after the full 15s (ignores the deadline)
	r.Get("/slow/{ms:[0-9]+}", slowHandler)
	r.Get("/stubborn/{ms:[0-9]+}", stubbornHandler)

	// Same idea, but the slow part is an HTTP call to another service (here,
	// ourselves: /slow above). See proxy_test.go.
	// curl -i localhost:3000/proxy/15000          -> 504 at ~10s, upstream call aborted
	// curl -i localhost:3000/proxy-noctx/15000    -> 200 at ~10s with an empty "proxied: " body
	//   (it ignores OUR deadline, but the upstream /slow hits its own 10s
	//   Timeout and returns an empty 504, which this handler never checks)
	// curl -i localhost:3000/proxy-noctx/8000     -> 200 after 8s, normal
	r.Get("/proxy/{ms:[0-9]+}", proxyHandler(selfURL))
	r.Get("/proxy-noctx/{ms:[0-9]+}", proxyNoCtxHandler(selfURL))

	// ---------------------------------------------------------------
	// 3. Sub-routers with r.Route: groups routes under a common prefix.
	//    Middleware added inside only applies to that sub-tree.
	//    This is a tiny in-memory REST API for "todos".
	// ---------------------------------------------------------------
	store := newTodoStore()

	r.Route("/todos", func(r chi.Router) {
		// curl localhost:3000/todos
		r.Get("/", store.list)
		// curl -X POST localhost:3000/todos -d '{"title":"learn chi"}'
		r.Post("/", store.create)

		// Nested route with its own middleware: todoCtx loads the todo once
		// and stashes it in the request context for the handlers below.
		r.Route("/{id}", func(r chi.Router) {
			r.Use(store.todoCtx)
			// curl localhost:3000/todos/1
			r.Get("/", store.get)
			// curl -X DELETE localhost:3000/todos/1
			r.Delete("/", store.delete)
		})
	})

	// ---------------------------------------------------------------
	// 4. Group: share middleware WITHOUT adding a path prefix.
	//    Here, everything in the group requires an API key.
	//    curl localhost:3000/admin/stats                      -> 401
	//    curl -H 'X-API-Key: secret' localhost:3000/admin/stats -> 200
	// ---------------------------------------------------------------
	r.Group(func(r chi.Router) {
		r.Use(requireAPIKey("secret"))
		r.Get("/admin/stats", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"todos": store.count()})
		})
	})

	// ---------------------------------------------------------------
	// 5. Mount: attach any other http.Handler (often another chi.Router
	//    built elsewhere) at a prefix. Great for splitting a big app up.
	//    curl localhost:3000/v2/ping
	// ---------------------------------------------------------------
	r.Mount("/v2", v2Router())

	// ---------------------------------------------------------------
	// 6. Custom 404 / 405 handlers.
	//    curl -i localhost:3000/nope
	//    curl -i -X PATCH localhost:3000/        (405: path exists, method doesn't)
	// ---------------------------------------------------------------
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such route: " + r.URL.Path})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	})

	return r
}

// workDuration parses the {ms} URL param into a duration.
func workDuration(r *http.Request) time.Duration {
	ms, _ := strconv.Atoi(chi.URLParam(r, "ms"))
	return time.Duration(ms) * time.Millisecond
}

// slowHandler simulates {ms} milliseconds of work and COOPERATES with
// middleware.Timeout by also listening on r.Context().Done().
func slowHandler(w http.ResponseWriter, r *http.Request) {
	d := workDuration(r)
	select {
	case <-time.After(d):
		w.Write([]byte("done after " + d.String() + "\n"))
	case <-r.Context().Done():
		// Deadline hit. Just return: Timeout's deferred func sees
		// DeadlineExceeded and writes the 504 for us.
		return
	}
}

// stubbornHandler does the same work but never looks at the context, so the
// timeout is ignored and the handler runs to completion.
func stubbornHandler(w http.ResponseWriter, r *http.Request) {
	d := workDuration(r)
	time.Sleep(d)
	w.Write([]byte("done after " + d.String() + "\n"))
}

// selfURL is where the proxy handlers send their upstream call in the real app.
const selfURL = "http://localhost:3000"

// proxyHandler calls GET {upstream}/slow/{ms} and relays the body. The key line
// is NewRequestWithContext: the outgoing request is tied to r.Context(), so
// when Timeout's deadline passes, the HTTP client aborts the call (closes the
// connection) and Do/ReadAll return an error wrapping context.DeadlineExceeded.
func proxyHandler(upstream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		url := upstream + "/slow/" + chi.URLParam(r, "ms")
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			relayUpstreamError(w, r, err)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body) // the body read is covered by the context too
		if err != nil {
			relayUpstreamError(w, r, err)
			return
		}
		w.Write([]byte("proxied: " + string(body)))
	}
}

// proxyNoCtxHandler makes the same call with http.Get, which has no context
// and (via DefaultClient) no timeout. It will wait for the upstream however
// long that takes, ignoring Timeout's deadline.
func proxyNoCtxHandler(upstream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(upstream + "/slow/" + chi.URLParam(r, "ms"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Write([]byte("proxied: " + string(body)))
	}
}

func relayUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		// Our own deadline expired. Write nothing and let middleware.Timeout
		// send the 504. If we wrote a 502 here, ours would win.
		return
	}
	http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
}

// v2Router is an independent router — it knows nothing about its mount point.
func v2Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong from v2\n"))
	})
	return r
}

// ---------------------------------------------------------------------
// Custom middleware. The shape is always:
//
//	func(next http.Handler) http.Handler
//
// Do work before and/or after calling next.ServeHTTP, or skip next to
// short-circuit the request.
// ---------------------------------------------------------------------
func requireAPIKey(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != key {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or bad X-API-Key"})
				return // short-circuit: next is never called
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------
// A tiny todo "database" plus handlers, to show a realistic shape.
// ---------------------------------------------------------------------
type todo struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type todoStore struct {
	mu     sync.Mutex // handlers run concurrently, so guard shared state
	nextID int
	todos  map[int]todo
}

func newTodoStore() *todoStore {
	s := &todoStore{nextID: 1, todos: map[int]todo{}}
	s.add("say hello to chi")
	return s
}

func (s *todoStore) add(title string) todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := todo{ID: s.nextID, Title: title}
	s.todos[t.ID] = t
	s.nextID++
	return t
}

func (s *todoStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.todos)
}

func (s *todoStore) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]todo, 0, len(s.todos))
	for id := 1; id < s.nextID; id++ { // iterate by id for stable output
		if t, ok := s.todos[id]; ok {
			out = append(out, t)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *todoStore) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `body must be {"title": "..."}`})
		return
	}
	writeJSON(w, http.StatusCreated, s.add(in.Title))
}

// ctxKey is an unexported type so our context key can't collide with
// keys from other packages.
type ctxKey struct{}

// todoCtx is route-level middleware: parse {id}, look it up, and either
// 404 early or hand the todo to downstream handlers via the context.
func (s *todoStore) todoCtx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be a number"})
			return
		}
		s.mu.Lock()
		t, ok := s.todos[id]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "todo not found"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, t)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *todoStore) get(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(ctxKey{}).(todo) // put there by todoCtx
	writeJSON(w, http.StatusOK, t)
}

func (s *todoStore) delete(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(ctxKey{}).(todo)
	s.mu.Lock()
	delete(s.todos, t.ID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
