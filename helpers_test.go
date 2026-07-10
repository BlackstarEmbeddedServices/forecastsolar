package forecastsolar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClearSkyEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, estimateBody(sumKwp(r.URL.Path), 300, 250))
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	pts, _, err := c.ClearSky(context.Background(), 52, 5, []Plane{{Kwp: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPath, "/K/clearsky/") {
		t.Errorf("clearsky URL = %q, want /K/clearsky/...", gotPath)
	}
	if pts[0].Watts != 200 { // kwp 2 * 100 → ceiling in Watts
		t.Errorf("clearsky watts = %v, want 200", pts[0].Watts)
	}
}

func TestJitterDeterministicInRange(t *testing.T) {
	const max = 15 * time.Minute
	a := Jitter("install-42", max)
	b := Jitter("install-42", max)
	if a != b {
		t.Errorf("Jitter not deterministic: %v vs %v", a, b)
	}
	if a < 0 || a >= max {
		t.Errorf("Jitter %v out of [0,%v)", a, max)
	}
	if Jitter("install-42", max) == Jitter("install-43", max) {
		t.Logf("note: two seeds collided (possible but unlikely)")
	}
	if Jitter("x", 0) != 0 {
		t.Errorf("Jitter with max=0 must be 0")
	}
}

func TestSunUpNightVsDay(t *testing.T) {
	// Amsterdam ~52N,5E. Noon UTC in July → sun up; midnight UTC → down.
	noon := time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)
	midnight := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	if !SunUp(52, 5, noon, 0) {
		t.Errorf("expected sun up at noon")
	}
	if SunUp(52, 5, midnight, 0) {
		t.Errorf("expected sun down at midnight UTC (01:00 local, deep night)")
	}
}
