# Perplexity Search Suggestions Service (Go)

A small HTTP service that returns the same autocomplete suggestions a user
would see on [perplexity.ai](https://www.perplexity.ai) for any partial
query, including non-Latin scripts (Cyrillic, CJK, emoji).

```
GET /suggest?q=where+to+buy+a+good
→ {
    "query": "where to buy a good",
    "suggestions": [
      "where to buy a good sofa in moscow reviews",
      "where to buy a good sofa spb reviews",
      "where to buy a good coffee in nha trang",
      "where to buy a good chinese tea",
      "where to buy a good umbrella",
      "where to buy a good sofa"
    ],
    "source": "api",
    "latency_ms": 187
  }
```

---

## Quickstart

```bash
git clone <repo>
cd perplexity-suggest
cp .env.example .env        # optional — defaults work out of the box
go mod tidy
make run                    # listens on :8080
```

Test it:
```bash
curl 'http://localhost:8080/suggest?q=where+to+buy+a+good'
curl 'http://localhost:8080/suggest?q=%D0%B3%D0%B4%D0%B5+%D0%BA%D1%83%D0%BF%D0%B8%D1%82%D1%8C'   # "где купить"
curl 'http://localhost:8080/suggest?q=%E6%9D%B1%E4%BA%AC%E3%81%A7'                              # "東京で"
curl 'http://localhost:8080/health'
```

Requirements: **Go 1.22+**. No external services, no API keys, no paid SaaS.

---

## Approach: reverse-engineered WebSocket

Perplexity's suggestion endpoint is **not** the kind of REST/XHR call you'd
expect. After inspecting traffic in DevTools, the suggestions come from a
persistent **WebSocket** at:

```
wss://suggest.perplexity.ai/suggest/ws
```

### Wire protocol

One TCP connection is multiplexed across many concurrent suggestion
requests, correlated by UUID.

**Client → Server** (JSON object):
```json
{"q": "where to buy a good", "uuid": "<uuid-v4>", "full_completion": true}
```

**Server → Client** (JSON positional array):
```json
["where to buy a good", ["sugg1", "sugg2", ...], "<uuid-v4>", ...]
```
- index 0 — echoed query
- index 1 — suggestions array
- index 2 — UUID that correlates the response back to a request

### Why this approach

| | Reverse-engineered WebSocket (this) | Headless browser (the alternative) |
|---|---|---|
| Warm-path latency | ~150–500 ms | 2–5 s |
| Resource cost | One TCP connection | A Chromium process |
| Fragility to UI changes | Low | High |
| Fragility to anti-bot changes | Medium | Low |
| Defensibility on the follow-up call | "I understood the protocol" | "I scraped the DOM" |

The WebSocket route fits the assignment's latency budget comfortably (< 1.5 s
median) and is materially cheaper to run. If Perplexity adds CAPTCHA or
rotates a required token, the fallback is to switch to a headless-browser
provider — the `suggest.Provider` interface is already shaped for that swap.

---

## Architecture

```
HTTP request
   │
   ▼
┌──────────────────────────────────┐
│  internal/httpapi.Handler        │  validates input, maps errors,
│                                  │  formats the spec response shape
└──────────────────────────────────┘
   │  (suggest.Provider interface)
   ▼
┌──────────────────────────────────┐
│  internal/suggest/ws.Client      │  multiplexed WebSocket client:
│                                  │  - one reader goroutine
│   pending: map[uuid]chan result  │  - serialized writes (mutex)
│           │                      │  - per-request retry + backoff
│  readLoop ┘ dispatches by uuid   │  - auto-reconnect with backoff
└──────────────────────────────────┘
   │
   ▼  wss://suggest.perplexity.ai/suggest/ws
```

### Concurrency model

- **One reader goroutine** drains the socket and dispatches each frame to
  the waiting caller via a per-request buffered channel keyed by UUID.
- **Writes are serialized** by a mutex. (Gorilla WebSocket's documented
  rule: only one goroutine may write at a time.)
- **The pending map** is guarded by a `sync.RWMutex` — dispatch (the hot
  path) takes a read lock.
- **Reconnect** runs in its own goroutine, gated by an `atomic.Bool` CAS so
  two reconnect loops can't run at once.
- **Graceful shutdown** flows from `signal.NotifyContext` in `main` through
  `http.Server.Shutdown` and finally `Client.Close`, which fails any
  remaining pending requests.

### Why these choices

- **Lazy initial connect.** If Perplexity is down at startup, the service
  still starts; the reconnect loop keeps trying and requests return
  `503 service_unavailable` in the meantime. Failing the process at boot
  would make this service brittle to upstream blips.
- **Empty array on no suggestions, not `null`.** Spec requirement — also
  what every reasonable JS client will expect.
