package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOpts "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type DB struct {
	PG    *pgxpool.Pool
	Mongo *mongo.Database
}

func Connect(ctx context.Context, pgDSN, mongoURI, mongoDBName string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(pgDSN)
	if err != nil {
		return nil, fmt.Errorf("pg parse config: %w", err)
	}
	// Disable prepared statements — required when using pgBouncer/Supabase
	// pooler in transaction mode (port 6543). Prepared statements don't
	// survive across pooled connections and cause "prepared statement does not
	// exist" errors.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	if err := pg.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	log.Println("store: PostgreSQL connected")

	// MongoDB is optional — if MONGO_URI is empty or unreachable, inbox/rooms
	// fall back to in-memory nop implementations.
	var mongoDB *mongo.Database
	if mongoURI != "" {
		mc, merr := mongo.Connect(mongoOpts.Client().ApplyURI(mongoURI).
			SetConnectTimeout(10 * time.Second))
		if merr != nil {
			log.Printf("store: MongoDB connect skipped (%v) — inbox/rooms disabled", merr)
		} else if merr = mc.Ping(ctx, nil); merr != nil {
			log.Printf("store: MongoDB ping failed (%v) — inbox/rooms disabled", merr)
		} else {
			mongoDB = mc.Database(mongoDBName)
			log.Println("store: MongoDB connected")
		}
	} else {
		log.Println("store: MONGO_URI not set — inbox/rooms running without persistence")
	}

	return &DB{PG: pg, Mongo: mongoDB}, nil
}

// Migrate runs idempotent DDL for PostgreSQL.
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.PG.Exec(ctx, `
		-- ── Multi-tenant: users & projects ───────────────────────────────────────
		CREATE TABLE IF NOT EXISTS users (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			api_key    TEXT NOT NULL UNIQUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS projects (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id),
			name       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- ── Agents ───────────────────────────────────────────────────────────────
		CREATE TABLE IF NOT EXISTS agents (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			capabilities    TEXT[] NOT NULL DEFAULT '{}',
			status          TEXT NOT NULL DEFAULT 'online',
			registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_heartbeat  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- ── Tasks ────────────────────────────────────────────────────────────────
		CREATE TABLE IF NOT EXISTS tasks (
			id                    TEXT PRIMARY KEY,
			title                 TEXT NOT NULL,
			description           TEXT NOT NULL DEFAULT '',
			required_capabilities TEXT[] NOT NULL DEFAULT '{}',
			priority              INT  NOT NULL DEFAULT 5,
			status                TEXT NOT NULL DEFAULT 'pending',
			assigned_agent_id     TEXT,
			result                TEXT,
			error                 TEXT,
			report_channel        JSONB,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at          TIMESTAMPTZ
		);

		-- Idempotent column additions
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS report_channel      JSONB;
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS assigned_at         TIMESTAMPTZ;
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS project_id          TEXT REFERENCES projects(id);
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS guidance            TEXT NOT NULL DEFAULT '';
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS acceptance_criteria TEXT NOT NULL DEFAULT '';
		ALTER TABLE tasks    ADD COLUMN IF NOT EXISTS review_note         TEXT;
		ALTER TABLE agents   ADD COLUMN IF NOT EXISTS user_id             TEXT REFERENCES users(id);
		ALTER TABLE agents   ADD COLUMN IF NOT EXISTS type                TEXT NOT NULL DEFAULT 'agent';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS repo                TEXT NOT NULL DEFAULT '';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS description         TEXT NOT NULL DEFAULT '';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS overview            TEXT NOT NULL DEFAULT '';
		ALTER TABLE users    ADD COLUMN IF NOT EXISTS is_human            BOOLEAN NOT NULL DEFAULT false;
		ALTER TABLE users    ADD COLUMN IF NOT EXISTS updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW();
		ALTER TABLE users    ADD COLUMN IF NOT EXISTS room_id             TEXT;
		-- Assign opaque room UUIDs to existing users that don't have one yet.
		UPDATE users SET room_id = gen_random_uuid()::text WHERE room_id IS NULL;

		-- Deduplicate users by name: keep the most recently updated record, delete others.
		-- Safe to run multiple times (idempotent). Only deletes rows with no dependent projects/agents.
		DELETE FROM users
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
					ROW_NUMBER() OVER (
						PARTITION BY name
						ORDER BY updated_at DESC, created_at DESC
					) AS rn
				FROM users
			) sub
			WHERE rn > 1
		)
		AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.user_id = users.id)
		AND NOT EXISTS (SELECT 1 FROM agents  a WHERE a.user_id = users.id);

		CREATE TABLE IF NOT EXISTS daily_reports (
			id         TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			date       TEXT NOT NULL,
			summary    TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (project_id, date)
		);

		-- Indexes for common queries
		CREATE INDEX IF NOT EXISTS idx_tasks_project_id   ON tasks(project_id);
		CREATE INDEX IF NOT EXISTS idx_tasks_assigned_to  ON tasks(assigned_agent_id);
		CREATE INDEX IF NOT EXISTS idx_agents_user_id     ON agents(user_id);
		CREATE INDEX IF NOT EXISTS idx_projects_user_id   ON projects(user_id);

		-- ── Report Jobs & Agent Reports ──────────────────────────────────────────
		CREATE TABLE IF NOT EXISTS report_jobs (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			cron_expr   TEXT NOT NULL DEFAULT '',
			agent_id    TEXT REFERENCES agents(id),
			enabled     BOOLEAN NOT NULL DEFAULT true,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS agent_reports (
			id         TEXT PRIMARY KEY,
			job_id     TEXT NOT NULL REFERENCES report_jobs(id) ON DELETE CASCADE,
			agent_id   TEXT REFERENCES agents(id),
			title      TEXT NOT NULL,
			content    TEXT NOT NULL DEFAULT '',
			metadata   JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_agent_reports_job_id    ON agent_reports(job_id);
		CREATE INDEX IF NOT EXISTS idx_agent_reports_created_at ON agent_reports(created_at DESC);

		-- BMAD Method compatibility: task hierarchy + structured fields
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_task_id TEXT REFERENCES tasks(id);
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS task_type      TEXT NOT NULL DEFAULT 'task';
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS user_story     TEXT;
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS acceptance_criteria JSONB;
		CREATE INDEX IF NOT EXISTS idx_tasks_parent_id ON tasks(parent_task_id);

		-- P0 security: tenant isolation
		ALTER TABLE tasks  ADD COLUMN IF NOT EXISTS owner_id TEXT REFERENCES users(id);
		CREATE INDEX IF NOT EXISTS idx_tasks_owner_id ON tasks(owner_id);
	`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Println("store: migrations OK")
	return nil
}

