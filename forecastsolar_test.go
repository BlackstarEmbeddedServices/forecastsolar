package forecastsolar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// estimateBody builds a minimal /estimate response with two epoch-keyed slots whose watts equal
// the summed Kwp of the requested planes (so tests can assert per-plane summing) and a ratelimit.
func estimateBody(kwpSum float64, limit, remaining int) string {
	t0 := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC).Unix()
	t1 := time.Date(2026, 7, 10, 10, 15, 0, 0, time.UTC).Unix()
	doc := map[string]any{
		"result": map[string]any{
			"watts":             map[string]float64{fmt.Sprint(t0): kwpSum * 100, fmt.Sprint(t1): kwpSum * 200},
			"watt_hours_period": map[string]float64{fmt.Sprint(t0): kwpSum * 25, fmt.Sprint(t1): kwpSum * 50},
		},
		"message": map[string]any{
			"info":      map[string]any{"timezone": "Europe/Amsterdam"},
			"ratelimit": map[string]any{"limit": limit, "remaining": remaining},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func sumKwp(path string) float64 {
	// path .../estimate/lat/lon/dec/az/kwp[/dec/az/kwp...] — sum every 3rd segment starting at kwp.
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// find "estimate" or "clearsky"
	i := 0
	for ; i < len(segs); i++ {
		if segs[i] == "estimate" || segs[i] == "clearsky" {
			break
		}
	}
	planeSegs := segs[i+3:] // skip endpoint, lat, lon
	var sum float64
	for j := 2; j < len(planeSegs); j += 3 {
		var kwp float64
		fmt.Sscan(planeSegs[j], &kwp)
		sum += kwp
	}
	return sum
}

func TestEstimateKeylessURLAndSum(t *testing.T) {
	var gotPath, gotAccept, gotUA string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAccept, gotUA = r.Header.Get("Accept"), r.Header.Get("User-Agent")
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 12, 11))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL} // keyless (free) tier
	pts, meta, err := c.Estimate(context.Background(), 52.0, 5.0, []Plane{{Dec: 30, Az: 0, Kwp: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotPath, "//") || !strings.HasPrefix(gotPath, "/estimate/") {
		t.Errorf("keyless URL should be /estimate/...: got %q", gotPath)
	}
	if gotQuery != "time=seconds" {
		t.Errorf("want time=seconds query, got %q", gotQuery)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header = %q", gotAccept)
	}
	if gotUA == "" {
		t.Errorf("User-Agent header not set")
	}
	if meta.Calls != 1 {
		t.Errorf("Calls = %d, want 1", meta.Calls)
	}
	if meta.RateLimit.Limit != 12 {
		t.Errorf("limit = %d, want 12", meta.RateLimit.Limit)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 points, got %d", len(pts))
	}
	if pts[0].Watts != 400 { // kwp 4 * 100
		t.Errorf("watts = %v, want 400", pts[0].Watts)
	}
	if !pts[0].Ts.Before(pts[1].Ts) {
		t.Errorf("points not sorted by time")
	}
}

func TestEstimateKeyedURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 300, 250))
	}))
	defer srv.Close()

	c := &Client{Key: "SECRET", BaseURL: srv.URL, SkipPlanDetect: true}
	_, _, err := c.Estimate(context.Background(), 52.0, 5.0, []Plane{{Dec: 30, Az: 0, Kwp: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPath, "/SECRET/estimate/") {
		t.Errorf("keyed URL should be /SECRET/estimate/...: got %q", gotPath)
	}
}

func TestMultiPlaneBatchingAndSum(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 300, 250))
	}))
	defer srv.Close()

	// Pure rate-limit inference path (SkipPlanDetect isolates it from the /info lookup):
	// Professional (limit 300 → 3 planes). First call bootstraps at 1 plane/call → 4 calls.
	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	planes := []Plane{{Kwp: 1}, {Kwp: 2}, {Kwp: 3}, {Kwp: 4}}
	pts, meta, err := c.Estimate(context.Background(), 52, 5, planes)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Calls != 4 {
		t.Errorf("bootstrap Calls = %d, want 4 (1 plane/call until limit learned)", meta.Calls)
	}
	// Points summed across all 4 planes: total kwp 10 → watts 1000 at slot 0.
	if pts[0].Watts != 1000 {
		t.Errorf("summed watts = %v, want 1000", pts[0].Watts)
	}
	if got := c.PlanesPerCall(); got != 3 {
		t.Errorf("after a 300-limit response, PlanesPerCall = %d, want 3", got)
	}

	// Second Estimate now batches at 3 planes/call → ⌈4/3⌉ = 2 calls.
	calls.Store(0)
	_, meta2, err := c.Estimate(context.Background(), 52, 5, planes)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Calls != 2 {
		t.Errorf("second Calls = %d, want 2 (3 planes/call)", meta2.Calls)
	}
	if meta2.PlanesPerCall != 3 {
		t.Errorf("meta.PlanesPerCall = %d, want 3", meta2.PlanesPerCall)
	}
}

