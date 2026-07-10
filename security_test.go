package forecastsolar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestKeyNotLeakedInTransportError: a *url.Error stringifies with the key-bearing URL; the client
// must redact the key before returning it (callers log errors verbatim).
func TestKeyNotLeakedInTransportError(t *testing.T) {
	const key = "SUPERSECRETKEY123"
	c := &Client{Key: key, BaseURL: "http://127.0.0.1:1", SkipPlanDetect: true} // port 1 → refused
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("API key leaked in transport error: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("want REDACTED marker, got: %v", err)
	}
	// Redaction must preserve unwrapping (errors.As still finds the underlying *url.Error).
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("redaction broke errors.As for *url.Error")
	}
}

// TestKeyNotLeakedInStatusError: a non-2xx body may echo the key (the /info body does); the snippet in
// the error must be redacted.
func TestKeyNotLeakedInStatusError(t *testing.T) {
	const key = "SUPERSECRETKEY123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"bad request for apikey %s"}`, key)
	}))
	defer srv.Close()

	c := &Client{Key: key, BaseURL: srv.URL, SkipPlanDetect: true}
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("API key leaked in status error: %v", err)
	}
}

// TestConcurrentEstimateNoRace hammers one shared Client from many goroutines (the documented
// "safe for concurrent use" contract) to shake out data races in the plan/pause/limiter state under
// `go test -race`.
func TestConcurrentEstimateNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			w.Write([]byte(realInfoBody))
			return
		}
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 300, 250))
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL} // lazy /info + inference + last-RL all exercised
	planes := []Plane{{Kwp: 1}, {Kwp: 2}, {Kwp: 3}}
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 10; j++ {
				_, _, _ = c.Estimate(context.Background(), 52, 5, planes)
				_ = c.PlanesPerCall()
				_, _ = c.Plan()
				_, _ = c.RateLimitSeen()
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// Test429FloorsStaleRetryAt: a retry-at in the past (clock skew) must not disable the backoff — the
// self-pause is floored so we can't spin an "infinite 429 loop".
func Test429FloorsStaleRetryAt(t *testing.T) {
	past := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ratelimit-Retry-At", past)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL} // keyless
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	var rl *RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimited, got %v", err)
	}
	if !rl.RetryAt.After(time.Now().Add(30 * time.Second)) {
		t.Errorf("RetryAt %v not floored — a stale/past retry-at must still enforce a minimum backoff", rl.RetryAt)
	}
	// And the self-pause must actually hold: a subsequent call is refused.
	if _, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}}); !errors.As(err, &rl) {
		t.Errorf("floored pause should still refuse the next call, got %v", err)
	}
}
