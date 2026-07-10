// Package forecastsolar is a shared client for the api.forecast.solar PV-production
// forecast API. It implements the API's operational requirements once — 429 retry-at
// self-pause, Accept/User-Agent headers, epoch (time=seconds) timestamps, multi-plane
// batching, and inference of the per-plan planes-per-call — so the several services that
// consume forecast.solar don't each re-implement (and mis-implement) them.
//
// A Client is safe for one API key (free/keyless when Key==""). Its rate-discipline state
// (the 429 self-pause and the optional MaxCallsPerHour limiter) is shared across every
// Estimate/ClearSky call, which is correct: forecast.solar rate-limits per key / per IP, so
// one shared Client for a whole fleet of installs behind one key honours the one true limit.
// Scheduling, caching, and any fleet-wide (store-persisted) call budget stay with the caller.
package forecastsolar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the production endpoint. Override Client.BaseURL for tests/proxies.
const DefaultBaseURL = "https://api.forecast.solar"

// DefaultUserAgent identifies this client to the API (forecast.solar recommends a UA).
const DefaultUserAgent = "forecastsolar-go/1 (+github.com/BlackstarEmbeddedServices/forecastsolar)"

// Plane is one PV array plane. Az uses the forecast.solar convention: 0=South, -90=East,
// 90=West. Dec is the tilt/declination 0..90. Kwp is the plane's DC peak power.
type Plane struct {
	Dec float64
	Az  float64
	Kwp float64
}

// Point is one forecast sample at an API timestamp (always UTC here). Watts is the API's
// result.watts (average power over the period ending at Ts); WhPeriod is result.watt_hours_period
// (energy produced since the previous stamp). Both are summed across planes/batches.
type Point struct {
	Ts       time.Time
	Watts    float64
	WhPeriod float64
}

// RateLimit is the server-reported rolling-hour quota, read from the body message.ratelimit
// and/or the X-Ratelimit-* headers. RetryAt is zero when unknown.
type RateLimit struct {
	Limit     int
	Remaining int
	RetryAt   time.Time
}

// Meta accompanies every Estimate/ClearSky result. Calls is the number of upstream API calls
// actually made (counted even across a mid-batch failure) so callers can do external budget
// bookkeeping.
type Meta struct {
	Timezone      string
	RateLimit     RateLimit
	Calls         int
	PlanesPerCall int // the planes-per-call the client used for this result
}

// RateLimited is returned (typed) when the client declines to call upstream because it is
// self-paused after a 429 (Reason "server-429") or because MaxCallsPerHour would be exceeded
// (Reason "local-cap"). RetryAt is when the caller may retry. Callers should treat it as a
// skip and keep their last-known-good forecast.
type RateLimited struct {
	RetryAt time.Time
	Reason  string
}

func (e *RateLimited) Error() string {
	d := time.Until(e.RetryAt).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("forecastsolar: rate limited (%s), retry in %s (at %s)", e.Reason, d, e.RetryAt.UTC().Format(time.RFC3339))
}

