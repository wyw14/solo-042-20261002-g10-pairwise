package detect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

type fakeProbes struct {
	mu     sync.Mutex
	probes []model.Probe
}

func (f *fakeProbes) List() []model.Probe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Probe(nil), f.probes...)
}

func (f *fakeProbes) set(probes ...model.Probe) {
	f.mu.Lock()
	f.probes = probes
	f.mu.Unlock()
}

type raisedCall struct {
	vesselID string
	kind     model.AlarmKind
	message  string
	evidence []model.EvidenceSample
}

type fakeRaiser struct {
	mu    sync.Mutex
	calls []raisedCall
	err   error
}

func (f *fakeRaiser) RaiseWithEvidence(_ context.Context, vesselID string, kind model.AlarmKind, message string, evidence []model.EvidenceSample) (model.Alarm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, raisedCall{vesselID: vesselID, kind: kind, message: message, evidence: evidence})
	return model.Alarm{VesselID: vesselID, Kind: kind, Message: message, Evidence: evidence}, f.err
}

func (f *fakeRaiser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRaiser) kinds() []model.AlarmKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	kinds := make([]model.AlarmKind, 0, len(f.calls))
	for _, call := range f.calls {
		kinds = append(kinds, call.kind)
	}
	return kinds
}

func testConfig() Config {
	return Config{
		DetectWindow:         10 * time.Second,
		MinSamples:           3,
		StaleAfter:           time.Minute,
		MaxWindowSamples:     16,
		OverPressureKPa:      500,
		LeakPressureKPa:      80,
		LeakPressureDropKPa:  15,
		LeakLevelMM:          0,
		LeakLevelDropMM:      20,
		LeakTemperatureC:     0,
		LeakTemperatureDropC: 10,
	}
}

type clock struct{ t time.Time }

func newDetector(t *testing.T, probes *fakeProbes) (*Detector, *fakeRaiser, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)}
	raiser := &fakeRaiser{}
	detector, err := NewDetector(testConfig(), probes, raiser, func() time.Time { return clk.t })
	if err != nil {
		t.Fatal(err)
	}
	return detector, raiser, clk
}

func sampleAt(probeID, vesselID string, kind model.ProbeKind, value float64, at time.Time) model.Sample {
	return model.Sample{
		ProbeID: probeID, VesselID: vesselID, Kind: kind, Value: value,
		CalibrationEpoch: 1, BindingGeneration: 1, ObservedAt: at,
	}
}

func TestSinglePointOverPressureDoesNotRaise(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 620, base))
	clk.t = base.Add(5 * time.Second)
	detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 400, clk.t))
	clk.t = base.Add(10 * time.Second)
	detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 400, clk.t))
	detector.Sweep(context.Background())

	if raiser.count() != 0 {
		t.Fatalf("single-point pressure spike raised %d alarms", raiser.count())
	}
}

func TestSustainedOverPressureRaisesOnceWithEvidence(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	for _, offset := range []time.Duration{0, 5 * time.Second, 10 * time.Second} {
		clk.t = base.Add(offset)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one alarm, got %d", raiser.count())
	}
	call := raiser.calls[0]
	if call.kind != model.AlarmPressure || call.vesselID != "V1" {
		t.Fatalf("unexpected alarm: %+v", call)
	}
	if len(call.evidence) < 3 {
		t.Fatalf("expected evidence samples, got %d", len(call.evidence))
	}
	for _, evidence := range call.evidence {
		if evidence.Kind != model.ProbePressure {
			t.Fatalf("evidence contains non-pressure sample: %+v", evidence)
		}
	}

	// Continuing anomaly must not be dispatched again.
	for _, offset := range []time.Duration{15, 20, 25} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 550, clk.t))
	}
	detector.Sweep(context.Background())
	if raiser.count() != 1 {
		t.Fatalf("continuing anomaly re-dispatched %d alarms", raiser.count())
	}
}

