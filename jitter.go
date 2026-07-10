package forecastsolar

import (
	"hash/fnv"
	"time"
)

// Jitter returns a deterministic offset in [0, max) derived from seed. forecast.solar recommends
// spreading calls with a random 0–900 s delay and not aligning them to the top of the hour; a
// scheduler adds Jitter(installID, 15*time.Minute) to each install's refresh interval so a fleet
// sharing one key doesn't stampede the API on the same tick. Deterministic per seed so an install's
// phase is stable across restarts (no thundering herd on redeploy) and unit-testable.
func Jitter(seed string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return time.Duration(h.Sum64() % uint64(max))
}
