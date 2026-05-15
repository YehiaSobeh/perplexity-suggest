// Package httpapi exposes the suggestion service over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/yourname/perplexity-suggest/internal/suggest"
)

// HealthChecker is implemented by providers that can report liveness.
// Kept as a tiny interface so the handler doesn't depend on the concrete
// WebSocket client.
type HealthChecker interface {
	IsConnected() bool
}

type Handler struct {
	provider suggest.Provider
	health   HealthChecker
	log      *slog.Logger
}

func NewHandler(p suggest.Provider, h HealthChecker, log *slog.Logger) *Handler {
	return &Handler{provider: p, health: h, log: log}
}

// Routes wires up the public endpoints with the standard middleware chain.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /suggest", h.Suggest)
	mux.HandleFunc("GET /health", h.Health)

	// Chain: requestID → accessLog → recover → mux.
	var handler http.Handler = mux
	handler = recover_(h.log)(handler)
	handler = accessLog(h.log)(handler)
	handler = requestID(handler)
	return handler
}

// Suggest handles GET /suggest?q=...
func (h *Handler) Suggest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := r.URL.Query().Get("q")

	if err := validateQuery(q); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}

	suggestions, err := h.provider.Suggest(r.Context(), q)
	if err != nil {
		status, code := mapError(err)
		h.log.Warn("suggest failed", "query", q, "err", err)
		writeError(w, status, code, err.Error())
		return
	}

	// Spec: empty array, not null, when there are no suggestions.
	if suggestions == nil {
		suggestions = []string{}
	}

	writeJSON(w, http.StatusOK, suggest.Result{
		Query:       q,
		Suggestions: suggestions,
		Source:      h.provider.Source(),
		LatencyMs:   time.Since(start).Milliseconds(),
	})
}

// Health reports whether the upstream WebSocket is currently live.
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "ok",
		"websocket_connected": h.health.IsConnected(),
	})
}

// validateQuery enforces the spec: non-empty, ≤200 chars, valid UTF-8.
// Note: 200 characters means 200 *runes*, not bytes — Cyrillic, CJK, and
// emoji each count as a single rune.
func validateQuery(q string) error {
	if q == "" {
		return errors.New("q is required")
	}
	if !utf8.ValidString(q) {
		return errors.New("q is not valid UTF-8")
	}
	if utf8.RuneCountInString(q) > 200 {
		return errors.New("q exceeds 200 characters")
	}
	return nil
}

// mapError translates internal errors to HTTP status codes + stable
// machine-readable error codes for clients.
func mapError(err error) (status int, code string) {
	switch {
	case errors.Is(err, suggest.ErrInvalidInput):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, suggest.ErrBlocked):
		return http.StatusForbidden, "upstream_blocked"
	case errors.Is(err, suggest.ErrTimeout):
		return http.StatusGatewayTimeout, "upstream_timeout"
	case errors.Is(err, suggest.ErrUnavailable):
		return http.StatusServiceUnavailable, "service_unavailable"
	case errors.Is(err, suggest.ErrUpstream):
		return http.StatusBadGateway, "bad_gateway"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream_timeout"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: fmt.Sprint(message)})
}
