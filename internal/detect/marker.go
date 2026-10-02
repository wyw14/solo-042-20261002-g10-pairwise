package detect

import (
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// runMarker tracks the contiguous anomalous run of one probe. Unlike the
// sample window it is not pruned by age, so the true start of an anomaly
// survives past the detection window and decisions do not depend on sample
// spacing. A normal (in-range) reading ends the run immediately.
type runMarker struct {
	active            bool
	start             time.Time
	count             int
	headSamples       int
	baseline          float64
	hasBaseline       bool
	lastAnomalyAt     time.Time
	lastSampleAt      time.Time
	calibrationEpoch  uint64
	bindingGeneration uint64
}

func (m *runMarker) resetRun() {
	m.active = false
	m.start = time.Time{}
	m.count = 0
	m.headSamples = 0
	m.hasBaseline = false
	m.baseline = 0
}

// classify inspects the newest reading of a probe window and reports whether
// it is anomalous, when the episode began (the threshold breach itself for
// level conditions, the pre-drop baseline reading for leak conditions), and
// how many non-anomalous head samples (e.g. the baseline reading) are part of
// the episode.
type classify func(view []model.Sample, newest model.Sample) (anomalous bool, episodeStart time.Time, headSamples int)

func (m *runMarker) update(samples []model.Sample, connected bool, now time.Time, staleAfter time.Duration, judge classify) {
	if !connected || len(samples) == 0 {
		return
	}
	newest := samples[len(samples)-1]
	if now.Sub(newest.ObservedAt) > staleAfter {
		return
	}
	// A calibration or rebinding generation change invalidates any prior run.
	genInitialized := m.lastSampleAt != (time.Time{})
	if genInitialized && (newest.CalibrationEpoch != m.calibrationEpoch || newest.BindingGeneration != m.bindingGeneration) {
		m.resetRun()
		m.lastAnomalyAt = time.Time{}
	}
	m.calibrationEpoch = newest.CalibrationEpoch
	m.bindingGeneration = newest.BindingGeneration

	if newest.ObservedAt.Equal(m.lastSampleAt) {
		return // already evaluated this reading (e.g. periodic sweep)
	}
	m.lastSampleAt = newest.ObservedAt

	anomalous, episodeStart, headSamples := judge(samples, newest)
	if !anomalous {
		m.resetRun()
		return
	}
	if !m.active {
		m.active = true
		m.start = episodeStart
		if m.start.IsZero() {
			m.start = newest.ObservedAt
		}
		m.count = 1
		m.headSamples = headSamples
	} else {
		m.count++
	}
	m.lastAnomalyAt = newest.ObservedAt
}

// matured reports whether an active run has lasted across the detection
// window with enough samples (including any episode head, e.g. the baseline
// reading of a leak).
func (m *runMarker) matured(now time.Time, window time.Duration, minSamples int) bool {
	return m.active && m.count+m.headSamples >= minSamples && now.Sub(m.start) >= window
}

// thresholdJudge flags readings strictly above a threshold; the episode
// starts at the first exceeding reading.
func thresholdJudge(threshold float64) classify {
	return func(_ []model.Sample, newest model.Sample) (bool, time.Time, int) {
		if newest.Value > threshold {
			return true, newest.ObservedAt, 0
		}
		return false, time.Time{}, 0
	}
}

// dropJudge flags readings below an optional absolute ceiling or that have
// fallen by at least dropThreshold from the run baseline. The episode starts
// at the last baseline (normal) reading so its duration covers the whole
// fall.
func dropJudge(marker *runMarker, absoluteThreshold, dropThreshold float64) classify {
	return func(view []model.Sample, newest model.Sample) (bool, time.Time, int) {
		baseline := marker.baseline
		if !marker.hasBaseline {
			baseline = highestValue(view)
		}
		anomalous := (absoluteThreshold != 0 && newest.Value < absoluteThreshold) ||
			(dropThreshold > 0 && baseline-newest.Value >= dropThreshold)
		if !anomalous {
			return false, time.Time{}, 0
		}
		if !marker.hasBaseline {
			marker.baseline = baseline
			marker.hasBaseline = true
		}
		episodeStart := newest.ObservedAt
		baselineIndex := -1
		for i := len(view) - 1; i >= 0; i-- {
			if view[i].Value >= baseline {
				episodeStart = view[i].ObservedAt
				baselineIndex = i
				break
			}
		}
		// Trailing contiguous samples from the baseline through the newest
		// form the episode; everything after the baseline is anomalous here,
		// so head contributes exactly one (baseline) sample.
		headSamples := 0
		if baselineIndex >= 0 {
			headSamples = 1
		}
		return true, episodeStart, headSamples
	}
}

func highestValue(samples []model.Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	high := samples[0].Value
	for _, sample := range samples[1:] {
		if sample.Value > high {
			high = sample.Value
		}
	}
	return high
}
