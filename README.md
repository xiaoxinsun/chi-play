# chi-play

A guided tour of [go-chi/chi](https://github.com/go-chi/chi) v5. `main.go` is the lesson (read it top to bottom; every route has a `curl` example in a comment).

```
make run     # start the server on :3000
make test    # go test -v ./...
make demo    # start the server, curl the basic routes, stop it
```

The global `middleware.Timeout` in the running app is **10s**. The `/proxy` routes call back into `localhost:3000`, so the server must be on that port.

Tip: add `-i -w '%{time_total}\n'` to any `curl` to see the status and elapsed time.

## Basics

| Endpoint | Try | Expected |
|---|---|---|
| `GET /` | `curl localhost:3000/` | 200, `hello, chi!` |
| `GET /hello/{name}` | `/hello/gopher` | 200, `hello, gopher!`. `{name}` matches one path segment. |
| `GET /square/{n}` (digits only) | `/square/7` | 200, `{"n":7,"square":49}` |
| | `/square/abc` | 404 JSON. The regex `[0-9]+` doesn't match, so the route isn't matched at all. |
| `GET /files/*` | `/files/a/b/c.txt` | 200, `file path: a/b/c.txt`. The wildcard takes the rest of the path. |
| `GET /greet?name=` | `/greet?name=bill` | 200, `hi bill` |
| | `/greet` | 200, `hi stranger`. Query strings are plain `net/http`, not chi. |
| `GET /panic` | `curl -i /panic` | 500. `Recoverer` catches the panic and the server stays up. |

## Todos API (`r.Route`, nested route, context middleware)

| Endpoint | Try | Expected |
|---|---|---|
| `GET /todos` | | 200, JSON list. It starts with one item (id 1). |
| `POST /todos` | `-d '{"title":"learn chi"}'` | 201, the new todo with its id |
| | `-d 'nope'` or `-d '{}'` | 400, `body must be {"title": "..."}` |
| `GET /todos/{id}` | `/todos/1` | 200, the todo, loaded once by the `todoCtx` middleware |
| | `/todos/99` | 404, `todo not found` |
| | `/todos/xyz` | 400, `id must be a number` |
| `DELETE /todos/{id}` | `-X DELETE /todos/1` | 204, no body. A `GET` of the same id then returns 404. |

## Middleware groups, mount and errors

| Endpoint | Try | Expected |
|---|---|---|
| `GET /admin/stats` | no header | 401, `missing or bad X-API-Key` (the group middleware short-circuits) |
| | `-H 'X-API-Key: secret'` | 200, `{"todos": N}` |
| `GET /v2/ping` | | 200, `pong from v2`. A separately built router attached with `Mount`. |
| unknown path | `/nope` | 404, custom JSON `no such route: /nope` |
| wrong method | `-X PATCH /` | 405, custom JSON. The path exists but not for that method. |

## Timeout demos (`ms` = simulated work in milliseconds)

`middleware.Timeout` only attaches a deadline to `r.Context()` and, after the handler returns, writes a 504 if the deadline passed. It can't interrupt a handler. The handler has to observe the context: `select` on `ctx.Done()`, or pass `r.Context()` to context-aware calls (DB queries, `http.NewRequestWithContext`, ...).

| Endpoint | Try | Expected |
|---|---|---|
| `GET /slow/{ms}` (honors the context) | `/slow/2000` | 200 after ~2s |
| | `/slow/9500` | 200 after ~9.5s, close to the limit |
| | `/slow/10000` | At the deadline. A race, so 200 or 504 are both valid. |
| | `/slow/15000` | **504 at ~10s**. The handler returns early and `Timeout` writes the 504. |
| `GET /stubborn/{ms}` (ignores the context) | `/stubborn/2000` | 200 after ~2s |
| | `/stubborn/12000` | **200 after the full 12s**. The deadline passed but nothing stopped it. |
| `GET /proxy/{ms}` (HTTP call tied to the request context) | `/proxy/2000` | 200, `proxied: done after 2s` |
| | `/proxy/15000` | **504 at ~10s**. The client call is aborted. |
| `GET /proxy-noctx/{ms}` (plain `http.Get`, no context) | `/proxy-noctx/8000` | 200 after ~8s, normal |
| | `/proxy-noctx/15000` | 200 at ~10s with an empty `proxied: ` body (see below) |

**Why `/proxy-noctx/15000` isn't "200 after 15s":** the handler ignores *our* deadline, but its upstream (`/slow`) is also behind the 10s `Timeout`, so it gives up at 10s and returns an empty 504. `proxyNoCtxHandler` never checks the upstream status, so it relays an empty body with a 200. The unit test case 4b doesn't hit this because its upstream has no timeout of its own.

**Error handling note:** when a context-aware call fails, check `r.Context().Err() != nil` and return without writing anything so `Timeout` can send the 504. If you write your own 502/500 first, yours wins. See `relayUpstreamError` in `main.go`.

## Context passing and deadlines

### How it flows

```
net/http server  ->  r.Context()   (cancelled if the client disconnects)
chi middleware   ->  r = r.WithContext(ctx with a deadline)     e.g. middleware.Timeout
your handler     ->  passes r.Context() down to everything it calls
  DB / HTTP / gRPC clients  ->  abort and return ctx.Err() when it expires
```

A `context.Context` is immutable, and deriving a child (`WithTimeout`, `WithDeadline`, `WithValue`) never loosens the parent. A child's deadline can only be the same or earlier, so the **earliest deadline wins**. That makes it safe to layer: a global request deadline, a tighter per-call timeout inside it, and a client-level timeout on top.

Three things are true about a deadline:

1. **It is a signal, not a kill switch.** Nothing interrupts your code. Something has to look at the context: a context-aware library call, a `select` on `ctx.Done()`, or a periodic `ctx.Err()` check.
2. **It propagates only where you pass it.** One `http.Get` or `db.Query` (no `Context`) in the chain breaks it.
3. **`ctx.Err()` tells you why it ended.** `context.DeadlineExceeded` means the deadline passed. `context.Canceled` means the caller went away or something cancelled it. Use `errors.Is`.

### Working with a deadline

```go
// Pass the request context into every outgoing call.
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, body)

// Read what is left.
if dl, ok := ctx.Deadline(); ok {
	remaining := time.Until(dl)
	_ = remaining
}

// Give one step a slice of the remaining time (never extends the parent).
stepCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
defer cancel() // always release the timer

// Finish something even if the caller is gone, with its own bound.
// WithoutCancel keeps the values (trace id, ...) but drops cancellation and deadline.
cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
defer cancel()
```

### Pitfalls

| Pitfall | What happens |
|---|---|
| Handler never looks at the context | The deadline passes and nothing stops it (`/stubborn`). |
| Using `http.Get`/`http.DefaultClient` without a request context | No abort and no timeout (`/proxy-noctx`). |
| Writing your own error response when the context expired | Yours goes out first, so `Timeout` can't send its 504. Check `ctx.Err()` and return. |
| Client disconnects | `r.Context()` is cancelled too. Fine for reads, dangerous for work that must complete (see below). |
| Forgetting `defer cancel()` | The timer and goroutine live until the deadline. `go vet` warns about this. |
| Storing business data in the context | Use `WithValue` only for request-scoped metadata (trace id, auth principal). |

### Design notes for latency-critical flows (e.g. payments with a short SLA)

`middleware.Timeout` is a good coarse safety net, but it is not enough on its own as the thing that enforces an SLA:

- **The clock starts when the middleware runs**, not when the message arrived. If the SLA is measured from the rail's timestamp, compute the deadline yourself (`context.WithDeadline(ctx, start.Add(sla))`) and keep a safety margin for writing the response.
- **It only acts through a 504 after the handler returns**, and a 504 reads as "failed". For a payment, a timeout after you sent a debit means the outcome is **unknown**, not failed. Return a "pending/processing" style response, make every downstream call idempotent (idempotency key), and have a way to query or reconcile the real outcome. Check the scheme rules for exactly what must be returned on timeout.
- **Split the budget across steps** and fail fast. Give screening a slice, give core banking a slice, and refuse to start a step if too little time is left.
- **Decide the point of no return.** Cancelling a request that has already sent a debit does not undo it. After that point, finish bookkeeping and any compensating action on a context detached with `context.WithoutCancel` and its own short deadline, so a client disconnect or the SLA deadline can't leave a half-done payment.
- **Use a configured `http.Client`** (dial, TLS and response-header timeouts, pooled connections) for downstream services. `DefaultClient` has none. Retries must respect the remaining budget, and a circuit breaker stops you spending the whole budget on a service that is down.
- **Also set `http.Server` timeouts** (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`) as a hard backstop.
- **Record `DeadlineExceeded` separately from other errors**, with per-step latency, so you can see which dependency is eating the budget.

Illustrative sketch of the shape (stubs for the services; compile-checked, not production code):

```go
// 1. One SLA deadline for the whole request, applied as early as possible.
func withSLA(sla time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), sla)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// 2. A slice of whatever time is left.
func budget(ctx context.Context, frac float64) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(float64(time.Until(dl))*frac))
}