- **`utf8.RuneCountInString` for the 200-char limit.** A single emoji is
  one user-visible character but 4 bytes; counting bytes would reject
  legitimate Cyrillic / CJK / emoji queries.
- **Sentinel errors + `errors.Is`.** Internal errors carry semantic class
  (`ErrTimeout`, `ErrUpstream`, `ErrBlocked`, `ErrUnavailable`) and the
  HTTP layer maps them to status codes in one place.
- **No caching of suggestion content.** The spec is explicit: responses
  must reflect what Perplexity returns *at request time*.

---

## Configuration

All configuration is via environment variables. No secrets are committed;
no API keys are required.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `PERPLEXITY_WS_URL` | `wss://suggest.perplexity.ai/suggest/ws` | Upstream endpoint |
| `ORIGIN` | `https://www.perplexity.ai` | Required WebSocket `Origin` header |
| `USER_AGENT` | Chrome desktop UA | Sent on the WS handshake |
| `WS_DIAL_TIMEOUT` | `10s` | TCP+TLS+WS handshake budget |
| `REQUEST_TIMEOUT` | `5s` | Per-request deadline |
| `MAX_RETRIES` | `2` | Retries on transient failures |
| `RECONNECT_DELAY` | `2s` | Base reconnect backoff |
| `MAX_RECONNECT_ATTEMPTS` | `5` | Reconnect attempts before giving up |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

---

## Error model

| HTTP | `error` code | When |
|---|---|---|
| 200 | — | Success (empty `suggestions: []` is still 200) |
| 400 | `invalid_input` | Missing `q`, > 200 runes, bad UTF-8 |
| 403 | `upstream_blocked` | Perplexity returned a block / challenge |
| 502 | `bad_gateway` | Upstream protocol or write error |
| 503 | `service_unavailable` | WebSocket not connected (reconnecting) |
| 504 | `upstream_timeout` | No response within `REQUEST_TIMEOUT` |
| 500 | `internal_error` | Unexpected (panic-recovered) |

Error body:
```json
{ "error": "upstream_timeout", "message": "no response within 5s" }
```

---

## Example invocations

### 1. English query

```bash
$ curl -s 'http://localhost:8080/suggest?q=where+to+buy+a+good' | jq
{
  "query": "where to buy a good",
  "suggestions": [
    "where to buy a good sofa in moscow reviews",
    "where to buy a good sofa spb reviews",
    "where to buy a good coffee in nha trang",
    "where to buy a good chinese tea",
    "where to buy a good umbrella",
    "where to buy a good sofa"
  ],
  "source": "api",
  "latency_ms": 187
}
```

### 2. Cyrillic query

```bash
$ curl -s --data-urlencode 'q=где купить' --get http://localhost:8080/suggest | jq
{
  "query": "где купить",
  "suggestions": [...],
  "source": "api",
  "latency_ms": 213
}
```

### 3. No suggestions / weird input

```bash
$ curl -s 'http://localhost:8080/suggest?q=qzqzqzqz' | jq
{
  "query": "qzqzqzqz",
  "suggestions": [],
  "source": "api",
  "latency_ms": 124
}
```

### 4. Validation error

```bash
$ curl -si 'http://localhost:8080/suggest?q=' | head -1
HTTP/1.1 400 Bad Request
```

---

## Tests

```bash
make test
```

Covered:
- Input validation: empty, oversized, valid UTF-8 across scripts, invalid UTF-8.
- WebSocket message dispatch: correct UUID routing, tolerance to malformed frames.
- Fast-fail when disconnected.

Not covered (intentionally — keep scope tight):
- Live integration against Perplexity (flaky, rate-limited, slow).
- Full end-to-end via a fake WebSocket server (worth adding next).

---

## Known limitations & what's next with more time

1. **Single upstream socket.** Under heavy concurrency a connection pool
   would be safer. The current design serializes writes, which becomes a
   bottleneck at very high QPS. A small pool (e.g. 4 sockets, round-robin)
   would scale better.
2. **No hybrid fallback.** If Perplexity rotates required headers or adds a
   challenge, the service goes down. The next iteration would add a
   browser-driven provider (`chromedp`) and auto-fall-back from
   `ErrBlocked` to that provider.
3. **Limited observability.** A `/metrics` endpoint with request counts,
   latency histograms, and reconnect counters would be one short PR.
4. **Wire format assumption.** Suggestions are assumed to be plain strings
   at index 1. If Perplexity starts returning objects, dispatch would
   produce an empty array — visible in logs but not an error.
5. **No fake WebSocket server in tests.** A `httptest`-style fake would let
   us cover reconnect, dispatch under load, and the retry path end to end.

---

