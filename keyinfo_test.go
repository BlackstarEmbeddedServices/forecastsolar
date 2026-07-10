package forecastsolar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realInfoBody mirrors the actual /info response shape observed live: message.info is an empty
// ARRAY and message.ratelimit is null (both differ from the /estimate object shapes).
const realInfoBody = `{
  "result": {
    "payer": "NVGXKNDZHSFNJ",
    "email": "user@example.com",
    "name": "Test User",
    "subscription": "I-XYZ",
    "apikey": "SECRET",
    "level": 3,
    "strings": 3,
    "account": "Professional",
    "until": "2026-07-19",
    "created": "2026-07-05 18:09:13"
  },
  "message": {"code": 0, "type": "success", "text": "", "pid": "abc", "info": [], "ratelimit": null}
}`

func TestKeyInfoParsesRealSchema(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(realInfoBody))
	}))
	defer srv.Close()

	c := &Client{Key: "SECRET", BaseURL: srv.URL}
	ki, err := c.KeyInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/SECRET/info" {
		t.Errorf("info path = %q, want /SECRET/info", gotPath)
	}
	if ki.Account != "Professional" || ki.Level != 3 || ki.Strings != 3 {
		t.Errorf("got account=%q level=%d strings=%d, want Professional/3/3", ki.Account, ki.Level, ki.Strings)
	}
	if ki.Name != "Test User" {
		t.Errorf("name = %q", ki.Name)
	}
	if ki.Until.IsZero() || ki.Until.Format("2006-01-02") != "2026-07-19" {
		t.Errorf("until = %v (raw %q), want 2026-07-19", ki.Until, ki.UntilRaw)
	}
	if ki.IsEnterprise() {
		t.Errorf("Professional must not report IsEnterprise")
	}
	// /info's result.strings authoritatively sets planes-per-call before any estimate call.
	if got := c.PlanesPerCall(); got != 3 {
		t.Errorf("PlanesPerCall after KeyInfo = %d, want 3 (from result.strings)", got)
	}
}

func TestKeyInfoEnterprise(t *testing.T) {
	body := `{"result":{"account":"Enterprise","level":9,"strings":4,"until":"2027-01-01"},"message":{"info":[],"ratelimit":null}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL}
	ki, err := c.KeyInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ki.IsEnterprise() {
		t.Errorf("account %q should report IsEnterprise", ki.Account)
	}
	if c.PlanesPerCall() != 4 {
		t.Errorf("enterprise strings=4 → PlanesPerCall %d, want 4", c.PlanesPerCall())
	}
}

// TestLazyPlanDetect verifies the default keyed path: the first fetch does one /info lookup, learns
// planes-per-call from result.strings (3), and therefore batches 4 planes into ⌈4/3⌉=2 calls on the
// very first Estimate — no bootstrap-at-1. It also caches the plan for Plan().
func TestLazyPlanDetect(t *testing.T) {
	var infoHits, estHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[len(r.URL.Path)-5:] == "/info" {
			infoHits++
			w.Write([]byte(realInfoBody)) // strings=3, account=Professional
			return
		}
		estHits++
		w.Write([]byte(estimateBody(sumKwp(r.URL.Path), 300, 250)))
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL} // auto-detect ON (default)
	planes := []Plane{{Kwp: 1}, {Kwp: 2}, {Kwp: 3}, {Kwp: 4}}
	_, meta, err := c.Estimate(context.Background(), 52, 5, planes)
	if err != nil {
		t.Fatal(err)
	}
	if infoHits != 1 {
		t.Errorf("info hits = %d, want exactly 1 (one-time lazy lookup)", infoHits)
	}
	if meta.Calls != 2 { // 4 planes / 3-per-call = 2, from the very first fetch
		t.Errorf("first-fetch Calls = %d, want 2 (planes from /info, no bootstrap)", meta.Calls)
	}
	if meta.PlanesPerCall != 3 {
		t.Errorf("PlanesPerCall = %d, want 3 (from result.strings)", meta.PlanesPerCall)
	}
	if ki, ok := c.Plan(); !ok || ki.Account != "Professional" {
		t.Errorf("Plan() = %+v ok=%v, want cached Professional", ki, ok)
	}
	// A second fetch must not repeat /info.
	if _, _, err := c.Estimate(context.Background(), 52, 5, planes); err != nil {
		t.Fatal(err)
	}
	if infoHits != 1 {
		t.Errorf("info hits after 2nd fetch = %d, want still 1", infoHits)
	}
	_ = estHits
}

// TestKeyInfoRawRedactsKey: /info echoes the apikey; the retained Raw body must not carry the secret.
func TestKeyInfoRawRedactsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(realInfoBody)) // contains "apikey": "SECRET"
	}))
	defer srv.Close()

	c := &Client{Key: "SECRET", BaseURL: srv.URL}
	ki, err := c.KeyInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ki.Raw), `"SECRET"`) {
		t.Errorf("apikey leaked in Raw: %s", ki.Raw)
	}
	if !strings.Contains(string(ki.Raw), "REDACTED") {
		t.Errorf("want REDACTED in Raw, got: %s", ki.Raw)
	}
}

func TestKeyInfoRequiresKey(t *testing.T) {
	c := &Client{}
	if _, err := c.KeyInfo(context.Background()); err == nil {
		t.Errorf("KeyInfo without a key should error")
	}
}

// TestEstimateToleratesInfoArray guards the estimate path against a body whose message.info is an
// array (not the usual object) — parsing must not fail wholesale.
func TestEstimateToleratesInfoArray(t *testing.T) {
	body := `{"result":{"watts":{"1752130800":500},"watt_hours_period":{"1752130800":125}},"message":{"info":[],"ratelimit":{"limit":300,"remaining":200}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{Key: "K", BaseURL: srv.URL, SkipPlanDetect: true}
	pts, meta, err := c.Estimate(context.Background(), 52, 5, []Plane{{Kwp: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Watts != 500 {
		t.Errorf("points = %+v, want one 500W point", pts)
	}
	if meta.RateLimit.Limit != 300 {
		t.Errorf("limit = %d, want 300", meta.RateLimit.Limit)
	}
}