func (s *Service) pay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.parse(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 3. Fail fast if there isn't enough time to do this safely.
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < 50*time.Millisecond {
		http.Error(w, "no time budget left", http.StatusServiceUnavailable)
		return
	}

	// 4. Screening: safe to abandon, nothing has happened yet.
	screenCtx, cancel := budget(ctx, 0.4)
	err = s.screen(screenCtx, p)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "screening timed out", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "rejected", http.StatusUnprocessableEntity)
		return
	}

	// 5. Core banking debit: past this point a timeout means "unknown".
	debitCtx, cancel := budget(ctx, 0.8)
	err = s.debit(debitCtx, p) // must carry an idempotency key
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Record it as pending on a detached context so it still gets
			// written, then answer "accepted/processing", not "failed".
			bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			s.markPending(bg, p)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, "debit failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

## Tests

| Command | What it covers |
|---|---|
| `go test ./...` | everything |
| `go test -run TestTimeout -v` | `/slow` vs `/stubborn` with a 500ms deadline: within, close to, at, and after the deadline |
| `go test -run TestProxyTimeout -v` | `/proxy` vs `/proxy-noctx`, including checking that the upstream saw the client disconnect |

| File | Contents |
|---|---|
| `main.go` | the app, with the lesson in comments |
| `main_test.go` | table test of the basic routes via `httptest` |
| `timeout_test.go` | timeout scenarios with a short deadline |
| `proxy_test.go` | timeout scenarios across an HTTP call to a second test server |
