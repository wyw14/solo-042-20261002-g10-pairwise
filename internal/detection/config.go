package detection

import (
	"errors"
	"time"
)

// Config tunes the anomaly detector. A condition must be supported by at least
// MinSamples fresh samples and persist for at least MinDuration before an
// alarm is dispatched; this rejects single-point spikes.
type Config struct {
	// Window is how long samples are retained for evaluation and evidence.
	Window time.Duration
	// MinDuration is how long an anomalous condition must be continuously
	// present before it qualifies for an alarm.
	MinDuration time.Duration
	// MinSamples is the minimum number of in-window samples a contributing
	// probe must report before its readings can vote.
	MinSamples int
	// MaxSampleAge treats a probe as offline when its newest sample is older
	// than this. Offline probes never vote and never block an alarm.
	MaxSampleAge time.Duration
	// PressureLimitKPa raises over_pressure when pressure stays above it.
	PressureLimitKPa float64
	// LeakLevelDropMM raises a suspected nitrogen_leak when level falls by at
	// least this much over the window while pressure stays elevated.
	LeakLevelDropMM float64
	// LeakPressureKPa is the elevated-pressure corroboration for a leak.
	LeakPressureKPa float64
	// MaxEvidenceSamples caps samples attached to a single alarm.
	MaxEvidenceSamples int
}

func DefaultConfig() Config {
	return Config{
		Window:             30 * time.Second,
		MinDuration:        10 * time.Second,
		MinSamples:         3,
		MaxSampleAge:       2 * time.Minute,
		PressureLimitKPa:   600,
		LeakLevelDropMM:    20,
		LeakPressureKPa:    300,
		MaxEvidenceSamples: 20,
	}
}

func (c Config) Validate() error {
	if c.Window <= 0 {
		return errors.New("detection window must be positive")
	}
	if c.MinDuration <= 0 {
		return errors.New("detection minimum duration must be positive")
	}
	if c.MinDuration > c.Window {
		return errors.New("detection minimum duration cannot exceed the window")
	}
	if c.MinSamples < 2 {
		return errors.New("detection requires at least two samples")
	}
	if c.MaxSampleAge <= 0 {
		return errors.New("detection probe max sample age must be positive")
	}
	if c.PressureLimitKPa <= 0 {
		return errors.New("detection pressure limit must be positive")
	}
	if c.LeakLevelDropMM <= 0 {
		return errors.New("detection leak level drop must be positive")
	}
	if c.LeakPressureKPa <= 0 {
		return errors.New("detection leak pressure threshold must be positive")
	}
	if c.MaxEvidenceSamples < 1 {
		return errors.New("detection evidence cap must be positive")
	}
	return nil
}
