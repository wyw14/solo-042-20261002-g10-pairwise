package detection

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

// AlarmRaiser creates and dispatches an alarm carrying the evidence samples
// that justify it. alarm.Service satisfies this interface.
type AlarmRaiser interface {
	RaiseWithEvidence(ctx context.Context, vesselID string, kind model.AlarmKind, message string, evidence []model.EvidenceSample) (model.Alarm, error)
}

type verdict int

const (
	// verdictUnknown means available data is insufficient (probe offline or
	// not enough fresh samples). It neither raises nor clears an alarm.
	verdictUnknown verdict = iota
	// verdictClear means fresh, sufficient data refutes the condition.
	verdictClear
	// verdictAlarm means fresh, sufficient data confirms the condition.
	verdictAlarm
)

type conditionState struct {
	active      bool
	since       time.Time
	lastChanged time.Time
}

// Detector is an independent anomaly discrimination module. It keeps a
// configurable trailing window of level, pressure and temperature samples per
// vessel and recognises sustained over-pressure and suspected nitrogen leaks.
//
// A condition must persist beyond the configured duration and be backed by
// several fresh samples before an alarm is dispatched, which filters single
// point noise. Probes that are disconnected or reporting stale data are
// ignored rather than trusted. While a condition stays active it is never
// dispatched twice; once fresh data shows recovery the state re-arms and a
// later anomaly can trigger a new alarm.
type Detector struct {
	cfg    Config
	raiser AlarmRaiser
	now    func() time.Time

	mu      sync.Mutex
	windows map[string]*probeWindow // keyed by probe id
	vessel  map[string]string       // probe id -> current vessel id
	states  map[string]map[model.AlarmKind]*conditionState
}

func NewDetector(cfg Config, raiser AlarmRaiser, now func() time.Time) (*Detector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if raiser == nil {
		return nil, ErrNoRaiser
	}
	if now == nil {
		now = time.Now
	}
	return &Detector{
		cfg:     cfg,
		raiser:  raiser,
		now:     now,
		windows: make(map[string]*probeWindow),
		vessel:  make(map[string]string),
		states:  make(map[string]map[model.AlarmKind]*conditionState),
	}, nil
}

// Observe is an ingestor observer: it is called once per accepted sample.
func (d *Detector) Observe(sample model.Sample) {
	d.mu.Lock()
	window, ok := d.windows[sample.ProbeID]
	if !ok {
		window, _ = newProbeWindow(sample.ProbeID, sample.Kind)
		d.windows[sample.ProbeID] = window
	}
	window.Add(sample)
	d.vessel[sample.ProbeID] = sample.VesselID
	d.mu.Unlock()

	d.evaluateVessel(context.Background(), sample.VesselID)
}

// Sweep re-evaluates every known vessel. Call it from a periodic ticker so a
// recovery that only becomes visible through sample aging is noticed even when
// no new samples arrive.
func (d *Detector) Sweep(ctx context.Context) {
	d.mu.Lock()
	vesselIDs := make([]string, 0, len(d.states))
	seen := make(map[string]struct{})
	for vesselID := range d.states {
		vesselIDs = append(vesselIDs, vesselID)
		seen[vesselID] = struct{}{}
	}
	for _, vesselID := range d.vessel {
		if _, ok := seen[vesselID]; !ok {
			vesselIDs = append(vesselIDs, vesselID)
			seen[vesselID] = struct{}{}
		}
	}
	d.mu.Unlock()
	for _, vesselID := range vesselIDs {
		d.evaluateVessel(ctx, vesselID)
	}
}

type vesselReading struct {
	pressure []model.Sample
	level    []model.Sample
	temp     []model.Sample
}

type decision struct {
	kind     model.AlarmKind
	verdict  verdict
	since    time.Time
	message  string
	evidence []model.EvidenceSample
}