func TestPlanesOverride(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 60, 50)) // Personal Plus shares limit 60
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, PlanesOverride: 2} // Personal Plus → 2 planes
	planes := []Plane{{Kwp: 1}, {Kwp: 2}, {Kwp: 3}}
	_, meta, err := c.Estimate(context.Background(), 52, 5, planes)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Calls != 2 { // ⌈3/2⌉ = 2, override active from the very first call (no bootstrap)
		t.Errorf("Calls = %d, want 2 (override 2 planes/call)", meta.Calls)
	}
	// Override must not be lowered by the 60→1 inference.
	if got := c.PlanesPerCall(); got != 2 {
		t.Errorf("PlanesPerCall = %d, want 2 (override wins over inference)", got)
	}
}

func TestMonotonicInferenceNeverLowers(t *testing.T) {
	c := &Client{}
	c.learn(300) // Professional
	if c.PlanesPerCall() != 3 {
		t.Fatalf("want 3 after learn(300)")
	}
	c.learn(12) // a stray low limit must not drop us to 1
	if got := c.PlanesPerCall(); got != 3 {
		t.Errorf("PlanesPerCall = %d, want 3 (monotonic, never lowers)", got)
	}
	c.learn(600) // upgrade confirmed
	if got := c.PlanesPerCall(); got != 4 {
		t.Errorf("PlanesPerCall = %d, want 4 after learn(600)", got)
	}
}

func Test429SelfPause(t *testing.T) {
	retry := time.Now().Add(37 * time.Minute).UTC().Truncate(time.Second)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-Ratelimit-Limit", "12")
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("X-Ratelimit-Retry-At", retry.Format(time.RFC3339))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":{"ratelimit":{"limit":12,"remaining":0}}}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	var rl *RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimited, got %v", err)
	}
	if rl.Reason != "server-429" {
		t.Errorf("reason = %q, want server-429", rl.Reason)
	}
	if !rl.RetryAt.Equal(retry) {
		t.Errorf("RetryAt = %v, want %v", rl.RetryAt, retry)
	}
	// A subsequent call must be refused WITHOUT hitting the server (self-pause).
	before := hits.Load()
	_, _, err2 := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	if !errors.As(err2, &rl) {
		t.Fatalf("second call want *RateLimited, got %v", err2)
	}
	if hits.Load() != before {
		t.Errorf("self-pause should not hit the server again: hits %d→%d", before, hits.Load())
	}
}

func TestMaxCallsPerHourCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 12, 5))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, MaxCallsPerHour: 2}
	for i := 0; i < 2; i++ {
		if _, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	var rl *RateLimited
	if !errors.As(err, &rl) || rl.Reason != "local-cap" {
		t.Fatalf("want *RateLimited local-cap, got %v", err)
	}
}

func TestEpochAndLocalTimeKeys(t *testing.T) {
	// local-time keys (no time=seconds honoured by a server that ignores the query) still parse.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":{"watts":{"2026-07-10 10:00:00":500},"watt_hours_period":{"2026-07-10 10:00:00":125}},"message":{"info":{"timezone":"Europe/Amsterdam"},"ratelimit":{"limit":12,"remaining":10}}}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	pts, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// 10:00 Europe/Amsterdam (CEST, +02:00) = 08:00 UTC.
	if pts[0].Ts.Hour() != 8 || pts[0].Ts.Location() != time.UTC {
		t.Errorf("local key parsed to %v, want 08:00 UTC", pts[0].Ts)
	}
}