func TestOverPressureRearmsAfterRecovery(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	for _, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one alarm, got %d", raiser.count())
	}

	// MinSamples consecutive in-range evaluations re-arm the condition.
	for _, offset := range []time.Duration{15, 20, 25} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 400, clk.t))
	}

	// A single normal blip before re-arm would not have reset it; now a new
	// sustained over-pressure episode raises a second alarm.
	for _, offset := range []time.Duration{40, 45, 50} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 600, clk.t))
	}
	if raiser.count() != 2 {
		t.Fatalf("expected re-triggered alarm after recovery, got %d", raiser.count())
	}
}

func TestOneNormalBlipDoesNotRearm(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	for _, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	// One in-range sample, then high again: not enough consecutive recoveries.
	clk.t = base.Add(15 * time.Second)
	detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 400, clk.t))
	for _, offset := range []time.Duration{20, 25, 30} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("single normal blip must not re-arm, got %d alarms", raiser.count())
	}
}

func TestDisconnectedAndStaleProbeIsFiltered(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: false})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	for _, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 700, clk.t))
	}
	if raiser.count() != 0 {
		t.Fatalf("disconnected probe samples must be filtered, got %d alarms", raiser.count())
	}

	// Go connected and sustain high: should now raise.
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true, CalibrationEpoch: 1})
	for _, offset := range []time.Duration{20, 25, 30} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 700, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected alarm once probe reconnected, got %d", raiser.count())
	}

	// Probe goes silent past StaleAfter: the raised condition is held rather
	// than cleared or re-dispatched by the sweep.
	clk.t = base.Add(30 * time.Minute)
	detector.Sweep(context.Background())
	if raiser.count() != 1 {
		t.Fatalf("stale sweep must hold the decision, got %d alarms", raiser.count())
	}
}

func TestLeakRequiresCorroboration(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(
		model.Probe{ID: "PP", Kind: model.ProbePressure, VesselID: "V1", Connected: true},
		model.Probe{ID: "PL", Kind: model.ProbeLevel, VesselID: "V1", Connected: true},
	)
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	// Pressure falls but level is steady: no corroboration, no alarm.
	pressure := []float64{100, 85, 70}
	level := []float64{800, 800, 800}
	for i, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, pressure[i], clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, level[i], clk.t))
	}
	if raiser.count() != 0 {
		t.Fatalf("pressure-only drop raised a leak alarm without corroboration: %d", raiser.count())
	}
}

func TestSustainedLeakWithLevelDropRaises(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(
		model.Probe{ID: "PP", Kind: model.ProbePressure, VesselID: "V1", Connected: true},
		model.Probe{ID: "PL", Kind: model.ProbeLevel, VesselID: "V1", Connected: true},
	)
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	pressure := []float64{100, 82, 70}
	level := []float64{800, 770, 745}
	for i, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, pressure[i], clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, level[i], clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one leak alarm, got %d", raiser.count())
	}
	call := raiser.calls[0]
	if call.kind != model.AlarmLeak {
		t.Fatalf("expected leak alarm, got %s", call.kind)
	}
	hasPressure, hasLevel := false, false
	for _, evidence := range call.evidence {
		switch evidence.Kind {
		case model.ProbePressure:
			hasPressure = true
		case model.ProbeLevel:
			hasLevel = true
		}
	}
	if !hasPressure || !hasLevel {
		t.Fatalf("leak evidence must include pressure and level samples: %+v", call.evidence)
	}

	// Continuing leak dispatches only once, even as the window slides.
	for _, offset := range []time.Duration{15, 20, 25} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, 69, clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, 740, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("continuing leak re-dispatched %d alarms", raiser.count())
	}
}