// LogTaskEvent writes a task lifecycle event to MongoDB (audit log).
func (db *DB) LogTaskEvent(ctx context.Context, taskID, event string, payload interface{}) {
	coll := db.Mongo.Collection("task_events")
	doc := bson.M{
		"task_id":    taskID,
		"event":      event,
		"payload":    payload,
		"created_at": time.Now(),
	}
	if _, err := coll.InsertOne(ctx, doc); err != nil {
		log.Printf("store: mongo log error: %v", err)
	}
}

// TaskEvent represents an audit log entry for a task lifecycle event.
type TaskEvent struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	TaskID    string        `bson:"task_id" json:"task_id"`
	Event     string        `bson:"event" json:"event"`
	Payload   interface{}   `bson:"payload" json:"payload"`
	CreatedAt time.Time     `bson:"created_at" json:"created_at"`
}

// GetTaskEvents retrieves all audit log entries for a task from MongoDB.
func (db *DB) GetTaskEvents(ctx context.Context, taskID string) ([]TaskEvent, error) {
	coll := db.Mongo.Collection("task_events")
	opts := mongoOpts.Find().SetSort(bson.M{"created_at": 1})
	cursor, err := coll.Find(ctx, bson.M{"task_id": taskID}, opts)
	if err != nil {
		return nil, fmt.Errorf("get task events: %w", err)
	}
	defer cursor.Close(ctx)

	var events []TaskEvent
	if err := cursor.All(ctx, &events); err != nil {
		return nil, fmt.Errorf("decode task events: %w", err)
	}
	return events, nil
}
