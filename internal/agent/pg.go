package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/claw-works/claw-hub/internal/store"
	"github.com/google/uuid"
)

// PGRegistry persists agents in PostgreSQL.
type PGRegistry struct {
	db *store.DB
}

func NewPGRegistry(db *store.DB) *PGRegistry {
	return &PGRegistry{db: db}
}

func (r *PGRegistry) Register(ctx context.Context, name string, capabilities []string) (*Agent, error) {
	a := &Agent{
		ID:            uuid.New().String(),
		Name:          name,
		Capabilities:  capabilities,
		Status:        StatusOnline,
		RegisteredAt:  time.Now(),
		LastHeartbeat: time.Now(),
	}
	_, err := r.db.PG.Exec(ctx,
		`INSERT INTO agents (id, name, capabilities, status, registered_at, last_heartbeat)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id) DO UPDATE SET name=$2, capabilities=$3, status=$4, last_heartbeat=$6`,
		a.ID, a.Name, a.Capabilities, string(a.Status), a.RegisteredAt, a.LastHeartbeat,
	)
	if err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}
	return a, nil
}

func (r *PGRegistry) Heartbeat(ctx context.Context, id string) bool {
	tag, err := r.db.PG.Exec(ctx,
		`UPDATE agents SET status='online', last_heartbeat=NOW() WHERE id=$1`, id)
	return err == nil && tag.RowsAffected() > 0
}

func (r *PGRegistry) Get(ctx context.Context, id string) (*Agent, error) {
	row := r.db.PG.QueryRow(ctx,
		`SELECT id, name, capabilities, status, registered_at, last_heartbeat FROM agents WHERE id=$1`, id)
	return scanAgent(row)
}

func (r *PGRegistry) List(ctx context.Context) ([]*Agent, error) {
	rows, err := r.db.PG.Query(ctx,
		`SELECT id, name, capabilities, status, registered_at, last_heartbeat FROM agents ORDER BY registered_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (r *PGRegistry) FindCapable(ctx context.Context, required []string) ([]*Agent, error) {
	rows, err := r.db.PG.Query(ctx,
		`SELECT id, name, capabilities, status, registered_at, last_heartbeat
		 FROM agents WHERE status='online' AND capabilities @> $1`, required)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (r *PGRegistry) MarkOfflineStale(ctx context.Context, timeout time.Duration) {
	r.db.PG.Exec(ctx,
		`UPDATE agents SET status='offline' WHERE status='online' AND last_heartbeat < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(timeout.Seconds())),
	)
}

// UpdateCapabilities updates the capabilities of an existing agent (called on WS REGISTER).
func (r *PGRegistry) UpdateCapabilities(ctx context.Context, id string, capabilities []string) error {
	_, err := r.db.PG.Exec(ctx,
		`UPDATE agents SET capabilities=$2, last_heartbeat=NOW() WHERE id=$1`, id, capabilities)
	return err
}

// SetBusy marks an agent as busy (task assigned, not available for new tasks).
func (r *PGRegistry) SetBusy(ctx context.Context, id string) {
	r.db.PG.Exec(ctx, `UPDATE agents SET status='busy' WHERE id=$1`, id)
}

// SetOnline marks an agent back to online (task completed or failed).
func (r *PGRegistry) SetOnline(ctx context.Context, id string) {
	r.db.PG.Exec(ctx, `UPDATE agents SET status='online' WHERE id=$1 AND status='busy'`, id)
}

// FindCapableAtomic finds an online capable agent and atomically claims it as busy.
// Returns the agent ID, or "" if none available.
func (r *PGRegistry) FindCapableAtomic(ctx context.Context, required []string) (string, error) {
	if len(required) == 0 {
		return "", nil
	}
	var id string
	err := r.db.PG.QueryRow(ctx,
		`UPDATE agents SET status='busy'
		 WHERE id = (
		   SELECT id FROM agents
		   WHERE status='online' AND capabilities @> $1
		   ORDER BY last_heartbeat DESC
		   LIMIT 1
		   FOR UPDATE SKIP LOCKED
		 )
		 RETURNING id`, required).Scan(&id)
	if err != nil {
		return "", nil // no capable online agent
	}
	return id, nil
}

// scanAgent works for both pgx.Row and pgx.Rows
type scanner interface {
	Scan(dest ...any) error
}

func scanAgent(s scanner) (*Agent, error) {
	a := &Agent{}
	var status string
	err := s.Scan(&a.ID, &a.Name, &a.Capabilities, &status, &a.RegisteredAt, &a.LastHeartbeat)
	if err != nil {
		return nil, err
	}
	a.Status = Status(status)
	return a, nil
}
