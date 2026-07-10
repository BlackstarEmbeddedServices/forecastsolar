package forecastsolar

import (
	"net/url"
	"strconv"
)

// Option is a per-request modifier for Estimate / ClearSky / Historic. Every modifier defaults OFF —
// with no options a request behaves exactly as the bare call. Options map to forecast.solar query
// parameters. Availability of some parameters depends on the account tier; an unsupported one surfaces
// as a normal request error.
type Option func(*reqOpts)

type reqOpts struct {
	query url.Values
	// capsOutput marks a modifier the API applies to the SUMMED output of the planes in one request
	// (inverter, actual). Because the client may split >PlanesPerCall planes across several requests
	// and sum them itself, such a modifier is only correct when all planes fit a single call — so the
	// client refuses (errors) rather than return a silently-wrong sum when it would have to split.
	capsOutput bool
}

func newReqOpts(opts []Option) *reqOpts {
	o := &reqOpts{query: url.Values{}}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	return o
}

// WithInverterKW caps the forecast to an inverter's AC limit in kW (the API's `inverter` parameter,
// which clips the production curve to this ceiling). Default off → raw, uncapped output. Per-request:
// only valid when all planes fit one call (see reqOpts.capsOutput).
func WithInverterKW(kw float64) Option {
	return func(o *reqOpts) { o.query.Set("inverter", num(kw)); o.capsOutput = true }
}

// WithActualW feeds recent actual production (watts) back to the API to self-calibrate the forecast
// (`actual`). Same single-call caveat as WithInverterKW.
func WithActualW(w float64) Option {
	return func(o *reqOpts) { o.query.Set("actual", num(w)); o.capsOutput = true }
}

// WithResolution sets the output granularity in minutes (typically 15, 30 or 60), the `resolution`
// parameter. Applied uniformly, so it is batching-safe.
func WithResolution(minutes int) Option {
	return func(o *reqOpts) { o.query.Set("resolution", strconv.Itoa(minutes)) }
}

// WithDamping applies morning/evening damping factors to shape the diurnal curve to a site
// (`damping_morning` / `damping_evening`). Batching-safe.
func WithDamping(morning, evening float64) Option {
	return func(o *reqOpts) {
		o.query.Set("damping_morning", num(morning))
		o.query.Set("damping_evening", num(evening))
	}
}

// WithHorizon applies a shading horizon (`horizon`) as a comma-separated list of horizon elevation
// degrees around the compass (the API rejects non-numeric values such as "auto"). Batching-safe (the
// same horizon applies to every plane). Example: WithHorizon("0,10,20,30,20,10,0,0,0,0,0,0").
func WithHorizon(csvDegrees string) Option {
	return func(o *reqOpts) { o.query.Set("horizon", csvDegrees) }
}

// WithParam sets an arbitrary forecast.solar query parameter — an escape hatch for options this
// package does not model as typed helpers. The caller owns the exact name/value and any batching
// implications. Passing "time" here is ignored (the client always requests epoch timestamps).
func WithParam(key, value string) Option {
	return func(o *reqOpts) {
		if key != "time" {
			o.query.Set(key, value)
		}
	}
}
