package task

import (
	"context"
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

func (s *PGStore) Create(ctx context.Context, title, description string, required []string, priority Priority) (*Task, error) {
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
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO tasks (id, title, description, required_capabilities, priority, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		t.ID, t.Title, t.Description, t.RequiredCapabilities, t.Priority, string(t.Status), t.CreatedAt, t.UpdatedAt,
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
		        assigned_agent_id,result,error,created_at,updated_at,completed_at
		 FROM tasks WHERE id=$1`, id)
	return scanTask(row)
}

func (s *PGStore) List(ctx context.Context, statusFilter string) ([]*Task, error) {
	var rows interface{ Scan(...any) error }
	var err error
	if statusFilter == "" {
		rows, err = s.db.PG.Query(ctx,
			`SELECT id,title,description,required_capabilities,priority,status,
			        assigned_agent_id,result,error,created_at,updated_at,completed_at
			 FROM tasks ORDER BY priority DESC, created_at ASC`)
	} else {
		rows, err = s.db.PG.Query(ctx,
			`SELECT id,title,description,required_capabilities,priority,status,
			        assigned_agent_id,result,error,created_at,updated_at,completed_at
			 FROM tasks WHERE status=$1 ORDER BY priority DESC, created_at ASC`, statusFilter)
	}
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
	var tasks []*Task
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
		`UPDATE tasks SET status='running', assigned_agent_id=$2, updated_at=NOW()
		 WHERE id=$1 AND status='pending'`, id, agentID)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "claimed", map[string]string{"agent_id": agentID})
	return true
}

func (s *PGStore) Complete(ctx context.Context, id, result string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='done', result=$2, updated_at=NOW(), completed_at=NOW()
		 WHERE id=$1 AND status='running'`, id, result)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "completed", map[string]string{"result": result})
	return true
}

func (s *PGStore) Fail(ctx context.Context, id, errMsg string) bool {
	tag, err := s.db.PG.Exec(ctx,
		`UPDATE tasks SET status='failed', error=$2, updated_at=NOW(), completed_at=NOW()
		 WHERE id=$1 AND status='running'`, id, errMsg)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	s.db.LogTaskEvent(ctx, id, "failed", map[string]string{"error": errMsg})
	return true
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(s scanner) (*Task, error) {
	t := &Task{}
	var status string
	var assignedAgentID, result, errMsg *string
	err := s.Scan(
		&t.ID, &t.Title, &t.Description, &t.RequiredCapabilities,
		&t.Priority, &status, &assignedAgentID, &result, &errMsg,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt,
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
	return t, nil
}
