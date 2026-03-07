package agent

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusOnline  Status = "online"
	StatusOffline Status = "offline"
	StatusBusy    Status = "busy"
)

type Agent struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Capabilities []string  `json:"capabilities"`
	Status       Status    `json:"status"`
	RegisteredAt time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent)}
}

func (r *Registry) Register(name string, capabilities []string) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := &Agent{
		ID:            uuid.New().String(),
		Name:          name,
		Capabilities:  capabilities,
		Status:        StatusOnline,
		RegisteredAt:  time.Now(),
		LastHeartbeat: time.Now(),
	}
	r.agents[a.ID] = a
	return a
}

func (r *Registry) Heartbeat(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return false
	}
	a.LastHeartbeat = time.Now()
	a.Status = StatusOnline
	return true
}

func (r *Registry) Get(id string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[id]
	return a, ok
}

func (r *Registry) List() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		list = append(list, a)
	}
	return list
}

// FindCapable returns online agents that have all required capabilities.
func (r *Registry) FindCapable(required []string) []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Agent
outer:
	for _, a := range r.agents {
		if a.Status != StatusOnline {
			continue
		}
		capSet := make(map[string]bool)
		for _, c := range a.Capabilities {
			capSet[c] = true
		}
		for _, req := range required {
			if !capSet[req] {
				continue outer
			}
		}
		result = append(result, a)
	}
	return result
}

// MarkOfflineStale marks agents as offline if no heartbeat in timeout.
func (r *Registry) MarkOfflineStale(timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, a := range r.agents {
		if a.Status == StatusOnline && now.Sub(a.LastHeartbeat) > timeout {
			a.Status = StatusOffline
		}
	}
}
