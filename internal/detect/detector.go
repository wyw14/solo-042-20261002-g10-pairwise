// Package detect is an independent anomaly discrimination module. It watches
// accepted telemetry samples and recognises sustained over-pressure and
// suspected nitrogen leaks over a configurable time window, combining changes
// from multiple sensor kinds. Single-point noise and samples from stale or
// disconnected probes are filtered out. Every dispatched alarm carries the
// evidence samples that justify it; a continuing anomaly is dispatched only
// once and re-arms after the vessel returns to normal.
package detect

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// ProbeLister exposes the probes currently known to the system. The probe
// registry satisfies it.
type ProbeLister interface {
	List() []model.Probe
}

// AlarmRaiser dispatches alarms raised by the detector. The alarm service
// satisfies it.
type AlarmRaiser interface {
	RaiseWithEvidence(ctx context.Context, vesselID string, kind model.AlarmKind, message string, evidence []model.EvidenceSample) (model.Alarm, error)
}

// pendingAlarm is a decision produced under the detector lock and dispatched
// after the lock is released.
type pendingAlarm struct {
	vesselID string
	kind     model.AlarmKind
	message  string
	evidence []model.EvidenceSample
}

// probeTracks holds the sliding sample window and the two anomaly markers of
// one probe (threshold-style for over-pressure, drop-style for leaks).
type probeTracks struct {
	window   *probeWindow
	pressure runMarker
	leak     runMarker
	kind     model.ProbeKind
}

// conditionState records whether a discriminator has dispatched the current
// anomaly. Continuity itself is tracked per probe by runMarker.
type conditionState struct {
	raised bool
}

type vesselState struct {
	tracks       map[string]*probeTracks
	overPressure conditionState
	leak         conditionState
}

// Detector keeps per-vessel sliding windows, anomaly markers and the raised
// state of each discriminator. It is safe for concurrent use.
type Detector struct {
	cfg     Config
	probes  ProbeLister
	alarms  AlarmRaiser
	now     func() time.Time
	mu      sync.Mutex
	vessels map[string]*vesselState
}

