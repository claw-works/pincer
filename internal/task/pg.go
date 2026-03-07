package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/claw-works/claw-hub/internal/store"
	"github.com/google/uuid"
)

// PGStore persists tasks in PostgreSQL.
type PGStore struct {
	db *store.DB
}

func NewPGStore(db *store.DB) *PGStore {
	return &PGStore{db: db}
}

func (s *PGStore) Create(ctx context.Context, title, description string, required []string, priority Priority, rc *ReportChannel, projectID string) (*Task, error) {
	t := &Task{
		ID:                   uuid.New().String(),
		Title:                title,
		Description:          description,
		RequiredCapabilities: required,
		Priority:             priority,
		Status:               StatusPending,
		ReportChannel:        rc,
		ProjectID:            projectID,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}

	var rcJSON []byte
	if rc != nil {
		var err error
		rcJSON, err = json.Marshal(rc)
		if err != nil {
			return nil, fmt.Errorf("create task: marshal report_channel: %w", err)
		}
	}

	var pid *string
	if projectID != "" {
		pid = &projectID
	}

	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO tasks (id, title, description, required_capabilities, priority, status, report_channel, project_id, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		t.ID, t.Title, t.Description, t.RequiredCapabilities, t.Priority, string(t.Status), rcJSON, pid, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	s.db.LogTaskEvent(ctx, t.ID, "created", t)
	return t, nil
}

func (s *PGStore) Get(ctx context.Context, id string) (*Task, error) {
	row := s.db.PG.QueryRow(ctx,
		`SELECT id,title,description,required_capabilities,priority,status,
		        assigned_agent_id,result,error,report_channel,assigned_at,project_id,created_at,updated_at,completed_at
		 FROM tasks WHERE id=$1`, id)
	return scanTask(row)
}

// ListFilter holds optional filters for List queries.
type ListFilter struct {
	Status     string // "" = all, "active" = not done/failed, or exact status
	ProjectID  string // filter by project
	AssignedTo string // filter by agent id
}

func (s *PGStore) List(ctx context.Context, statusFilter string) ([]*Task, error) {
	return s.ListFiltered(ctx, ListFilter{Status: statusFilter})
}

func (s *PGStore) ListFiltered(ctx context.Context, f ListFilter) ([]*Task, error) {
	base := `SELECT id,title,description,required_capabilities,priority,status,
		        assigned_agent_id,result,error,report_channel,assigned_at,project_id,created_at,updated_at,completed_at
		 FROM tasks WHERE 1=1`
	args := []interface{}{}
	n := 1

	switch f.Status {
	case "active":
		base += " AND status NOT IN ('done','failed')"
	case "":
		// no filter
	default:
		base += fmt.Sprintf(" AND status=$%d", n)
		args = append(args, f.Status)
		n++
	}

	if f.ProjectID != "" {
		base += fmt.Sprintf(" AND project_id=$%d", n)
		args = append(args, f.ProjectID)
		n++
	}

	if f.AssignedTo != "" {
		base += fmt.Sprintf(" AND assigned_agent_id=$%d", n)
		args = append(args, f.AssignedTo)
		n++
	}

	base += " ORDER BY priority DESC, created_at ASC"

	rows, err := s.db.PG.Query(ctx, base, args...)
	if err != nil {
		return nil, err
	}
	type closer interface{ Close() }
	if c, ok := rows.(closer); ok {
		defer c.Close()
	}
	type rower interface {
		Next() bool
		Scan(...any) error
		Err() error
	}
	r := rows.(rower)
	tasks := make([]*Task, 0)
	for r.Next() {
		t, err := scanTask(r)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, r.Err()
}

func (s *PGStore) ListRecent(ctx context.Context, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.PG.Query(ctx,
		`SELECT id,title,description,required_capabilities,priority,status,
		        assigned_agent_id,result,error,report_channel,assigned_at,project_id,created_at,updated_at,completed_at
		 FROM tasks ORDER BY updated_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	type closer interface{ Close() }
	if c, ok := rows.(closer); ok {
		defer c.Close()
	}
	type rower interface {
		Next() bool
		Scan(...any) error
		Err() error
	}
	r := rows.(rower)
	tasks := make([]*Task, 0)
	for r.Next() {
		t, err := scanTask(r)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, r.Err()
}

func (s *PGStore) Claim(ctx context.Context, id, agentID string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='assigned', assigned_agent_id=$2, assigned_at=NOW(), updated_at=NOW()
		 WHERE id=$1 AND status='pending'`, id, agentID)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "claimed", map[string]string{"agent_id": agentID})
	return true
}

// Start transitions a task from assigned → running (agent sent TASK_UPDATE status=running).
func (s *PGStore) Start(ctx context.Context, id string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='running', updated_at=NOW()
		 WHERE id=$1 AND status='assigned'`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "started", nil)
	return true
}

// ReassignStale resets tasks stuck in 'running' or 'assigned' longer than timeout back to 'pending'.
// Returns the list of stale agent IDs (so the caller can also mark the agents online).
func (s *PGStore) ReassignStale(ctx context.Context, timeout time.Duration) []string {
	rows, err := s.db.PG.Query(ctx,
		`UPDATE tasks SET status='pending', assigned_agent_id=NULL, assigned_at=NULL, updated_at=NOW()
		 WHERE status IN ('running', 'assigned') AND assigned_at < NOW() - $1::interval
		 RETURNING id, assigned_agent_id`,
		fmt.Sprintf("%d seconds", int(timeout.Seconds())),
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var taskID, agentID string
		_ = rows.Scan(&taskID, &agentID)
		ids = append(ids, agentID) // return agent IDs so caller can set them online
		s.db.LogTaskEvent(ctx, taskID, "reassigned", map[string]string{"reason": "ack_timeout"})
	}
	return ids
}

func (s *PGStore) Complete(ctx context.Context, id, result string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='done', result=$2, updated_at=NOW(), completed_at=NOW()
		 WHERE id=$1 AND status IN ('running', 'assigned')`, id, result)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "completed", map[string]string{"result": result})
	return true
}

