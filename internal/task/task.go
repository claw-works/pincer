package task

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type Status string
type Priority int

const (
	StatusPending  Status = "pending"
	StatusAssigned Status = "assigned" // task claimed by agent, not yet started
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"

	PriorityLow    Priority = 0
	PriorityNormal Priority = 5
	PriorityHigh   Priority = 10
)

// ReportChannel describes where to send task completion notifications.
// Hub calls WebhookURL (if set) with an HTTP POST when the task finishes.
// DiscordThreadID / FeishuChatID are opaque metadata forwarded in the webhook
// payload so the receiver knows which channel to post into — hub never calls
// Discord or Feishu directly.
type ReportChannel struct {
	WebhookURL      string `json:"webhook_url,omitempty"`
	DiscordThreadID string `json:"discord_thread_id,omitempty"`
	FeishuChatID    string `json:"feishu_chat_id,omitempty"`
}

type Task struct {
	ID                   string          `json:"id"`
	Title                string          `json:"title"`
	Description          string          `json:"description"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	Priority             Priority        `json:"priority"`
	Status               Status          `json:"status"`
	AssignedAgentID      string          `json:"assigned_agent_id,omitempty"`
	ProjectID            string          `json:"project_id,omitempty"`
	Result               string          `json:"result,omitempty"`
	ErrorMsg             string          `json:"error,omitempty"`
	ReportChannel        *ReportChannel  `json:"report_channel,omitempty"`
	AssignedAt           *time.Time      `json:"assigned_at,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	CompletedAt          *time.Time      `json:"completed_at,omitempty"`
}

type Store struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewStore() *Store {
	return &Store{tasks: make(map[string]*Task)}
}

func (s *Store) Create(title, description string, required []string, priority Priority) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Task{
		ID:                   uuid.New().String(),
		Title:                title,
		Description:          description,
		RequiredCapabilities: required,
		Priority:             priority,
		Status:               StatusPending,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}
	s.tasks[t.ID] = t
	return t
}

func (s *Store) Get(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	return t, ok
}

func (s *Store) List(statusFilter string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Task
	for _, t := range s.tasks {
		if statusFilter == "" || string(t.Status) == statusFilter {
			result = append(result, t)
		}
	}
	return result
}

func (s *Store) Claim(id, agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t.Status != StatusPending {
		return false
	}
	now := time.Now()
	t.Status = StatusAssigned
	t.AssignedAgentID = agentID
	t.AssignedAt = &now
	t.UpdatedAt = time.Now()
	return true
}

func (s *Store) Complete(id, result string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || (t.Status != StatusRunning && t.Status != StatusAssigned) {
		return false
	}
	now := time.Now()
	t.Status = StatusDone
	t.Result = result
	t.UpdatedAt = now
	t.CompletedAt = &now
	return true
}

func (s *Store) Fail(id, errMsg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || (t.Status != StatusRunning && t.Status != StatusAssigned) {
		return false
	}
	now := time.Now()
	t.Status = StatusFailed
	t.ErrorMsg = errMsg
	t.UpdatedAt = now
	t.CompletedAt = &now
	return true
}