func NewDetector(cfg Config, probes ProbeLister, alarms AlarmRaiser, now func() time.Time) (*Detector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if probes == nil || alarms == nil {
		return nil, fmt.Errorf("detect dependencies are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Detector{cfg: cfg, probes: probes, alarms: alarms, now: now, vessels: make(map[string]*vesselState)}, nil
}

// Observe is an ingestor observer. It is invoked for every accepted sample.
func (d *Detector) Observe(sample model.Sample) {
	d.mu.Lock()
	pending := d.ingestLocked(sample)
	d.mu.Unlock()
	d.dispatch(context.Background(), pending)
}

// Sweep re-evaluates every known vessel at the current wall-clock time. It
// lets recovery age out when probes go quiet even if no new samples arrive,
// without allowing a lost probe to clear an alarm by itself. The runtime
// calls it periodically.
func (d *Detector) Sweep(ctx context.Context) {
	d.mu.Lock()
	var pending []pendingAlarm
	for vesselID := range d.vessels {
		pending = append(pending, d.evaluateLocked(vesselID)...)
	}
	d.mu.Unlock()
	d.dispatch(ctx, pending)
}

func (d *Detector) ingestLocked(sample model.Sample) []pendingAlarm {
	// The ingestor already rejects disconnected probes; enforce it here too
	// so the discriminator is self-contained and never trusts such samples.
	connected := false
	for _, probeState := range d.probes.List() {
		if probeState.ID == sample.ProbeID && probeState.Connected {
			connected = true
			break
		}
	}
	if !connected {
		return nil
	}
	state := d.vessels[sample.VesselID]
	if state == nil {
		state = &vesselState{tracks: make(map[string]*probeTracks)}
		d.vessels[sample.VesselID] = state
	}
	tracks := state.tracks[sample.ProbeID]
	if tracks == nil {
		tracks = &probeTracks{window: &probeWindow{}, kind: sample.Kind}
		state.tracks[sample.ProbeID] = tracks
	}
	tracks.window.add(sample, d.cfg.MaxWindowSamples)
	return d.evaluateLocked(sample.VesselID)
}

func (d *Detector) evaluateLocked(vesselID string) []pendingAlarm {
	state := d.vessels[vesselID]
	now := d.now()
	cutoff := now.Add(-d.cfg.DetectWindow)

	connected := make(map[string]bool)
	for _, probeState := range d.probes.List() {
		if probeState.VesselID == vesselID {
			connected[probeState.ID] = probeState.Connected
		}
	}

	var pending []pendingAlarm
	if alarm, fire := d.discriminateOverPressure(vesselID, state, connected, now, cutoff); fire {
		pending = append(pending, alarm)
	}
	if alarm, fire := d.discriminateLeak(vesselID, state, connected, now, cutoff); fire {
		pending = append(pending, alarm)
	}
	return pending
}

// tracksByKind returns the connected probe tracks of one sensor kind after
// pruning their windows to the detection window. fresh reports whether at
// least one such probe delivered a sample within StaleAfter, so callers can
// distinguish "sensors report normal" from "sensors went silent" and hold
// their decision in the latter case.
func (d *Detector) tracksByKind(state *vesselState, connected map[string]bool, kind model.ProbeKind, cutoff, now time.Time) (tracks []*probeTracks, fresh bool) {
	result := make([]*probeTracks, 0)
	for probeID, t := range state.tracks {
		if t.kind != kind || !connected[probeID] {
			continue
		}
		t.window.prune(cutoff)
		if len(t.window.samples) == 0 {
			continue
		}
		newest := t.window.samples[len(t.window.samples)-1].ObservedAt
		if now.Sub(newest) <= d.cfg.StaleAfter {
			fresh = true
		}
		result = append(result, t)
	}
	return result, fresh
}

func (d *Detector) discriminateOverPressure(vesselID string, state *vesselState, connected map[string]bool, now time.Time, cutoff time.Time) (pendingAlarm, bool) {
	tracks, fresh := d.tracksByKind(state, connected, model.ProbePressure, cutoff, now)
	matured := (*probeTracks)(nil)
	latestAnomaly := time.Time{}
	for _, t := range tracks {
		t.pressure.update(t.window.samples, true, now, d.cfg.StaleAfter,
			thresholdJudge(d.cfg.OverPressureKPa))
		if t.pressure.matured(now, d.cfg.DetectWindow, d.cfg.MinSamples) && matured == nil {
			matured = t
		}
		if t.pressure.lastAnomalyAt.After(latestAnomaly) {
			latestAnomaly = t.pressure.lastAnomalyAt
		}
	}

	if matured == nil {
		// Hold while probes are silent: a lost probe cannot clear an alarm.
		if state.overPressure.raised && (!fresh || !d.recovered(latestAnomaly, now)) {
			return pendingAlarm{}, false
		}
		state.overPressure.raised = false
		return pendingAlarm{}, false
	}
	if state.overPressure.raised {
		return pendingAlarm{}, false
	}
	state.overPressure.raised = true
	evidence := model.EvidenceFromSamples(matured.window.samples)
	latest := matured.window.samples[len(matured.window.samples)-1].Value
	return pendingAlarm{
		vesselID: vesselID,
		kind:     model.AlarmPressure,
		message: fmt.Sprintf("sustained pressure above %.1f kPa threshold on vessel %s (latest %.1f kPa)",
			d.cfg.OverPressureKPa, vesselID, latest),
		evidence: evidence,
	}, true
}

func (d *Detector) discriminateLeak(vesselID string, state *vesselState, connected map[string]bool, now time.Time, cutoff time.Time) (pendingAlarm, bool) {
	pressureTracks, pressureFresh := d.tracksByKind(state, connected, model.ProbePressure, cutoff, now)
	levelTracks, _ := d.tracksByKind(state, connected, model.ProbeLevel, cutoff, now)
	temperatureTracks, _ := d.tracksByKind(state, connected, model.ProbeTemperature, cutoff, now)

	update := func(tracks []*probeTracks, absolute, drop float64) {
		for _, t := range tracks {
			t.leak.update(t.window.samples, true, now, d.cfg.StaleAfter,
				dropJudge(&t.leak, absolute, drop))
		}
	}
	update(pressureTracks, d.cfg.LeakPressureKPa, d.cfg.LeakPressureDropKPa)
	if d.cfg.leakLevelEnabled() {
		update(levelTracks, d.cfg.LeakLevelMM, d.cfg.LeakLevelDropMM)
	}
	if d.cfg.leakTemperatureEnabled() {
		update(temperatureTracks, d.cfg.LeakTemperatureC, d.cfg.LeakTemperatureDropC)
	}

	maturedPressure := maturedTracks(pressureTracks, now, d.cfg)
	latestAnomaly := latestLeakAnomaly(pressureTracks)
	if len(maturedPressure) == 0 {
		// Hold while the pressure probe is silent: a lost probe cannot clear
		// a leak alarm, nor re-arm it.
		if state.leak.raised && (!pressureFresh || !d.recovered(latestAnomaly, now)) {
			return pendingAlarm{}, false
		}
		state.leak.raised = false
		return pendingAlarm{}, false
	}

	// The pressure loss must be corroborated over the same window by a
	// falling level or temperature, so a single-sensor glitch cannot raise a
	// leak alarm.
	corroborating := make([]model.Sample, 0)
	confirmations := 0
	for _, t := range maturedTracks(levelTracks, now, d.cfg) {
		confirmations++
		corroborating = append(corroborating, t.window.samples...)
	}
	for _, t := range maturedTracks(temperatureTracks, now, d.cfg) {
		confirmations++
		corroborating = append(corroborating, t.window.samples...)
	}
	if confirmations == 0 {
		return pendingAlarm{}, false
	}
	if state.leak.raised {
		return pendingAlarm{}, false
	}
	state.leak.raised = true
	pressureEvidence := make([]model.Sample, 0)
	for _, t := range maturedPressure {
		pressureEvidence = append(pressureEvidence, t.window.samples...)
	}
	evidence := append(model.EvidenceFromSamples(pressureEvidence),
		model.EvidenceFromSamples(corroborating)...)
	primary := maturedPressure[0]
	latest := primary.window.samples[len(primary.window.samples)-1].Value
	return pendingAlarm{
		vesselID: vesselID,
		kind:     model.AlarmLeak,
		message: fmt.Sprintf(
			"suspected nitrogen leak on vessel %s: pressure fell to %.1f kPa with %d corroborating sensor signal(s)",
			vesselID, latest, confirmations),
		evidence: evidence,
	}, true
}

func maturedTracks(tracks []*probeTracks, now time.Time, cfg Config) []*probeTracks {
	result := make([]*probeTracks, 0)
	for _, t := range tracks {
		if t.leak.matured(now, cfg.DetectWindow, cfg.MinSamples) {
			result = append(result, t)
		}
	}
	return result
}

func latestLeakAnomaly(tracks []*probeTracks) time.Time {
	latest := time.Time{}
	for _, t := range tracks {
		if t.leak.lastAnomalyAt.After(latest) {
			latest = t.leak.lastAnomalyAt
		}
	}
	return latest
}

// recovered reports whether every anomaly marker has either gone quiet or
// aged out of the detection window. A single normal reading therefore neither
// clears nor re-fires an alarm: single-point noise is filtered in both
// directions.
func (d *Detector) recovered(latestAnomaly time.Time, now time.Time) bool {
	return latestAnomaly.IsZero() || now.Sub(latestAnomaly) >= d.cfg.DetectWindow
}

func (c Config) leakLevelEnabled() bool {
	return c.LeakLevelMM != 0 || c.LeakLevelDropMM > 0
}

func (c Config) leakTemperatureEnabled() bool {
	return c.LeakTemperatureC != 0 || c.LeakTemperatureDropC > 0
}

func (d *Detector) dispatch(ctx context.Context, pending []pendingAlarm) {
	for _, alarm := range pending {
		if _, err := d.alarms.RaiseWithEvidence(ctx, alarm.vesselID, alarm.kind, alarm.message, alarm.evidence); err != nil {
			// The anomaly is already latched as raised; delivery retries are
			// handled by the notification retry queue, so the same anomaly is
			// not dispatched on every subsequent sample.
			_ = err
		}
	}
}