func (d *Detector) evaluateVessel(ctx context.Context, vesselID string) {
	at := d.now().UTC()
	reading := d.reading(vesselID, at.Add(-d.cfg.Window))

	decisions := []decision{
		d.judgeOverpressure(vesselID, reading, at),
		d.judgeLeak(vesselID, reading, at),
	}

	// Resolve what is active right now and mark new alarms before leaving the
	// lock so concurrent samples cannot dispatch the same condition twice.
	type raise struct{ decision decision }
	pending := make([]raise, 0, len(decisions))
	d.mu.Lock()
	states := d.states[vesselID]
	if states == nil {
		states = make(map[model.AlarmKind]*conditionState)
		d.states[vesselID] = states
	}
	for _, dec := range decisions {
		state := states[dec.kind]
		if state == nil {
			state = &conditionState{}
			states[dec.kind] = state
		}
		switch dec.verdict {
		case verdictAlarm:
			if !state.active {
				state.active = true
				state.since = dec.since
				state.lastChanged = at
				pending = append(pending, raise{dec})
			}
		case verdictClear:
			if state.active {
				state.active = false
				state.since = time.Time{}
				state.lastChanged = at
			}
		}
	}
	d.mu.Unlock()

	for _, item := range pending {
		d.dispatch(ctx, vesselID, item.decision)
	}
}

func (d *Detector) dispatch(ctx context.Context, vesselID string, dec decision) {
	alarm, err := d.raiser.RaiseWithEvidence(ctx, vesselID, dec.kind, dec.message, dec.evidence)
	if err != nil {
		if alarm.ID == uuid.Nil {
			// The alarm was never created; re-arm so a later sample retries.
			d.rearm(vesselID, dec.kind)
			slog.Error("anomaly alarm could not be raised", "vessel", vesselID, "kind", dec.kind, "error", err)
			return
		}
		// Created but partially delivered: the retry queue owns delivery now,
		// and the condition must still count as active to avoid duplicates.
		slog.Warn("anomaly alarm partially delivered", "vessel", vesselID, "kind", dec.kind, "error", err)
	}
}

func (d *Detector) rearm(vesselID string, kind model.AlarmKind) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if states := d.states[vesselID]; states != nil {
		if state := states[kind]; state != nil {
			state.active = false
			state.since = time.Time{}
		}
	}
}

func (d *Detector) reading(vesselID string, horizon time.Time) vesselReading {
	d.mu.Lock()
	probes := make([]*probeWindow, 0)
	for probeID, currentVessel := range d.vessel {
		if currentVessel == vesselID {
			probes = append(probes, d.windows[probeID])
		}
	}
	d.mu.Unlock()

	var reading vesselReading
	for _, window := range probes {
		window.prune(horizon)
		samples := window.snapshot()
		switch window.kind {
		case model.ProbePressure:
			reading.pressure = append(reading.pressure, samples...)
		case model.ProbeLevel:
			reading.level = append(reading.level, samples...)
		case model.ProbeTemperature:
			reading.temp = append(reading.temp, samples...)
		}
	}
	sort.Slice(reading.pressure, func(i, j int) bool { return reading.pressure[i].ObservedAt.Before(reading.pressure[j].ObservedAt) })
	sort.Slice(reading.level, func(i, j int) bool { return reading.level[i].ObservedAt.Before(reading.level[j].ObservedAt) })
	sort.Slice(reading.temp, func(i, j int) bool { return reading.temp[i].ObservedAt.Before(reading.temp[j].ObservedAt) })
	return reading
}

// freshAndSufficient reports whether a probe stream can vote: it must carry at
// least MinSamples and its newest reading must not be older than MaxSampleAge.
// A probe with no recent data is offline and excluded from every decision.
func (d *Detector) freshAndSufficient(samples []model.Sample, at time.Time) bool {
	if len(samples) < d.cfg.MinSamples {
		return false
	}
	return at.Sub(samples[len(samples)-1].ObservedAt) <= d.cfg.MaxSampleAge
}