func (s *PGStore) Fail(ctx context.Context, id, errMsg string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='failed', error=$2, updated_at=NOW(), completed_at=NOW()
		 WHERE id=$1 AND status IN ('running', 'assigned')`, id, errMsg)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "failed", map[string]string{"error": errMsg})
	return true
}

// LogEvent logs a task lifecycle event to MongoDB.
func (s *PGStore) LogEvent(ctx context.Context, taskID, event string, payload interface{}) {
	s.db.LogTaskEvent(ctx, taskID, event, payload)
}

// GetEvents retrieves the audit log for a task from MongoDB.
func (s *PGStore) GetEvents(ctx context.Context, taskID string) ([]store.TaskEvent, error) {
	return s.db.GetTaskEvents(ctx, taskID)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(s scanner) (*Task, error) {
	t := &Task{}
	var status string
	var assignedAgentID, result, errMsg, projectID *string
	var rcJSON []byte
	err := s.Scan(
		&t.ID, &t.Title, &t.Description, &t.RequiredCapabilities,
		&t.Priority, &status, &assignedAgentID, &result, &errMsg, &rcJSON,
		&t.AssignedAt, &projectID, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	t.Status = Status(status)
	if assignedAgentID != nil {
		t.AssignedAgentID = *assignedAgentID
	}
	if result != nil {
		t.Result = *result
	}
	if errMsg != nil {
		t.ErrorMsg = *errMsg
	}
	if projectID != nil {
		t.ProjectID = *projectID
	}
	if len(rcJSON) > 0 {
		var rc ReportChannel
		if err := json.Unmarshal(rcJSON, &rc); err == nil {
			t.ReportChannel = &rc
		}
	}
	return t, nil
}

