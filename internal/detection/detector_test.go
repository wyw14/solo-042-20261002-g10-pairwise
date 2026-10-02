package detection

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

type raisedAlarm struct {
	vesselID string
	kind     model.AlarmKind
	message  string
	evidence []model.EvidenceSample
}

type mockRaiser struct {
	mu    sync.Mutex
	calls []raisedAlarm
	fail  bool
}

func (m *mockRaiser) RaiseWithEvidence(_ context.Context, vesselID string, kind model.AlarmKind, message string, evidence []model.EvidenceSample) (model.Alarm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, raisedAlarm{vesselID: vesselID, kind: kind, message: message, evidence: evidence})
	if m.fail {
		return model.Alarm{}, errors.New("raiser unavailable")
	}
	return model.Alarm{ID: uuid.New(), VesselID: vesselID, Kind: kind, Message: message, Evidence: evidence}, nil
}

func (m *mockRaiser) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockRaiser) kinds() []model.AlarmKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	kinds := make([]model.AlarmKind, 0, len(m.calls))
	for _, call := range m.calls {
		kinds = append(kinds, call.kind)
	}
	return kinds
}

func testConfig() Config {
	return Config{
		Window:             30 * time.Second,
		MinDuration:        5 * time.Second,
		MinSamples:         3,
		MaxSampleAge:       time.Minute,
		PressureLimitKPa:   600,
		LeakLevelDropMM:    20,
		LeakPressureKPa:    300,
		MaxEvidenceSamples: 10,
	}
}

func newTestDetector(t *testing.T, cfg Config) (*Detector, *mockRaiser, *time.Time) {
	t.Helper()
	clock := time.Unix(1000, 0)
	raiser := &mockRaiser{}
	detector, err := NewDetector(cfg, raiser, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	return detector, raiser, &clock
}

func pressureSample(vesselID string, value float64, at time.Time) model.Sample {
	return model.Sample{ProbeID: "P-P", VesselID: vesselID, Kind: model.ProbePressure, Value: value, CalibrationEpoch: 1, BindingGeneration: 1, ObservedAt: at}
}

func levelSample(probeID, vesselID string, value float64, at time.Time) model.Sample {
	return model.Sample{ProbeID: probeID, VesselID: vesselID, Kind: model.ProbeLevel, Value: value, CalibrationEpoch: 1, BindingGeneration: 1, ObservedAt: at}
}

func tempSample(vesselID string, value float64, at time.Time) model.Sample {
	return model.Sample{ProbeID: "P-T", VesselID: vesselID, Kind: model.ProbeTemperature, Value: value, CalibrationEpoch: 1, BindingGeneration: 1, ObservedAt: at}
}

func TestSinglePointPressureSpikeIsFiltered(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i, value := range []float64{500, 500, 500, 700, 500, 500} {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", value, *clock))
	}
	if raiser.count() != 0 {
		t.Fatalf("single-point spike raised %d alarms", raiser.count())
	}
	if detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("detector stayed active after a one-sample spike")
	}
}

func TestSustainedOverpressureRaisesOnceAndCarriesEvidence(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 500, *clock))
	}
	for i := 3; i < 9; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 650, *clock))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one over-pressure alarm, got %d", raiser.count())
	}
	call := raiser.calls[0]
	if call.kind != model.AlarmPressure || call.vesselID != "V-01" {
		t.Fatalf("unexpected alarm %+v", call)
	}
	if len(call.evidence) < 3 {
		t.Fatalf("alarm carried %d evidence samples", len(call.evidence))
	}
	for _, sample := range call.evidence {
		if sample.Kind != model.ProbePressure {
			t.Fatalf("over-pressure evidence included %s sample", sample.Kind)
		}
	}
	if !detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("detector not active after sustained over-pressure")
	}
	// Continuing high readings must not dispatch the same anomaly again.
	for i := 9; i < 12; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 650, *clock))
	}
	if raiser.count() != 1 {
		t.Fatalf("persistent anomaly re-dispatched %d times", raiser.count())
	}
}

func TestRecoveryRearmsOverpressure(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	send := func(second int, value float64) {
		*clock = base.Add(time.Duration(second) * time.Second)
		detector.Observe(pressureSample("V-01", value, *clock))
	}
	for i := 0; i < 3; i++ {
		send(i, 500)
	}
	for i := 3; i < 9; i++ {
		send(i, 650)
	}
	if raiser.count() != 1 {
		t.Fatalf("expected initial alarm, got %d", raiser.count())
	}
	for i := 9; i < 12; i++ {
		send(i, 500)
	}
	if detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("detector stayed active after recovery")
	}
	for i := 12; i < 18; i++ {
		send(i, 650)
	}
	if raiser.count() != 2 {
		t.Fatalf("re-occurrence after recovery raised %d alarms, want 2", raiser.count())
	}
}

