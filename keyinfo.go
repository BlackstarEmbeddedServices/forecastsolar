package forecastsolar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// KeyInfoResult is what KeyInfo returns, decoded from /{key}/info. Account is the exact plan name
// as forecast.solar reports it ("Public", "Personal", "Personal Plus", "Professional",
// "Professional Plus", "Enterprise") — the one unambiguous way to identify an Enterprise key,
// which the rolling-hour limit alone cannot distinguish. Strings is the authoritative
// planes-per-call for the plan. Until is the subscription expiry (so callers can warn before it
// lapses). Raw preserves the full body.
type KeyInfoResult struct {
	Account      string    // plan name, e.g. "Professional" / "Enterprise"
	Level        int       // plan level (Professional = 3)
	Strings      int       // planes-per-call the key allows in one request
	Name         string    // account holder name
	Email        string    // account email
	Subscription string    // subscription id
	Until        time.Time // subscription expiry (zero if unparseable/absent)
	UntilRaw     string    // the raw "until" string as returned
	RateLimit    RateLimit // usually empty on /info (ratelimit is null there)

	Raw json.RawMessage
}

// IsEnterprise reports whether this is an Enterprise key (no fixed call limit — pay-per-use).
func (k KeyInfoResult) IsEnterprise() bool {
	return strings.EqualFold(strings.TrimSpace(k.Account), "Enterprise")
}

// KeyInfo queries /{key}/info to read the key's plan/limit/expiry without spending a forecast
// call (the /info endpoint reports message.ratelimit as null — it does not consume the estimate
// budget). It requires a Key (the keyless tier has no /info). On success it learns the exact
// planes-per-call from result.strings (authoritative — overrides the rate-limit inference). On
// HTTP 429 it self-pauses and returns *RateLimited like the forecast calls do.
func (c *Client) KeyInfo(ctx context.Context) (KeyInfoResult, error) {
	if c.Key == "" {
		return KeyInfoResult{}, fmt.Errorf("forecastsolar: KeyInfo requires an API key")
	}
	url := fmt.Sprintf("%s/%s/info", c.baseURL(), c.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return KeyInfoResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// The URL carries the key in its path; redact so a *url.Error can't leak it via logs.
		return KeyInfoResult{}, c.redactErr(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	rl, _ := parseRateLimit(resp.Header, body)
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAt := retryAtFloor(rl.RetryAt)
		c.pause(retryAt)
		return KeyInfoResult{RateLimit: rl}, &RateLimited{RetryAt: retryAt, Reason: "server-429"}
	}
	if resp.StatusCode != http.StatusOK {
		return KeyInfoResult{RateLimit: rl}, fmt.Errorf("forecastsolar: info http %d: %s", resp.StatusCode, c.redactKey(snippet(body)))
	}

	var doc struct {
		Result struct {
			Account      string `json:"account"`
			Level        int    `json:"level"`
			Strings      int    `json:"strings"`
			Name         string `json:"name"`
			Email        string `json:"email"`
			Subscription string `json:"subscription"`
			Until        string `json:"until"`
		} `json:"result"`
	}
	// Redact the key from the retained raw body: /info echoes the apikey field, so an unredacted Raw
	// would leak the secret to any caller that logs it. Replacing the string value keeps valid JSON.
	rawRedacted := json.RawMessage(c.redactKey(string(body)))
	if err := json.Unmarshal(body, &doc); err != nil {
		return KeyInfoResult{RateLimit: rl, Raw: rawRedacted}, fmt.Errorf("forecastsolar: info parse: %w", err)
	}
	res := KeyInfoResult{
		Account:      doc.Result.Account,
		Level:        doc.Result.Level,
		Strings:      doc.Result.Strings,
		Name:         doc.Result.Name,
		Email:        doc.Result.Email,
		Subscription: doc.Result.Subscription,
		UntilRaw:     doc.Result.Until,
		RateLimit:    rl,
		Raw:          rawRedacted,
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(doc.Result.Until)); err == nil {
		res.Until = t
	}
	// Authoritative: /info states exactly how many planes-per-call the key allows.
	c.setPlanes(res.Strings)
	return res, nil
}

// setPlanes records an authoritative planes-per-call (from /info's result.strings). Unlike the
// rate-limit inference (learn, monotonic raise-only), this sets the value directly — /info is the
// source of truth, so it may lower as well as raise (e.g. on a plan downgrade). PlanesOverride
// still wins. A non-positive n is ignored.
func (c *Client) setPlanes(n int) {
	if c.PlanesOverride > 0 || n <= 0 {
		return
	}
	c.mu.Lock()
	c.inferred = n
	c.mu.Unlock()
}
