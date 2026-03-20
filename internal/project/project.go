package project

import (
	"context"
	"fmt"
	"time"

	"github.com/claw-works/pincer/internal/store"
	"github.com/google/uuid"
)

// User represents a tenant in the SaaS multi-tenant model.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key,omitempty"`
	IsHuman   bool      `json:"is_human"`
	RoomID    string    `json:"room_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Project belongs to a User and groups Tasks and Agents.
type Project struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Repo        string    `json:"repo"`
	Description string    `json:"description"`
	Overview    string    `json:"overview"`
	RoomID      string    `json:"room_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// PGStore handles User and Project persistence in PostgreSQL.
type PGStore struct {
	db *store.DB
}

func NewPGStore(db *store.DB) *PGStore {
	return &PGStore{db: db}
}

// ── User ──────────────────────────────────────────────────────────────────────

func (s *PGStore) CreateUser(ctx context.Context, name string) (*User, error) {
	u := &User{
		ID:        uuid.New().String(),
		Name:      name,
		APIKey:    uuid.New().String(), // simple random API key for now
		RoomID:    uuid.New().String(), // opaque room ID — does not expose user ID
		CreatedAt: time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO users (id, name, api_key, room_id, created_at) VALUES ($1,$2,$3,$4,$5)`,
		u.ID, u.Name, u.APIKey, u.RoomID, u.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *PGStore) GetUser(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, name, api_key, is_human, COALESCE(room_id, ''), created_at, updated_at FROM users WHERE id=$1`, id,
	).Scan(&u.ID, &u.Name, &u.APIKey, &u.IsHuman, &u.RoomID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *PGStore) GetUserByAPIKey(ctx context.Context, apiKey string) (*User, error) {
	u := &User{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, name, api_key, is_human, COALESCE(room_id, ''), created_at, updated_at FROM users WHERE api_key=$1`, apiKey,
	).Scan(&u.ID, &u.Name, &u.APIKey, &u.IsHuman, &u.RoomID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user by api key: %w", err)
	}
	return u, nil
}

func (s *PGStore) SetIsHuman(ctx context.Context, userID string, isHuman bool) error {
	_, err := s.db.PG.Exec(ctx,
		`UPDATE users SET is_human = $1, updated_at = NOW() WHERE id = $2`,
		isHuman, userID,
	)
	return err
}

func (s *PGStore) ResetAPIKey(ctx context.Context, userID string) (string, error) {
	newKey := uuid.New().String()
	_, err := s.db.PG.Exec(ctx,
		`UPDATE users SET api_key = $1 WHERE id = $2`,
		newKey, userID,
	)
	if err != nil {
		return "", fmt.Errorf("reset api key: %w", err)
	}
	return newKey, nil
}

func (s *PGStore) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, name, api_key, is_human, COALESCE(room_id, ''), created_at, updated_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Name, &u.APIKey, &u.IsHuman, &u.RoomID, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpsertHumanByName registers a human identity for the current user.
// If a user with the given name already exists (different record), return that record directly
// as the caller's identity — key is NOT changed (key is auth credential, name is identity).
// The current anonymous record is deleted to avoid duplicates.
// If no user with that name exists, update the current record: set name and is_human = true.
func (s *PGStore) UpsertHumanByName(ctx context.Context, currentUserID, name string) (*User, error) {
	tx, err := s.db.PG.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert human: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Check for an existing user with this name (different record).
	var existingID string
	err = tx.QueryRow(ctx,
		`SELECT id FROM users WHERE name = $1 AND id != $2`, name, currentUserID,
	).Scan(&existingID)

	var targetID string
	if err == nil {
		// Existing record found: associate current key with that identity.
		// Update existing record's api_key to the current session's key,
		// so future logins with this key return the named user directly.
		var currentAPIKey string
		if err := tx.QueryRow(ctx,
			`SELECT api_key FROM users WHERE id = $1`, currentUserID,
		).Scan(&currentAPIKey); err != nil {
			return nil, fmt.Errorf("upsert human: get current key: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET api_key = $1, is_human = true, updated_at = NOW() WHERE id = $2`,
			currentAPIKey, existingID,
		); err != nil {
			return nil, fmt.Errorf("upsert human: update existing: %w", err)
		}
		// Delete the anonymous record.
		if _, err := tx.Exec(ctx,
			`DELETE FROM users WHERE id = $1`, currentUserID,
		); err != nil {
			return nil, fmt.Errorf("upsert human: delete current: %w", err)
		}
		targetID = existingID
	} else {
		// No existing record: update current user in place.
		if _, err := tx.Exec(ctx,
			`UPDATE users SET name = $1, is_human = true, updated_at = NOW() WHERE id = $2`,
			name, currentUserID,
		); err != nil {
			return nil, fmt.Errorf("upsert human: update current: %w", err)
		}
		targetID = currentUserID
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("upsert human: commit: %w", err)
	}

	return s.GetUser(ctx, targetID)
}

// ── Project ───────────────────────────────────────────────────────────────────

func (s *PGStore) CreateProject(ctx context.Context, userID, name, repo, description, overview string) (*Project, error) {
	p := &Project{
		ID:          uuid.New().String(),
		UserID:      userID,
		Name:        name,
		Repo:        repo,
		Description: description,
		Overview:    overview,
		RoomID:      uuid.New().String(),
		CreatedAt:   time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO projects (id, user_id, name, repo, description, overview, room_id, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.ID, p.UserID, p.Name, p.Repo, p.Description, p.Overview, p.RoomID, p.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

func (s *PGStore) GetProject(ctx context.Context, id string) (*Project, error) {
	p := &Project{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, user_id, name, repo, description, overview, COALESCE(room_id,''), created_at FROM projects WHERE id=$1`, id,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.Repo, &p.Description, &p.Overview, &p.RoomID, &p.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

func (s *PGStore) ListProjects(ctx context.Context, userID string) ([]*Project, error) {
	var rows interface {
		Next() bool
		Scan(...any) error
		Err() error
		Close()
	}
	var err error
	if userID != "" {
		rows, err = s.db.PG.Query(ctx,
			`SELECT id, user_id, name, repo, description, overview, COALESCE(room_id,''), created_at FROM projects WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	} else {
		rows, err = s.db.PG.Query(ctx,
			`SELECT id, user_id, name, repo, description, overview, COALESCE(room_id,''), created_at FROM projects ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []*Project
	for rows.Next() {
		p := &Project{}
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Repo, &p.Description, &p.Overview, &p.RoomID, &p.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *PGStore) UpdateProject(ctx context.Context, id, repo, description, overview string) (*Project, error) {
	_, err := s.db.PG.Exec(ctx,
		`UPDATE projects SET repo=$2, description=$3, overview=$4 WHERE id=$1`,
		id, repo, description, overview,
	)
	if err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	return s.GetProject(ctx, id)
}

func (s *PGStore) DeleteProject(ctx context.Context, id string) error {
	_, err := s.db.PG.Exec(ctx, `DELETE FROM projects WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return nil
}
