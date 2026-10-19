package telemetry

import (
	"sync"
	"time"

	"github.com/RCooLeR/StarlinkBridge/internal/model"
)

type View struct {
	Snapshot *model.Snapshot `json:"snapshot,omitempty"`
	Health   model.Health    `json:"health"`
	Fresh    bool            `json:"fresh"`
}

type Store struct {
	mu         sync.RWMutex
	snapshot   *model.Snapshot
	health     model.Health
	staleAfter time.Duration
}

func NewStore(staleAfter time.Duration) *Store { return &Store{staleAfter: staleAfter} }

// Snapshots are immutable after publishing to the store.
func (s *Store) Update(snapshot *model.Snapshot, health model.Health) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot != nil {
		s.snapshot = snapshot
	}
	s.health = health
}

func (s *Store) View(now time.Time) View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fresh := s.snapshot != nil && !s.snapshot.CollectedAt.IsZero() && now.Sub(s.snapshot.CollectedAt) <= s.staleAfter
	return View{Snapshot: s.snapshot, Health: s.health, Fresh: fresh}
}