// Client talks to one forecast.solar key (or the keyless free tier when Key==""). The zero
// value is usable except for Key; set BaseURL/HTTP/UserAgent/PlanesOverride/MaxCallsPerHour as
// needed. A Client is safe for concurrent use.
type Client struct {
	// Key is the API key; "" selects the keyless public (free) tier.
	Key string
	// PlanesOverride, when >0, forces the planes-per-call instead of inferring it from the
	// response limit. It exists for the two tiers whose planes-per-call can't be inferred from
	// the rolling-hour limit: Personal Plus (limit 60 → 2 planes) and Enterprise (→ 4 planes).
	PlanesOverride int
	// BaseURL overrides the endpoint (tests/proxies); "" → DefaultBaseURL.
	BaseURL string
	// HTTP is the http client; nil → a 30s-timeout client.
	HTTP *http.Client
	// UserAgent is sent on every request; "" → DefaultUserAgent.
	UserAgent string
	// MaxCallsPerHour, when >0, caps how many upstream calls this Client makes in any rolling
	// hour. On exceed, Estimate/ClearSky return *RateLimited{Reason:"local-cap"} rather than
	// call upstream. 0 disables the cap (e.g. when a caller runs its own fleet-wide budget).
	MaxCallsPerHour int
	// InterBatchDelay is slept between successive upstream calls within one Estimate/ClearSky
	// (politeness on the shared free-tier per-IP limit). 0 disables it.
	InterBatchDelay time.Duration
	// SkipPlanDetect disables the automatic startup /info lookup. By default a keyed client
	// (Key!="" and no PlanesOverride) does one /info call on its first fetch to learn the exact
	// planes-per-call, plan, and expiry (see ensurePlan). Set true if the caller drives detection
	// itself via KeyInfo, or to force pure rate-limit inference. Ignored for keyless clients.
	SkipPlanDetect bool

	mu         sync.Mutex
	inferred   int           // planes-per-call; 0 until known (treated as 1)
	pauseUntil time.Time     // 429 self-pause deadline
	calls      []time.Time   // rolling-hour call timestamps for MaxCallsPerHour
	planKnown  bool          // /info has been fetched successfully
	planInfo   KeyInfoResult // cached /info result (plan/expiry); valid when planKnown
	lastRL     RateLimit     // most recent rate limit seen on an estimate/clearsky response
	lastRLSet  bool
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// PlanesPerCall reports the planes-per-call the client will use for the next call: the override
// if set, else the value inferred so far (a safe floor of 1 until a response confirms more).
func (c *Client) PlanesPerCall() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.planesLocked()
}

// planesLocked must be called with c.mu held.
func (c *Client) planesLocked() int {
	if c.PlanesOverride > 0 {
		return c.PlanesOverride
	}
	if c.inferred > 0 {
		return c.inferred
	}
	return 1
}

// planesForLimit maps a rolling-hour call limit to the plan's planes-per-call. The fixed tiers:
// Public 12→1, Personal(+Plus) 60→1 (Plus is 2, but shares limit 60 → use PlanesOverride),
// Professional 300→3, Professional Plus 600→4. An unrecognised limit yields 0 (leave unchanged).
func planesForLimit(limit int) int {
	switch limit {
	case 12:
		return 1
	case 60:
		return 1
	case 300:
		return 3
	case 600:
		return 4
	default:
		return 0
	}
}

// Estimate fetches the weather-adjusted production forecast (/estimate) for lat/lon across all
// planes, batching ⌈len(planes)/PlanesPerCall⌉ calls and summing per-timestamp. It returns the
// merged points sorted by time and a Meta describing the calls made and the server rate limit.
func (c *Client) Estimate(ctx context.Context, lat, lon float64, planes []Plane, opts ...Option) ([]Point, Meta, error) {
	return c.fetch(ctx, "estimate", lat, lon, planes, opts...)
}

// ClearSky fetches the clear-sky ceiling (/clearsky) — the theoretical no-cloud array potential,
// NOT inverter-capped — for lat/lon across all planes. Same batching/summing/accounting as
// Estimate; the ceiling lands in Point.Watts.
func (c *Client) ClearSky(ctx context.Context, lat, lon float64, planes []Plane, opts ...Option) ([]Point, Meta, error) {
	return c.fetch(ctx, "clearsky", lat, lon, planes, opts...)
}

// Historic fetches the historic-average production forecast (the API's /history route) — a forward
// 2–7 day forecast computed from long-term/climatological weather rather than the live forecast, i.e.
// the "typical" expected production for the days ahead. Same route pattern, batching, summing, and
// ResponseFull shape as Estimate; the values land in Point.Watts / Point.WhPeriod. Paid tiers only —
// the keyless public tier returns an error ("'history' is not available with a 'Public' subscription").
func (c *Client) Historic(ctx context.Context, lat, lon float64, planes []Plane, opts ...Option) ([]Point, Meta, error) {
	return c.fetch(ctx, "history", lat, lon, planes, opts...)
}

