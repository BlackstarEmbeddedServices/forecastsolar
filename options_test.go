package forecastsolar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// captureQuery serves an estimate body and records the query string of the last request.
func captureQuery(q *url.Values, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		*q = r.URL.Query()
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 300, 250))
	}))
}

func TestOptionsEncodeQueryParams(t *testing.T) {
	var q url.Values
	srv := captureQuery(&q, nil)
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	_, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}},
		WithInverterKW(3.5), WithResolution(60), WithDamping(0.4, 0.6),
		WithHorizon("0,10,20"), WithParam("foo", "bar"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"time":            "seconds",
		"inverter":        "3.5",
		"resolution":      "60",
		"damping_morning": "0.4",
		"damping_evening": "0.6",
		"horizon":         "0,10,20",
		"foo":             "bar",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("query %s = %q, want %q", k, got, v)
		}
	}
}

func TestNoOptionsOnlyTimeParam(t *testing.T) {
	var q url.Values
	srv := captureQuery(&q, nil)
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	if _, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}}); err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || q.Get("time") != "seconds" {
		t.Errorf("bare call should send only time=seconds, got %v", q)
	}
}

func TestWithParamIgnoresTime(t *testing.T) {
	var q url.Values
	srv := captureQuery(&q, nil)
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	if _, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}}, WithParam("time", "utc")); err != nil {
		t.Fatal(err)
	}
	if q.Get("time") != "seconds" {
		t.Errorf("WithParam(time) must not override epoch timestamps, got time=%q", q.Get("time"))
	}
}

// TestCapsOutputGuard: an output-capping modifier with more planes than fit one call must error
// WITHOUT hitting the API (the per-request cap can't be summed across batches).
func TestCapsOutputGuard(t *testing.T) {
	var q url.Values
	var hits atomic.Int32
	srv := captureQuery(&q, &hits)
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, PlanesOverride: 1} // 1 plane/call → 2 planes must split
	planes := []Plane{{Kwp: 1}, {Kwp: 1}}
	for _, opt := range []struct {
		name string
		o    Option
	}{{"inverter", WithInverterKW(5)}, {"actual", WithActualW(1000)}} {
		_, _, err := c.Estimate(context.Background(), 52, 5, planes, opt.o)
		if err == nil {
			t.Errorf("%s + splitting planes should error", opt.name)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("guard must refuse before any API call, got %d hits", hits.Load())
	}

	// The same modifiers are fine when all planes fit one call.
	hits.Store(0)
	if _, _, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}}, WithInverterKW(5)); err != nil {
		t.Errorf("inverter with a single plane should be allowed: %v", err)
	}
	if q.Get("inverter") != "5" {
		t.Errorf("single-plane inverter should reach the API, got inverter=%q", q.Get("inverter"))
	}
}
