package forecastsolar

import (
	"math"
	"time"
)

// SunUp reports whether the sun is above the horizon at t (or within ±slack of being up) at
// lat/lon, using the NOAA solar-position approximation. It lets schedulers skip pointless
// nighttime forecast refreshes; slack keeps fetching through dawn/dusk. Degrades gracefully at
// polar latitudes (no panic).
func SunUp(lat, lon float64, t time.Time, slack time.Duration) bool {
	for _, tt := range []time.Time{t.Add(-slack), t, t.Add(slack)} {
		if SolarElevation(lat, lon, tt) > 0 {
			return true
		}
	}
	return false
}

// SolarElevation returns the sun's elevation in degrees at lat/lon and time t (NOAA general
// solar-position calculation).
func SolarElevation(lat, lon float64, t time.Time) float64 {
	u := t.UTC()
	rad := math.Pi / 180
	// Fractional year (radians).
	g := 2 * math.Pi / 365 * (float64(u.YearDay()) - 1 + (float64(u.Hour())-12)/24)
	decl := 0.006918 - 0.399912*math.Cos(g) + 0.070257*math.Sin(g) - 0.006758*math.Cos(2*g) +
		0.000907*math.Sin(2*g) - 0.002697*math.Cos(3*g) + 0.00148*math.Sin(3*g)
	eqtime := 229.18 * (0.000075 + 0.001868*math.Cos(g) - 0.032077*math.Sin(g) -
		0.014615*math.Cos(2*g) - 0.040849*math.Sin(2*g))
	tst := float64(u.Hour()*60+u.Minute()) + float64(u.Second())/60 + eqtime + 4*lon
	ha := (tst/4 - 180) * rad // hour angle
	cosZenith := math.Sin(lat*rad)*math.Sin(decl) + math.Cos(lat*rad)*math.Cos(decl)*math.Cos(ha)
	cosZenith = math.Max(-1, math.Min(1, cosZenith))
	return 90 - math.Acos(cosZenith)/rad
}
