package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"io"
	"testing"
	"time"
)

// newTestClient builds a Client with a discarding logger. The connection
// fields stay nil — that's fine for tests that exercise dispatch only.
func newTestClient() *Client {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Config{RequestTimeout: time.Second, MaxRetries: 0}, log)
}

// TestDispatch_RoutesByUUID verifies the receive-side multiplexer: a
// frame arriving with a known UUID resolves the matching pending channel.
func TestDispatch_RoutesByUUID(t *testing.T) {
	c := newTestClient()

	ch := make(chan suggestResult, 1)
	const id = "test-uuid-1234"
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	// Wire format: ["echoed query", [suggestions], "uuid", ...]
	frame, _ := json.Marshal([]any{
		"buy a good",
		[]string{"buy a good sofa", "buy a good chair"},
		id,
	})
	c.dispatch(frame)

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("unexpected err: %v", res.err)
		}
		want := []string{"buy a good sofa", "buy a good chair"}
		if len(res.suggestions) != len(want) {
			t.Fatalf("got %d suggestions, want %d", len(res.suggestions), len(want))
		}
		for i := range want {
			if res.suggestions[i] != want[i] {
				t.Errorf("suggestion[%d] = %q, want %q", i, res.suggestions[i], want[i])
			}
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch did not deliver to pending channel")
	}
}

// TestDispatch_TolerantToBadShape — bad messages must not panic and must
// not corrupt the pending map.
func TestDispatch_TolerantToBadShape(t *testing.T) {
	c := newTestClient()
	bad := [][]byte{
		[]byte(`not json`),
		[]byte(`{"object": "not array"}`),
		[]byte(`["only one element"]`),
		[]byte(`["q", "not-a-list", "uuid"]`),
	}
	for _, b := range bad {
		c.dispatch(b) // must not panic
	}
}

// TestSuggestOnce_ErrorsWhenNotConnected — sanity check for the
// fast-fail path used by the retry layer.
func TestSuggestOnce_ErrorsWhenNotConnected(t *testing.T) {
	c := newTestClient()
	_, err := c.suggestOnce(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error when not connected, got nil")
	}
}
