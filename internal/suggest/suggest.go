// Package suggest defines the shared types and errors used by the
// suggestion service. The Provider interface lets us swap backends
// (WebSocket API, headless browser, fake for tests) without touching
// the HTTP layer.
package suggest

import (
	"context"
	"errors"
)

// Result is the wire format returned by the HTTP layer. The field
// names and JSON shape come straight from the assignment spec.
type Result struct {
	Query       string   `json:"query"`
	Suggestions []string `json:"suggestions"`
	Source      string   `json:"source"`     // "api" or "browser"
	LatencyMs   int64    `json:"latency_ms"`
}

// Provider is anything that can turn a partial query into a list of
// autocomplete suggestions. Implementations must preserve upstream
// order and return an empty (non-nil) slice — not an error — when
// upstream returns no suggestions.
type Provider interface {
	Suggest(ctx context.Context, query string) ([]string, error)
	Source() string // "api" | "browser"
}

// Sentinel errors. HTTP handlers map these to status codes.
// Wrap with fmt.Errorf("%w: ...", ErrXxx, ...) to add context.
var (
	ErrInvalidInput = errors.New("invalid input")
	ErrUpstream     = errors.New("upstream error")
	ErrBlocked      = errors.New("upstream blocked the request")
	ErrTimeout      = errors.New("upstream timeout")
	ErrUnavailable  = errors.New("service unavailable")
)
