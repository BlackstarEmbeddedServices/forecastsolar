# forecastsolar

A shared Go client for the [api.forecast.solar](https://doc.forecast.solar/api) PV-production
forecast API. It implements the API's operational requirements **once** so the services that consume
forecast.solar don't each re-implement (and mis-implement) them:

- **429 `retry-at` self-pause** (typed `*RateLimited`) with a minimum backoff floor — no "infinite 429 loop".
- `Accept` / `User-Agent` headers, `?time=seconds` epoch timestamps.
- **Endpoints**: `Estimate` (live weather), `ClearSky` (no-cloud ceiling), `Historic` (`/history` —
  historic-average "typical" production, paid tiers only), and `KeyInfo` (`/info`).
- **Multi-plane batching** into `⌈planes / planes-per-call⌉` requests, summed per timestamp.
- **Planes-per-call detection**: a keyed client lazily reads `/{key}/info` (`result.strings`) on its
  first fetch — authoritative, incl. exact plan name (`account`, so `Enterprise` is identifiable) and
  subscription expiry (`until`). Falls back to inferring from the rate-limit `limit` when `/info` is
  unavailable. Keyless (free public tier) uses the limit inference.
- **Optional per-call modifiers** (functional options, all default off): `WithInverterKW`, `WithActualW`
  (both cap/adjust the *summed* output per request → guarded against multi-plane batch splits),
  `WithResolution`, `WithDamping`, `WithHorizon`, and a `WithParam` escape hatch.
- Optional per-client `MaxCallsPerHour` limiter, deterministic `Jitter`, and a NOAA `SunUp` helper.
- **Secret hygiene**: the API key lives in the URL path, so the client redacts it from returned
  errors and the retained `/info` body.

The `Client` is safe for concurrent use and is designed to be shared across a fleet behind one key
(its 429 pause / plan / rate-limit state is per-client, which matches how forecast.solar rate-limits).
Scheduling, caching, and any fleet-wide (store-persisted) call budget stay with the caller.

## Usage

```go
c := &forecastsolar.Client{Key: os.Getenv("FSOLAR_KEY")} // Key=="" → keyless free tier
pts, meta, err := c.Estimate(ctx, lat, lon, []forecastsolar.Plane{{Dec: 30, Az: -15, Kwp: 7.5}})
// pts: []Point{Ts, Watts, WhPeriod}; meta.Calls: API calls made (for budget bookkeeping)
```

Consumed by `ems-datastore` (paid key, fleet dispatch) and `blackstarems` (keyless fallback).
