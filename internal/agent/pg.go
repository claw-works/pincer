package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/claw-works/pincer/internal/store"
	"github.com/google/uuid"
)

// PGRegistry persists agents in PostgreSQL.
type PGRegistry struct {
	db *store.DB
}

func NewPGRegistry(db *store.DB) *PGRegistry {
	return &PGRegistry{db: db}
}

// Register creates or updates an agent. If id is non-empty, that ID is used
// (idempotent upsert: re-registering an existing agent updates its fields and
// returns the existing record — no ghost agents). If id is empty, a new UUID
// is generated.
func (r *PGRegistry) Register(ctx context.Context, id, name string, capabilities []string, agentType AgentType, userID string) (*Agent, error) {
	if id == "" {
		id = uuid.New().String()
	}
	if agentType == "" {
		agentType = TypeAgent
	}
	now := time.Now()
	var uid interface{}
	if userID != "" {
		uid = userID
	}
	_, err := r.db.PG.Exec(ctx,
		`INSERT INTO agents (id, name, type, capabilities, status, registered_at, last_heartbeat, user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE
		   SET name=$2, type=$3, capabilities=$4, status=$5, last_heartbeat=$7`,
		id, name, string(agentType), capabilities, string(StatusOnline), now, now, uid,
	)
	if err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}
	return r.Get(ctx, id)
}

func (r *PGRegistry) Heartbeat(ctx context.Context, id string) bool {
	tag, err := r.db.PG.Exec(ctx,
		`UPDATE agents SET status='online', last_heartbeat=NOW() WHERE id=$1`, id)
	return err == nil && tag.RowsAffected() > 0
}

func (r *PGRegistry) Get(ctx context.Context, id string) (*Agent, error) {
	row := r.db.PG.QueryRow(ctx,
		`SELECT id, name, COALESCE(type, 'agent'), capabilities, status, registered_at, last_heartbeat, COALESCE(user_id,'') FROM agents WHERE id=$1`, id)
	return scanAgent(row)
}

func (r *PGRegistry) List(ctx context.Context, userID string) ([]*Agent, error) {
	var rows interface {
		Close()
		Next() bool
		Err() error
		Scan(dest ...any) error
	}
	var err error
	if userID != "" {
		rows, err = r.db.PG.Query(ctx,
			`SELECT id, name, COALESCE(type, 'agent'), capabilities, status, registered_at, last_heartbeat, COALESCE(user_id,'')
			 FROM agents WHERE user_id=$1 ORDER BY registered_at DESC`, userID)
	} else {
		rows, err = r.db.PG.Query(ctx,
			`SELECT id, name, COALESCE(type, 'agent'), capabilities, status, registered_at, last_heartbeat, COALESCE(user_id,'')
			 FROM agents ORDER BY registered_at DESC`)
	}
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
		`SELECT id, name, COALESCE(type, 'agent'), capabilities, status, registered_at, last_heartbeat, COALESCE(user_id,'')
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
	var status, agentType string
	err := s.Scan(&a.ID, &a.Name, &agentType, &a.Capabilities, &status, &a.RegisteredAt, &a.LastHeartbeat, &a.UserID)
	if err != nil {
		return nil, err
	}
	a.Status = Status(status)
	a.Type = AgentType(agentType)
	return a, nil
}

// Delete removes an agent by ID.
func (r *PGRegistry) Delete(ctx context.Context, id string) error {
	_, err := r.db.PG.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	return err
}

// FindOrCreateHumanAgent finds an existing human-type agent linked to a user,
// or creates a new one. Returns the agent object so callers never expose user.ID.
func (r *PGRegistry) FindOrCreateHumanAgent(ctx context.Context, userID, name string) (*Agent, error) {
	// 1. Find by user_id (fast path).
	var existingID string
	err := r.db.PG.QueryRow(ctx,
		`SELECT id FROM agents WHERE user_id=$1 AND type='human' LIMIT 1`, userID,
	).Scan(&existingID)
	if err == nil {
		// Found — update name in case it changed.
		_, _ = r.db.PG.Exec(ctx,
			`UPDATE agents SET name=$1, last_heartbeat=NOW() WHERE id=$2`, name, existingID)
		return r.Get(ctx, existingID)
	}

	// 2. Fallback: find by name (handles agents created before user_id column existed).
	err = r.db.PG.QueryRow(ctx,
		`SELECT id FROM agents WHERE name=$1 AND type='human' AND user_id IS NULL LIMIT 1`, name,
	).Scan(&existingID)
	if err == nil {
		// Backfill user_id so future lookups use the fast path.
		_, _ = r.db.PG.Exec(ctx,
			`UPDATE agents SET user_id=$1, last_heartbeat=NOW() WHERE id=$2`, userID, existingID)
		return r.Get(ctx, existingID)
	}

	// 3. Not found — create a new human agent.
	id := uuid.New().String()
	now := time.Now()
	_, err = r.db.PG.Exec(ctx,
		`INSERT INTO agents (id, name, type, capabilities, status, registered_at, last_heartbeat, user_id)
		 VALUES ($1, $2, 'human', '{}', 'offline', $3, $3, $4)`,
		id, name, now, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("create human agent: %w", err)
	}
	return r.Get(ctx, id)
}
