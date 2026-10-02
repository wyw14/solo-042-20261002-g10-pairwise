package detection

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// probeWindow retains the samples a single probe produced within the trailing
// evaluation window. Samples older than the window are evicted lazily so the
// detector reasons about current behaviour rather than stale history.
type probeWindow struct {
	mu                sync.RWMutex
	probeID           string
	kind              model.ProbeKind
	calibrationEpoch  uint64
	bindingGeneration uint64
	samples           []model.Sample
}

func newProbeWindow(probeID string, kind model.ProbeKind) (*probeWindow, error) {
	if probeID == "" || !kind.Valid() {
		return nil, errors.New("invalid detection probe window")
	}
	return &probeWindow{probeID: probeID, kind: kind}, nil
}

// Add records the sample. A calibration or binding generation change means the
// probe was recalibrated or rebound, so readings before it are not comparable
// and the window is restarted.
func (w *probeWindow) Add(sample model.Sample) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calibrationEpoch != sample.CalibrationEpoch || w.bindingGeneration != sample.BindingGeneration {
		w.calibrationEpoch = sample.CalibrationEpoch
		w.bindingGeneration = sample.BindingGeneration
		w.samples = nil
	}
	w.samples = append(w.samples, sample)
}

// prune drops samples older than horizon. Callers hold no lock; pruning takes
// the write lock. Samples are expected to arrive roughly ordered by time.
func (w *probeWindow) prune(horizon time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.samples[:0]
	for _, sample := range w.samples {
		if !sample.ObservedAt.Before(horizon) {
			kept = append(kept, sample)
		}
	}
	w.samples = kept
}

func (w *probeWindow) snapshot() []model.Sample {
	w.mu.RLock()
	defer w.mu.RUnlock()
	result := append([]model.Sample(nil), w.samples...)
	sort.Slice(result, func(i, j int) bool { return result[i].ObservedAt.Before(result[j].ObservedAt) })
	return result
}

func (w *probeWindow) latest() (model.Sample, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if len(w.samples) == 0 {
		return model.Sample{}, false
	}
	return w.samples[len(w.samples)-1], true
}
