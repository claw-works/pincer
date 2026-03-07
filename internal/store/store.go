package store

import (
	"context"
	"fmt"
	"log"
	"time"

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
	pg, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		return nil, fmt.Errorf("pg connect: %w", err)
	}
	if err := pg.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	log.Println("store: PostgreSQL connected")

	mc, err := mongo.Connect(mongoOpts.Client().ApplyURI(mongoURI).
		SetConnectTimeout(10 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := mc.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	log.Println("store: MongoDB connected")

	return &DB{PG: pg, Mongo: mc.Database(mongoDBName)}, nil
}

// Migrate runs idempotent DDL for PostgreSQL.
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.PG.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS agents (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			capabilities    TEXT[] NOT NULL DEFAULT '{}',
			status          TEXT NOT NULL DEFAULT 'online',
			registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_heartbeat  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

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

		-- Idempotent: add report_channel if the table already existed without it.
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS report_channel JSONB;
		-- Idempotent: add assigned_at for ACK timeout tracking.
		ALTER TABLE tasks ADD COLUMN IF NOT EXISTS assigned_at TIMESTAMPTZ;
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
