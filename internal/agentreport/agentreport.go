package agentreport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/claw-works/pincer/internal/store"
	"github.com/google/uuid"
)

// Job defines a scheduled report task.
type Job struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CronExpr    string    `json:"cron_expr"`
	AgentID     string    `json:"agent_id"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Summary is a list-safe view of a report (no content field).
type Summary struct {
	ID        string    `json:"id"`
	JobID     string    `json:"job_id"`
	AgentID   string    `json:"agent_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// Report is the full report including content.
type Report struct {
	Summary
	Content  string          `json:"content"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Store handles report_jobs and agent_reports persistence.
type Store struct {
	db *store.DB
}

func NewStore(db *store.DB) *Store {
	return &Store{db: db}
}

// ── Report Jobs ───────────────────────────────────────────────────────────────

func (s *Store) CreateJob(ctx context.Context, name, description, cronExpr, agentID string) (*Job, error) {
	j := &Job{
		ID:          uuid.New().String(),
		Name:        name,
		Description: description,
		CronExpr:    cronExpr,
		AgentID:     agentID,
		Enabled:     true,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO report_jobs (id, name, description, cron_expr, agent_id, enabled, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		j.ID, j.Name, j.Description, j.CronExpr, nullStr(j.AgentID), j.Enabled, j.CreatedAt, j.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create report job: %w", err)
	}
	return j, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	j := &Job{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, name, description, cron_expr, COALESCE(agent_id,''), enabled, created_at, updated_at
		 FROM report_jobs WHERE id=$1`, id,
	).Scan(&j.ID, &j.Name, &j.Description, &j.CronExpr, &j.AgentID, &j.Enabled, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get report job: %w", err)
	}
	return j, nil
}

// UpdateJob updates mutable fields (name, description) of a report job.
// Only non-empty fields in the patch are applied.
func (s *Store) UpdateJob(ctx context.Context, id string, name, description *string) (*Job, error) {
	existing, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if name != nil && *name != "" {
		existing.Name = *name
	}
	if description != nil {
		existing.Description = *description
	}
	_, err = s.db.PG.Exec(ctx,
		`UPDATE report_jobs SET name=$1, description=$2, updated_at=NOW() WHERE id=$3`,
		existing.Name, existing.Description, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update report job: %w", err)
	}
	return s.GetJob(ctx, id)
}

func (s *Store) ListJobs(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, name, description, cron_expr, COALESCE(agent_id,''), enabled, created_at, updated_at
		 FROM report_jobs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j := &Job{}
		if err := rows.Scan(&j.ID, &j.Name, &j.Description, &j.CronExpr, &j.AgentID, &j.Enabled, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if jobs == nil {
		jobs = []*Job{}
	}
	return jobs, rows.Err()
}

// ── Agent Reports ─────────────────────────────────────────────────────────────

// DeleteJob removes a report job and all its associated reports.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	_, err := s.db.PG.Exec(ctx, `DELETE FROM agent_reports WHERE job_id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete reports for job: %w", err)
	}
	tag, err := s.db.PG.Exec(ctx, `DELETE FROM report_jobs WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete report job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("report job not found")
	}
	return nil
}

// DeleteReport removes a single agent report by ID.
func (s *Store) DeleteReport(ctx context.Context, id string) error {
	tag, err := s.db.PG.Exec(ctx, `DELETE FROM agent_reports WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete report: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("report not found")
	}
	return nil
}

func (s *Store) SubmitReport(ctx context.Context, jobID, agentID, title, content string, metadata json.RawMessage) (*Report, error) {
	r := &Report{
		Summary: Summary{
			ID:        uuid.New().String(),
			JobID:     jobID,
			AgentID:   agentID,
			Title:     title,
			CreatedAt: time.Now(),
		},
		Content:  content,
		Metadata: metadata,
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO agent_reports (id, job_id, agent_id, title, content, metadata, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.ID, r.JobID, nullStr(r.AgentID), r.Title, r.Content, nullJSON(r.Metadata), r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("submit report: %w", err)
	}
	return r, nil
}

func (s *Store) GetReport(ctx context.Context, id string) (*Report, error) {
	r := &Report{}
	var metadata []byte
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, job_id, COALESCE(agent_id,''), title, content, metadata, created_at
		 FROM agent_reports WHERE id=$1`, id,
	).Scan(&r.ID, &r.JobID, &r.AgentID, &r.Title, &r.Content, &metadata, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get report: %w", err)
	}
	if len(metadata) > 0 {
		r.Metadata = json.RawMessage(metadata)
	}
	return r, nil
}

// ListByJob returns summaries (no content) for a job, newest first.
func (s *Store) ListByJob(ctx context.Context, jobID string, limit, offset int) ([]*Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, job_id, COALESCE(agent_id,''), title, created_at
		 FROM agent_reports WHERE job_id=$1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		jobID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Summary
	for rows.Next() {
		r := &Summary{}
		if err := rows.Scan(&r.ID, &r.JobID, &r.AgentID, &r.Title, &r.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if list == nil {
		list = []*Summary{}
	}
	return list, rows.Err()
}

// ListAll returns summaries across all jobs, newest first.
func (s *Store) ListAll(ctx context.Context, limit, offset int) ([]*Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, job_id, COALESCE(agent_id,''), title, created_at
		 FROM agent_reports ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Summary
	for rows.Next() {
		r := &Summary{}
		if err := rows.Scan(&r.ID, &r.JobID, &r.AgentID, &r.Title, &r.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if list == nil {
		list = []*Summary{}
	}
	return list, rows.Err()
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(b json.RawMessage) interface{} {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