func (c *Client) fetch(ctx context.Context, endpoint string, lat, lon float64, planes []Plane, opts ...Option) ([]Point, Meta, error) {
	if len(planes) == 0 {
		return nil, Meta{}, fmt.Errorf("forecastsolar: no planes")
	}
	ro := newReqOpts(opts)
	// Honour an active 429 self-pause before spending any call.
	c.mu.Lock()
	if now := time.Now(); now.Before(c.pauseUntil) {
		until := c.pauseUntil
		c.mu.Unlock()
		return nil, Meta{}, &RateLimited{RetryAt: until, Reason: "server-429"}
	}
	c.mu.Unlock()

	// Lazily learn the plan (planes-per-call/account/expiry) from /info on the first keyed fetch.
	// A RateLimited from /info propagates (respect the pause); any other /info failure is soft —
	// we fall back to inferring planes-per-call from the estimate response limit.
	if err := c.ensurePlan(ctx); err != nil {
		return nil, Meta{}, err
	}

	per := c.PlanesPerCall()

	// An output-capping modifier (inverter/actual) is applied by the API to the summed planes of ONE
	// request; splitting across batches and summing would multiply the cap. Refuse rather than mislead.
	if ro.capsOutput && len(planes) > per {
		return nil, Meta{}, fmt.Errorf("forecastsolar: inverter/actual caps the summed output per request, but %d planes exceed %d planes-per-call (would split across batches)", len(planes), per)
	}

	sum := map[int64]*Point{}
	var meta Meta
	meta.PlanesPerCall = per
	for start := 0; start < len(planes); start += per {
		if meta.Calls > 0 && c.InterBatchDelay > 0 {
			select {
			case <-time.After(c.InterBatchDelay):
			case <-ctx.Done():
				return nil, meta, ctx.Err()
			}
		}
		// Local rolling-hour cap: refuse rather than exceed.
		if c.MaxCallsPerHour > 0 {
			if retryAt, ok := c.capExceeded(); ok {
				return nil, meta, &RateLimited{RetryAt: retryAt, Reason: "local-cap"}
			}
		}
		batch := planes[start:min(start+per, len(planes))]
		meta.Calls++
		c.recordCall()
		pts, rl, tz, err := c.fetchBatch(ctx, endpoint, lat, lon, batch, ro.query)
		if rl.Limit > 0 || !rl.RetryAt.IsZero() {
			meta.RateLimit = rl
		}
		if tz != "" {
			meta.Timezone = tz
		}
		if err != nil {
			return nil, meta, err
		}
		for _, p := range pts {
			k := p.Ts.Unix()
			if s, ok := sum[k]; ok {
				s.Watts += p.Watts
				s.WhPeriod += p.WhPeriod
			} else {
				cp := p
				sum[k] = &cp
			}
		}
	}
	// Learn the planes-per-call from the confirmed limit (monotonic, raise-only) and remember the
	// last-seen rate limit so callers can size a fleet-wide budget from it.
	c.learn(meta.RateLimit.Limit)
	if meta.RateLimit.Limit > 0 {
		c.mu.Lock()
		c.lastRL, c.lastRLSet = meta.RateLimit, true
		c.mu.Unlock()
	}

	out := make([]Point, 0, len(sum))
	for _, p := range sum {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out, meta, nil
}

// fetchBatch performs one upstream GET for up to PlanesPerCall planes and parses it. On HTTP 429
// it records the retry-at self-pause and returns *RateLimited. rl/tz are returned even on error
// (a 4xx/5xx body may still carry ratelimit info).
func (c *Client) fetchBatch(ctx context.Context, endpoint string, lat, lon float64, batch []Plane, extra url.Values) ([]Point, RateLimit, string, error) {
	var b strings.Builder
	b.WriteString(c.baseURL())
	if c.Key != "" {
		b.WriteByte('/')
		b.WriteString(c.Key)
	}
	fmt.Fprintf(&b, "/%s/%s/%s", endpoint, num(lat), num(lon))
	for _, p := range batch {
		fmt.Fprintf(&b, "/%s/%s/%s", num(p.Dec), num(p.Az), num(p.Kwp))
	}
	// epoch timestamp keys (unambiguous, tz-independent) plus any per-request modifier params.
	q := url.Values{"time": {"seconds"}}
	for k, vs := range extra {
		q[k] = vs
	}
	b.WriteByte('?')
	b.WriteString(q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.String(), nil)
	if err != nil {
		return nil, RateLimit{}, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// A transport error is a *url.Error that stringifies with the full URL — which carries the
		// API key in its path. Redact so it can't leak through a caller's log.
		return nil, RateLimit{}, "", c.redactErr(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	rl, tz := parseRateLimit(resp.Header, body)

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAt := retryAtFloor(rl.RetryAt)
		c.pause(retryAt)
		return nil, rl, tz, &RateLimited{RetryAt: retryAt, Reason: "server-429"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, rl, tz, fmt.Errorf("forecastsolar: http %d: %s", resp.StatusCode, c.redactKey(snippet(body)))
	}

	pts, err := parseResult(body, tz)
	if err != nil {
		return nil, rl, tz, err
	}
	if len(pts) == 0 {
		return nil, rl, tz, fmt.Errorf("forecastsolar: empty %s result", endpoint)
	}
	return pts, rl, tz, nil
}

// --- rate-discipline state (all guarded by c.mu) ---

func (c *Client) pause(until time.Time) {
	c.mu.Lock()
	if until.After(c.pauseUntil) {
		c.pauseUntil = until
	}
	c.mu.Unlock()
}

// ensurePlan performs the one-time lazy /info lookup for a keyed client. It is a no-op for keyless
// clients, when PlanesOverride is set, when SkipPlanDetect is set, or once the plan is known. It
// swallows soft /info failures (falling back to rate-limit inference) but propagates a RateLimited
// so the caller respects a self-pause. It is called before batching so it runs at most once per
// Estimate/ClearSky. The /info call is not counted in Meta.Calls (it does not spend estimate budget).
func (c *Client) ensurePlan(ctx context.Context) error {
	if c.Key == "" || c.PlanesOverride > 0 || c.SkipPlanDetect {
		return nil
	}
	c.mu.Lock()
	known := c.planKnown
	c.mu.Unlock()
	if known {
		return nil
	}
	ki, err := c.KeyInfo(ctx) // sets planes from result.strings on success; self-pauses on 429
	if err != nil {
		var rl *RateLimited
		if errors.As(err, &rl) {
			return err
		}
		return nil // soft failure: fall back to estimate-limit inference
	}
	c.mu.Lock()
	c.planInfo = ki
	c.planKnown = true
	c.mu.Unlock()
	return nil
}

// Plan returns the cached /info result and whether it is known yet. Callers use it to log the plan
// name, react to Enterprise (unlimited budget), or warn on an approaching Until (expiry).
func (c *Client) Plan() (KeyInfoResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.planInfo, c.planKnown
}

// RateLimitSeen returns the most recent rolling-hour rate limit reported by an estimate/clearsky
// response, and whether one has been seen. A fleet scheduler can size its call budget from
// RateLimit.Limit (e.g. 300 → Professional) instead of hard-coding it.
func (c *Client) RateLimitSeen() (RateLimit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRL, c.lastRLSet
}

func (c *Client) learn(limit int) {
	if c.PlanesOverride > 0 || limit <= 0 {
		return
	}
	p := planesForLimit(limit)
	if p == 0 {
		return
	}
	c.mu.Lock()
	if p > c.inferred { // monotonic: raise only to a response-confirmed plan
		c.inferred = p
	}
	c.mu.Unlock()
}

func (c *Client) recordCall() {
	if c.MaxCallsPerHour <= 0 {
		return
	}
	c.mu.Lock()
	c.calls = append(c.calls, time.Now())
	c.mu.Unlock()
}

// capExceeded reports whether making another call now would exceed MaxCallsPerHour, and if so the
// time the oldest in-window call ages out (when a slot frees).
func (c *Client) capExceeded() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	kept := c.calls[:0]
	for _, t := range c.calls {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.calls = kept
	if len(c.calls) >= c.MaxCallsPerHour {
		return c.calls[0].Add(time.Hour), true
	}
	return time.Time{}, false
}

// --- parsing ---

// apiDoc is the subset of the forecast.solar JSON we read. message.info is a RawMessage because
// forecast.solar serialises it as an object {timezone,…} on data endpoints but as an empty array
// [] on /info and error responses (a PHP empty-assoc-array quirk); message.ratelimit is a pointer
// because /info returns it as null. Both would break a fixed-shape struct.
type apiDoc struct {
	Result struct {
		Watts    map[string]float64 `json:"watts"`
		WhPeriod map[string]float64 `json:"watt_hours_period"`
	} `json:"result"`
	Message struct {
		Info      json.RawMessage `json:"info"`
		Ratelimit *struct {
			Limit     int    `json:"limit"`
			Remaining int    `json:"remaining"`
			RetryAt   string `json:"retry-at"`
		} `json:"ratelimit"`
	} `json:"message"`
}

// timezoneOf extracts message.info.timezone, tolerating info being an array ([]), null, or absent.
func timezoneOf(info json.RawMessage) string {
	var o struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(info, &o) == nil {
		return o.Timezone
	}
	return ""
}

func parseResult(body []byte, tz string) ([]Point, error) {
	var doc apiDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("forecastsolar: parse: %w", err)
	}
	loc := time.UTC
	if tz == "" {
		tz = timezoneOf(doc.Message.Info)
	}
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	out := make([]Point, 0, len(doc.Result.Watts))
	for k, w := range doc.Result.Watts {
		ts, err := parseTimeKey(k, loc)
		if err != nil {
			return nil, fmt.Errorf("forecastsolar: time key %q: %w", k, err)
		}
		out = append(out, Point{Ts: ts, Watts: w, WhPeriod: doc.Result.WhPeriod[k]})
	}
	return out, nil
}

// parseRateLimit reads the rate-limit fields from the X-Ratelimit-* headers (authoritative) with
// a fallback to the body's message.ratelimit, and the timezone from the body. It tolerates a
// missing body (headers still parse). retry-at may be an RFC3339 timestamp or epoch seconds.
func parseRateLimit(h http.Header, body []byte) (RateLimit, string) {
	var rl RateLimit
	var tz string
	if len(body) > 0 {
		var doc apiDoc
		if json.Unmarshal(body, &doc) == nil {
			if r := doc.Message.Ratelimit; r != nil {
				rl.Limit = r.Limit
				rl.Remaining = r.Remaining
				rl.RetryAt = parseRetryAt(r.RetryAt)
			}
			tz = timezoneOf(doc.Message.Info)
		}
	}
	if v := h.Get("X-Ratelimit-Limit"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			rl.Limit = n
		}
	}
	if v := h.Get("X-Ratelimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			rl.Remaining = n
		}
	}
	if v := h.Get("X-Ratelimit-Retry-At"); v != "" {
		if t := parseRetryAt(v); !t.IsZero() {
			rl.RetryAt = t
		}
	}
	if v := h.Get("Retry-After"); v != "" && rl.RetryAt.IsZero() {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			rl.RetryAt = time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	return rl, tz
}

// parseRetryAt accepts RFC3339 / "2006-01-02 15:04:05" / epoch seconds; "" → zero time.
func parseRetryAt(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(sec, 0).UTC()
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// parseTimeKey accepts epoch seconds (time=seconds) or "2006-01-02 15:04:05" in loc.
func parseTimeKey(k string, loc *time.Location) (time.Time, error) {
	if sec, err := strconv.ParseInt(k, 10, 64); err == nil {
		return time.Unix(sec, 0).UTC(), nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", k, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// num renders a coordinate/plane value compactly for the URL path (no trailing zeros).
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// minRateLimitPause floors the 429 self-pause: even if the server's retry-at is absent, in the past
// (clock skew), or immediate, we back off at least this long — the forecast.solar FAQ warns that
// honouring a stale retry-at can spin an "infinite 429 loop".
const minRateLimitPause = 60 * time.Second

// retryAtFloor returns retryAt bounded below by now+minRateLimitPause (and to that floor when zero).
func retryAtFloor(retryAt time.Time) time.Time {
	floor := time.Now().Add(minRateLimitPause)
	if retryAt.Before(floor) {
		return floor
	}
	return retryAt
}

// redactKey removes the API key from a string — defence against leaking it through a logged error or
// an echoed response body (the /info body contains the apikey). No-op for the keyless tier.
func (c *Client) redactKey(s string) string {
	if c.Key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Key, "REDACTED")
}

// sanitizedErr wraps an error so its message has the API key redacted (a *url.Error stringifies with
// the key-bearing URL) while still supporting errors.Is/As on the underlying error.
type sanitizedErr struct {
	err error
	key string
}

func (e *sanitizedErr) Error() string { return strings.ReplaceAll(e.err.Error(), e.key, "REDACTED") }
func (e *sanitizedErr) Unwrap() error { return e.err }

// redactErr wraps err so its rendered message can't leak the key; nil and keyless pass through.
func (c *Client) redactErr(err error) error {
	if err == nil || c.Key == "" {
		return err
	}
	return &sanitizedErr{err: err, key: c.Key}
}