func TestOfflinePressureProbeNeitherTriggersNorClears(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 500, *clock))
	}
	for i := 3; i < 9; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 650, *clock))
	}
	if raiser.count() != 1 {
		t.Fatalf("expected one alarm, got %d", raiser.count())
	}
	// Probe falls silent. Old samples leave the window; absence of data must
	// not be treated as recovery.
	*clock = base.Add(5 * time.Minute)
	detector.Sweep(context.Background())
	if !detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("missing probe data cleared an active alarm")
	}
	if raiser.count() != 1 {
		t.Fatal("sweep with stale data dispatched an alarm")
	}
	// Fresh healthy readings prove recovery.
	for i := 0; i < 3; i++ {
		*clock = clock.Add(time.Second)
		detector.Observe(pressureSample("V-01", 500, *clock))
	}
	if detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("fresh healthy readings did not clear the alarm")
	}
}

func TestShortOverpressureBelowDurationDoesNotAlarm(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	values := []float64{500, 500, 500, 650, 650, 650, 500, 500}
	for i, value := range values {
		*clock = base.Add(time.Duration(i*200) * time.Millisecond)
		detector.Observe(pressureSample("V-01", value, *clock))
	}
	if raiser.count() != 0 {
		t.Fatalf("brief high readings raised an alarm: %d", raiser.count())
	}
}

func TestSuspectedLeakCombinesFallingLevelAndElevatedPressure(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	// Level falling on its own with no pressure data is unknown, never a leak.
	levels := []float64{100, 95, 90, 85, 80, 75}
	for i, value := range levels {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(levelSample("P-L", "V-01", value, *clock))
	}
	if raiser.count() != 0 {
		t.Fatalf("falling level alone raised %d alarms", raiser.count())
	}
	// Elevated pressure corroborates the leak.
	for i := 0; i < 6; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 350, *clock))
		detector.Observe(tempSample("V-01", -180, *clock))
	}
	if raiser.count() != 1 || raiser.calls[0].kind != model.AlarmLeak {
		t.Fatalf("expected one leak alarm, got %v", raiser.kinds())
	}
	kinds := map[model.ProbeKind]bool{}
	for _, sample := range raiser.calls[0].evidence {
		kinds[sample.Kind] = true
	}
	if !kinds[model.ProbeLevel] || !kinds[model.ProbePressure] {
		t.Fatalf("leak evidence missing primary signals: %+v", kinds)
	}
	if !kinds[model.ProbeTemperature] {
		t.Fatal("leak evidence did not include available temperature samples")
	}
}

func TestLeakIgnoredWhenPressureProbeGoesOffline(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 350, at))
	}
	// Much later the level probe reports a fall but pressure data is stale.
	for i, value := range []float64{100, 95, 90, 85, 80, 75} {
		*clock = base.Add(5 * time.Minute).Add(time.Duration(i) * time.Second)
		detector.Observe(levelSample("P-L", "V-01", value, *clock))
	}
	detector.Sweep(context.Background())
	if raiser.count() != 0 {
		t.Fatalf("leak alarm raised with offline pressure probe: %d", raiser.count())
	}
}

func TestStableLevelAndPressureDoNotIndicateLeak(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		*clock = at
		detector.Observe(levelSample("P-L", "V-01", 100, at))
		detector.Observe(pressureSample("V-01", 350, at))
	}
	if raiser.count() != 0 {
		t.Fatalf("stable level raised a leak alarm: %d", raiser.count())
	}
}

func TestCalibrationEpochChangeRestartsEvidenceWindow(t *testing.T) {
	detector, raiser, clock := newTestDetector(t, testConfig())
	base := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		*clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 500, *clock))
	}
	recalibrated := pressureSample("V-01", 650, base.Add(3*time.Second))
	recalibrated.CalibrationEpoch = 2
	recalibratedAgain := pressureSample("V-01", 650, base.Add(4*time.Second))
	recalibratedAgain.CalibrationEpoch = 2
	detector.Observe(recalibrated)
	detector.Observe(recalibratedAgain)
	if raiser.count() != 0 {
		t.Fatal("readings immediately after recalibration raised an alarm")
	}
}

func TestRaiserFailureRearmsForRetry(t *testing.T) {
	cfg := testConfig()
	clock := time.Unix(1000, 0)
	raiser := &mockRaiser{fail: true}
	detector, err := NewDetector(cfg, raiser, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	base := clock
	for i := 0; i < 3; i++ {
		clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 500, clock))
	}
	for i := 3; i < 9; i++ {
		clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 650, clock))
	}
	if raiser.count() == 0 {
		t.Fatal("raiser was never attempted")
	}
	if detector.Active("V-01", model.AlarmPressure) {
		t.Fatal("failed dispatch stayed active and would suppress retries")
	}
	raiser.fail = false
	for i := 9; i < 12; i++ {
		clock = base.Add(time.Duration(i) * time.Second)
		detector.Observe(pressureSample("V-01", 650, clock))
	}
	if raiser.count() < 2 {
		t.Fatalf("condition did not retry after a failed dispatch: %d", raiser.count())
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := testConfig()
	cfg.MinDuration = cfg.Window + time.Second
	if _, err := NewDetector(cfg, &mockRaiser{}, time.Now); err == nil {
		t.Fatal("detector accepted a minimum duration longer than its window")
	}
	cfg = testConfig()
	cfg.MinSamples = 1
	if _, err := NewDetector(cfg, &mockRaiser{}, time.Now); err == nil {
		t.Fatal("detector accepted a single-sample threshold")
	}
}
