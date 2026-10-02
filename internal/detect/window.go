package detect

import (
	"sort"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// probeWindow retains the recent samples of one probe. A calibration or
// rebinding generation change invalidates the history because the values
// are no longer comparable across the change.
type probeWindow struct {
	calibrationEpoch  uint64
	bindingGeneration uint64
	initialized       bool
	samples           []model.Sample
}

func (w *probeWindow) add(sample model.Sample, maxSamples int) {
	if !w.initialized {
		w.calibrationEpoch = sample.CalibrationEpoch
		w.bindingGeneration = sample.BindingGeneration
		w.initialized = true
	} else if sample.CalibrationEpoch != w.calibrationEpoch || sample.BindingGeneration != w.bindingGeneration {
		w.calibrationEpoch = sample.CalibrationEpoch
		w.bindingGeneration = sample.BindingGeneration
		w.samples = nil
	}
	w.samples = append(w.samples, sample)
	if len(w.samples) > maxSamples {
		w.samples = w.samples[len(w.samples)-maxSamples:]
	}
}

func (w *probeWindow) prune(cutoff time.Time) {
	if len(w.samples) == 0 {
		return
	}
	kept := w.samples[:0]
	for _, sample := range w.samples {
		if !sample.ObservedAt.Before(cutoff) {
			kept = append(kept, sample)
		}
	}
	w.samples = kept
}

// kindView merges the in-window samples of every usable probe of one kind
// on a vessel.
type kindView struct {
	kind   model.ProbeKind
	sorted []model.Sample
}

func (v kindView) latest() float64 { return v.sorted[len(v.sorted)-1].Value }
func (v kindView) oldest() float64 { return v.sorted[0].Value }

func (v kindView) allBelow(threshold float64) bool {
	for _, sample := range v.sorted {
		if sample.Value >= threshold {
			return false
		}
	}
	return true
}

func (v kindView) allAbove(threshold float64) bool {
	for _, sample := range v.sorted {
		if sample.Value <= threshold {
			return false
		}
	}
	return true
}

// tail returns the n most recent samples in chronological order.
func (v kindView) tail(n int) []model.Sample {
	if len(v.sorted) <= n {
		return v.sorted
	}
	return v.sorted[len(v.sorted)-n:]
}

// mergeKindViews merges windows of one sensor kind into a chronological view
// and reports whether the sensor is usable: enough fresh samples inside the
// window. Callers filter in only connected probes, so a disconnected probe
// never contributes to a decision.
func mergeKindViews(windows []*probeWindow, kind model.ProbeKind, minSamples int, now time.Time, staleAfter time.Duration) (kindView, bool) {
	view := kindView{kind: kind}
	for _, window := range windows {
		if len(window.samples) == 0 || window.samples[0].Kind != kind {
			continue
		}
		view.sorted = append(view.sorted, window.samples...)
	}
	if len(view.sorted) < minSamples {
		return view, false
	}
	sort.Slice(view.sorted, func(i, j int) bool {
		return view.sorted[i].ObservedAt.Before(view.sorted[j].ObservedAt)
	})
	newest := view.sorted[len(view.sorted)-1].ObservedAt
	if now.Sub(newest) > staleAfter {
		return view, false
	}
	return view, true
}