func (d *Detector) judgeOverpressure(vesselID string, reading vesselReading, at time.Time) decision {
	dec := decision{kind: model.AlarmPressure}
	if !d.freshAndSufficient(reading.pressure, at) {
		dec.verdict = verdictUnknown
		return dec
	}
	// Suffix of consecutive samples above the limit: any in-window sample at
	// or below the limit breaks the streak (and therefore proves recovery).
	runStart := -1
	for i := len(reading.pressure) - 1; i >= 0; i-- {
		if reading.pressure[i].Value <= d.cfg.PressureLimitKPa {
			break
		}
		runStart = i
	}
	if runStart < 0 {
		dec.verdict = verdictClear
		return dec
	}
	run := reading.pressure[runStart:]
	if len(run) < d.cfg.MinSamples || run[len(run)-1].ObservedAt.Sub(run[0].ObservedAt) < d.cfg.MinDuration {
		dec.verdict = verdictClear
		return dec
	}
	dec.verdict = verdictAlarm
	dec.since = run[0].ObservedAt
	dec.message = messageOverpressure(run, d.cfg.PressureLimitKPa)
	dec.evidence = d.evidence(vesselID, reading, []model.ProbeKind{model.ProbePressure}, at)
	return dec
}

func (d *Detector) judgeLeak(vesselID string, reading vesselReading, at time.Time) decision {
	dec := decision{kind: model.AlarmLeak}
	levelOK := d.freshAndSufficient(reading.level, at)
	pressureOK := d.freshAndSufficient(reading.pressure, at)
	if !levelOK || !pressureOK {
		// Cannot confirm a leak without both signals, and their absence must
		// not clear an alarm that was already raised.
		dec.verdict = verdictUnknown
		return dec
	}

	firstLevel := reading.level[0]
	lastLevel := reading.level[len(reading.level)-1]
	levelDrop := firstLevel.Value - lastLevel.Value
	levelFalling := levelDrop >= d.cfg.LeakLevelDropMM &&
		lastLevel.ObservedAt.Sub(firstLevel.ObservedAt) >= d.cfg.MinDuration

	elevated := 0
	for _, sample := range reading.pressure {
		if sample.Value >= d.cfg.LeakPressureKPa {
			elevated++
		}
	}
	pressureHigh := reading.pressure[len(reading.pressure)-1].Value >= d.cfg.LeakPressureKPa &&
		elevated*2 >= len(reading.pressure)

	if !levelFalling || !pressureHigh {
		dec.verdict = verdictClear
		return dec
	}
	dec.verdict = verdictAlarm
	dec.since = firstLevel.ObservedAt
	dec.message = messageLeak(levelDrop, reading.pressure, d.cfg.LeakPressureKPa)
	// Temperature is corroborating evidence only; a missing temperature probe
	// never blocks the decision.
	dec.evidence = d.evidence(vesselID, reading, []model.ProbeKind{model.ProbeLevel, model.ProbePressure, model.ProbeTemperature}, at)
	return dec
}

func (d *Detector) evidence(vesselID string, reading vesselReading, kinds []model.ProbeKind, at time.Time) []model.EvidenceSample {
	want := make(map[model.ProbeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		want[kind] = struct{}{}
	}
	picked := make([]model.Sample, 0)
	appendSamples := func(samples []model.Sample) {
		for _, sample := range samples {
			if _, ok := want[sample.Kind]; ok && at.Sub(sample.ObservedAt) <= d.cfg.MaxSampleAge {
				picked = append(picked, sample)
			}
		}
	}
	appendSamples(reading.level)
	appendSamples(reading.pressure)
	appendSamples(reading.temp)
	sort.Slice(picked, func(i, j int) bool {
		if picked[i].ObservedAt.Equal(picked[j].ObservedAt) {
			return picked[i].ProbeID < picked[j].ProbeID
		}
		return picked[i].ObservedAt.Before(picked[j].ObservedAt)
	})
	if len(picked) > d.cfg.MaxEvidenceSamples {
		cap := d.cfg.MaxEvidenceSamples
		head := cap / 2
		tail := cap - head
		kept := make([]model.Sample, 0, cap)
		kept = append(kept, picked[:head]...)
		kept = append(kept, picked[len(picked)-tail:]...)
		picked = kept
	}
	evidence := make([]model.EvidenceSample, 0, len(picked))
	for _, sample := range picked {
		evidence = append(evidence, model.NewEvidenceSample(sample))
	}
	return evidence
}

// Active reports whether a condition is currently flagged for a vessel. It is
// primarily intended for tests and diagnostics.
func (d *Detector) Active(vesselID string, kind model.AlarmKind) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if states := d.states[vesselID]; states != nil {
		if state := states[kind]; state != nil {
			return state.active
		}
	}
	return false
}
