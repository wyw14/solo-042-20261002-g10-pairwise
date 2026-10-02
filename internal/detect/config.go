package detect

import (
	"errors"
	"time"
)

// Config configures the anomaly detection windows and thresholds.
// All thresholds are inclusive where applicable.
type Config struct {
	// DetectWindow is how long an anomaly must remain present before an
	// alarm is dispatched. Single-point noise never survives it.
	DetectWindow time.Duration
	// MinSamples is the minimum number of supporting samples collected
	// during DetectWindow (per primary sensor kind).
	MinSamples int
	// StaleAfter marks a probe as lost when no sample arrives for this
	// long. Stale probes do not participate in decisions.
	StaleAfter time.Duration
	// MaxWindowSamples bounds the per-vessel, per-kind sample buffer.
	MaxWindowSamples int

	// OverPressureKPa is the sustained pressure threshold (kPa).
	OverPressureKPa float64

	// Leak detection combines correlated drops across sensors.
	// A pressure drop must be one of at least two confirming signals.
	LeakPressureKPa      float64 // absolute ceiling below which pressure is considered low
	LeakPressureDropKPa  float64 // pressure fall required within the window
	LeakLevelMM          float64 // absolute ceiling below which level is considered low (0 disables)
	LeakLevelDropMM      float64 // level fall required within the window (0 disables)
	LeakTemperatureC     float64 // absolute ceiling below which temperature is considered low (0 disables)
	LeakTemperatureDropC float64 // temperature fall required within the window (0 disables)
}

func DefaultConfig() Config {
	return Config{
		DetectWindow:         30 * time.Second,
		MinSamples:           3,
		StaleAfter:           2 * time.Minute,
		MaxWindowSamples:     64,
		OverPressureKPa:      600,
		LeakPressureKPa:      80,
		LeakPressureDropKPa:  15,
		LeakLevelMM:          0,
		LeakLevelDropMM:      20,
		LeakTemperatureC:     0,
		LeakTemperatureDropC: 10,
	}
}

func (c Config) Validate() error {
	if c.DetectWindow <= 0 {
		return errors.New("detect window must be positive")
	}
	if c.MinSamples < 2 {
		return errors.New("min samples must be at least two")
	}
	if c.StaleAfter < c.DetectWindow {
		return errors.New("stale-after duration must cover the detect window")
	}
	if c.MaxWindowSamples < c.MinSamples {
		return errors.New("max window samples must cover min samples")
	}
	if c.OverPressureKPa <= 0 {
		return errors.New("over-pressure threshold must be positive")
	}
	if c.LeakPressureKPa <= 0 || c.LeakPressureDropKPa <= 0 {
		return errors.New("leak pressure thresholds must be positive")
	}
	if c.LeakLevelDropMM < 0 || c.LeakTemperatureDropC < 0 {
		return errors.New("leak drop thresholds cannot be negative")
	}
	return nil
}
