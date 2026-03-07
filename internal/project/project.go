package project

import (
	"context"
	"fmt"
	"time"

	"github.com/claw-works/claw-hub/internal/store"
	"github.com/google/uuid"
)

// User represents a tenant in the SaaS multi-tenant model.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Project belongs to a User and groups Tasks and Agents.
type Project struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
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
		CreatedAt: time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO users (id, name, api_key, created_at) VALUES ($1,$2,$3,$4)`,
		u.ID, u.Name, u.APIKey, u.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *PGStore) GetUser(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, name, api_key, created_at FROM users WHERE id=$1`, id,
	).Scan(&u.ID, &u.Name, &u.APIKey, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *PGStore) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, name, api_key, created_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Name, &u.APIKey, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// ── Project ───────────────────────────────────────────────────────────────────

func (s *PGStore) CreateProject(ctx context.Context, userID, name string) (*Project, error) {
	p := &Project{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		CreatedAt: time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO projects (id, user_id, name, created_at) VALUES ($1,$2,$3,$4)`,
		p.ID, p.UserID, p.Name, p.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

func (s *PGStore) GetProject(ctx context.Context, id string) (*Project, error) {
	p := &Project{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, user_id, name, created_at FROM projects WHERE id=$1`, id,
	).Scan(&p.ID, &p.UserID, &p.Name, &p.CreatedAt)
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
			`SELECT id, user_id, name, created_at FROM projects WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	} else {
		rows, err = s.db.PG.Query(ctx,
			`SELECT id, user_id, name, created_at FROM projects ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []*Project
	for rows.Next() {
		p := &Project{}
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
