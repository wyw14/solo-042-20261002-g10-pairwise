package alarm

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-98/internal/model"
)

type Service struct {
	mu       sync.RWMutex
	alarms   map[uuid.UUID]model.Alarm
	delivery *DeliveryService
	now      func() time.Time
}

func NewService(delivery *DeliveryService, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{alarms: make(map[uuid.UUID]model.Alarm), delivery: delivery, now: now}
}

func (s *Service) Raise(ctx context.Context, vesselID string, kind model.AlarmKind, message string) (model.Alarm, error) {
	return s.RaiseWithEvidence(ctx, vesselID, kind, message, nil)
}

func (s *Service) RaiseWithEvidence(ctx context.Context, vesselID string, kind model.AlarmKind, message string, evidence []model.EvidenceSample) (model.Alarm, error) {
	alarm, err := model.NewAlarm(vesselID, kind, message, s.now())
	if err != nil {
		return model.Alarm{}, err
	}
	if len(evidence) > 0 {
		alarm = alarm.WithEvidence(evidence)
	}
	s.mu.Lock()
	s.alarms[alarm.ID] = alarm
	s.mu.Unlock()
	delivered, err := s.delivery.Deliver(ctx, alarm)
	s.mu.Lock()
	if delivered.ID != alarm.ID {
		// Delivery failed before producing a journaled alarm; keep the
		// detected alarm so it remains visible and retryable.
		delivered = alarm
	}
	s.alarms[alarm.ID] = delivered
	s.mu.Unlock()
	return delivered, err
}

func (s *Service) Restore(alarm model.Alarm) {
	s.mu.Lock()
	s.alarms[alarm.ID] = alarm
	s.mu.Unlock()
}

func (s *Service) Get(id uuid.UUID) (model.Alarm, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	alarm, ok := s.alarms[id]
	return alarm, ok
}

func (s *Service) List() []model.Alarm {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Alarm, 0, len(s.alarms))
	for _, alarm := range s.alarms {
		result = append(result, alarm)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (s *Service) Acknowledge(id uuid.UUID) (model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	alarm, ok := s.alarms[id]
	if !ok {
		return model.Alarm{}, errors.New("alarm not found")
	}
	alarm = alarm.Acknowledge(s.now())
	s.alarms[id] = alarm
	return alarm, nil
}

func (s *Service) Clear(id uuid.UUID) (model.Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	alarm, ok := s.alarms[id]
	if !ok {
		return model.Alarm{}, errors.New("alarm not found")
	}
	alarm = alarm.Clear(s.now())
	s.alarms[id] = alarm
	return alarm, nil
}