func TestCalibrationEpochChangeResetsWindow(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	// High samples that do not yet span the full window.
	for _, offset := range []time.Duration{0, 4, 8} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 600, clk.t))
	}
	if raiser.count() != 0 {
		t.Fatalf("sub-window high readings must not raise, got %d", raiser.count())
	}

	// Recalibration bumps the epoch; pre-recalibration history is discarded.
	high := func(at time.Time, epoch uint64) model.Sample {
		s := sampleAt("P1", "V1", model.ProbePressure, 600, at)
		s.CalibrationEpoch = epoch
		return s
	}
	clk.t = base.Add(10 * time.Second)
	detector.Observe(high(clk.t, 2))
	clk.t = base.Add(11 * time.Second)
	detector.Observe(high(clk.t, 2))
	detector.Sweep(context.Background())
	if raiser.count() != 0 {
		t.Fatalf("post-recalibration history must start fresh, got %d", raiser.count())
	}

	clk.t = base.Add(20 * time.Second)
	detector.Observe(high(clk.t, 2))
	if raiser.count() != 1 {
		t.Fatalf("expected alarm after a full window under the new epoch, got %d", raiser.count())
	}
}

func TestLeakRearmsAfterRecovery(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(
		model.Probe{ID: "PP", Kind: model.ProbePressure, VesselID: "V1", Connected: true},
		model.Probe{ID: "PL", Kind: model.ProbeLevel, VesselID: "V1", Connected: true},
	)
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	pressure := []float64{100, 82, 70}
	level := []float64{800, 770, 745}
	for i, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, pressure[i], clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, level[i], clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one leak alarm, got %d", raiser.count())
	}

	// Recover: stable normal readings until the prior anomaly ages out.
	for _, offset := range []time.Duration{15, 20, 25, 40} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, 110, clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, 900, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("recovery must not dispatch, got %d", raiser.count())
	}

	// A second leak episode after recovery must re-trigger.
	second := clk.t
	p2 := []float64{110, 90, 72}
	l2 := []float64{900, 860, 820}
	for i, offset := range []time.Duration{0, 5, 10} {
		clk.t = second.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("PP", "V1", model.ProbePressure, p2[i], clk.t))
		detector.Observe(sampleAt("PL", "V1", model.ProbeLevel, l2[i], clk.t))
	}
	if raiser.count() != 2 {
		t.Fatalf("expected leak to re-trigger after recovery, got %d", raiser.count())
	}
}

func TestSustainedOverPressureWithIrregularSpacing(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true})
	detector, raiser, clk := newDetector(t, probes)

	// Production-like irregular spacing: the anomalous run began at the first
	// exceeding sample, so firing depends on elapsed time since then, not on
	// whether samples happen to straddle the sliding window boundary.
	base := clk.t
	highAt := []time.Duration{0, 3 * time.Second, 11 * time.Second}
	for _, offset := range highAt {
		clk.t = base.Add(offset)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected alarm after irregular sustained run, got %d", raiser.count())
	}

	// Sub-window run (last two readings span only 4s) must not raise.
	clk.t = base.Add(20 * time.Minute)
	raiser2 := &fakeRaiser{}
	detector2, err := NewDetector(testConfig(), probes, raiser2, func() time.Time { return clk.t })
	if err != nil {
		t.Fatal(err)
	}
	reset := clk.t
	for _, offset := range []time.Duration{0, 2 * time.Second, 4 * time.Second} {
		clk.t = reset.Add(offset)
		detector2.Observe(sampleAt("P1", "V1", model.ProbePressure, 540, clk.t))
	}
	if raiser2.count() != 0 {
		t.Fatalf("sub-window run must not raise, got %d", raiser2.count())
	}
}

func TestVesselsAreEvaluatedIndependently(t *testing.T) {
	probes := &fakeProbes{}
	probes.set(
		model.Probe{ID: "P1", Kind: model.ProbePressure, VesselID: "V1", Connected: true},
		model.Probe{ID: "P2", Kind: model.ProbePressure, VesselID: "V2", Connected: true},
	)
	detector, raiser, clk := newDetector(t, probes)

	base := clk.t
	for _, offset := range []time.Duration{0, 5, 10} {
		clk.t = base.Add(time.Duration(offset) * time.Second)
		detector.Observe(sampleAt("P1", "V1", model.ProbePressure, 600, clk.t))
		detector.Observe(sampleAt("P2", "V2", model.ProbePressure, 300, clk.t))
	}
	if raiser.count() != 1 || raiser.calls[0].vesselID != "V1" {
		t.Fatalf("expected exactly one alarm for V1, got %+v", raiser.kinds())
	}
}
