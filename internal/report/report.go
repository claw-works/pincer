package report

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/claw-works/pincer/internal/store"
	"github.com/google/uuid"
)

// DailyReport stores a project's daily summary.
type DailyReport struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Date      string    `json:"date"` // YYYY-MM-DD (Asia/Shanghai)
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// PGStore handles daily report persistence.
type PGStore struct {
	db *store.DB
}

func NewPGStore(db *store.DB) *PGStore {
	return &PGStore{db: db}
}

func (s *PGStore) Save(ctx context.Context, projectID, date, summary string) (*DailyReport, error) {
	r := &DailyReport{
		ID:        uuid.New().String(),
		ProjectID: projectID,
		Date:      date,
		Summary:   summary,
		CreatedAt: time.Now(),
	}
	_, err := s.db.PG.Exec(ctx,
		`INSERT INTO daily_reports (id, project_id, date, summary, created_at)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (project_id, date) DO UPDATE SET summary=EXCLUDED.summary, created_at=EXCLUDED.created_at`,
		r.ID, r.ProjectID, r.Date, r.Summary, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("save report: %w", err)
	}
	return r, nil
}

func (s *PGStore) ListByProject(ctx context.Context, projectID string, limit int) ([]*DailyReport, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 365 {
		limit = 365
	}
	rows, err := s.db.PG.Query(ctx,
		`SELECT id, project_id, date, summary, created_at
		 FROM daily_reports WHERE project_id=$1
		 ORDER BY date DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []*DailyReport
	for rows.Next() {
		r := &DailyReport{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Date, &r.Summary, &r.CreatedAt); err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

func (s *PGStore) GetLatest(ctx context.Context, projectID string) (*DailyReport, error) {
	r := &DailyReport{}
	err := s.db.PG.QueryRow(ctx,
		`SELECT id, project_id, date, summary, created_at
		 FROM daily_reports WHERE project_id=$1
		 ORDER BY date DESC LIMIT 1`, projectID,
	).Scan(&r.ID, &r.ProjectID, &r.Date, &r.Summary, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ScheduleDaily blocks and fires the daily report at 15:30 UTC (23:30 CST) every day.
func ScheduleDaily(ctx context.Context, fire func(ctx context.Context)) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 15, 30, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		wait := next.Sub(now)
		log.Printf("daily-report: next run in %s (at %s UTC)", wait.Round(time.Second), next.Format("2006-01-02 15:04"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			fire(ctx)
		}
	}
}

// ReportTask is a minimal task representation for report generation.
type ReportTask struct {
	Title           string
	Status          string
	AssignedAgentID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ReportProject holds project metadata for report generation.
type ReportProject struct {
	Name        string
	Description string
	Repo        string
	Overview    string
}

// FormatReport generates a rich markdown daily report for a project.
// agentMap maps agent_id → agent_name.
func FormatReport(proj ReportProject, date string, tasks []ReportTask, agentMap map[string]string, cst *time.Location) string {
	// Strip "[ProjectName] " prefix from task titles (redundant within a project report)
	prefix := "[" + proj.Name + "] "
	stripPrefix := func(title string) string {
		if strings.HasPrefix(title, prefix) {
			return title[len(prefix):]
		}
		return title
	}

	agentName := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := agentMap[id]; ok && n != "" {
			return n
		}
		// fallback: first 8 chars of id
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}

	isToday := func(t time.Time) bool {
		return t.In(cst).Format("2006-01-02") == date
	}

	var (
		doneToday    []ReportTask
		newToday     []ReportTask
		running      []ReportTask
		review       []ReportTask
		assigned     []ReportTask
		pending      []ReportTask
		failed       []ReportTask
		rejected     []ReportTask
		totalDone    int
	)

	for _, t := range tasks {
		switch t.Status {
		case "done":
			totalDone++
			if isToday(t.UpdatedAt) {
				doneToday = append(doneToday, t)
			}
		case "running":
			running = append(running, t)
		case "review":
			review = append(review, t)
		case "assigned":
			assigned = append(assigned, t)
		case "pending":
			pending = append(pending, t)
		case "failed":
			failed = append(failed, t)
		case "rejected":
			rejected = append(rejected, t)
		}
		if t.Status != "done" && isToday(t.CreatedAt) {
			newToday = append(newToday, t)
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 **%s 日报**（%s 北京时间）\n", proj.Name, date))

	if proj.Description != "" {
		sb.WriteString(fmt.Sprintf("\n> %s\n", proj.Description))
	}
	if proj.Repo != "" {
		sb.WriteString(fmt.Sprintf("> 仓库：%s\n", proj.Repo))
	}

	sb.WriteString("\n---\n")

	// 今日动态
	if len(doneToday) > 0 {
		sb.WriteString(fmt.Sprintf("\n✅ **今日完成**（%d 个）\n", len(doneToday)))
		for _, t := range doneToday {
			line := fmt.Sprintf("  • %s", stripPrefix(t.Title))
			if n := agentName(t.AssignedAgentID); n != "" {
				line += fmt.Sprintf("（@%s）", n)
			}
			sb.WriteString(line + "\n")
		}
	}

	if len(newToday) > 0 {
		sb.WriteString(fmt.Sprintf("\n📝 **今日新建**（%d 个）\n", len(newToday)))
		for _, t := range newToday {
			sb.WriteString(fmt.Sprintf("  • %s\n", stripPrefix(t.Title)))
		}
	}

	// 进行中
	if len(running) > 0 {
		sb.WriteString(fmt.Sprintf("\n🔄 **进行中**（%d 个）\n", len(running)))
		for _, t := range running {
			line := fmt.Sprintf("  • %s", stripPrefix(t.Title))
			if n := agentName(t.AssignedAgentID); n != "" {
				line += fmt.Sprintf("（@%s）", n)
			}
			sb.WriteString(line + "\n")
		}
	}

	// 待审核
	if len(review) > 0 {
		sb.WriteString(fmt.Sprintf("\n🔍 **待审核**（%d 个）\n", len(review)))
		for _, t := range review {
			line := fmt.Sprintf("  • %s", stripPrefix(t.Title))
			if n := agentName(t.AssignedAgentID); n != "" {
				line += fmt.Sprintf("（@%s）", n)
			}
			sb.WriteString(line + "\n")
		}
	}

	// 已分配待启动
	if len(assigned) > 0 {
		sb.WriteString(fmt.Sprintf("\n📌 **已分配待启动**（%d 个）\n", len(assigned)))
		for _, t := range assigned {
			line := fmt.Sprintf("  • %s", stripPrefix(t.Title))
			if n := agentName(t.AssignedAgentID); n != "" {
				line += fmt.Sprintf("（→ @%s）", n)
			}
			sb.WriteString(line + "\n")
		}
	}

	// 待处理
	if len(pending) > 0 {
		const maxShow = 5
		sb.WriteString(fmt.Sprintf("\n⏳ **待处理**（%d 个）\n", len(pending)))
		shown := pending
		if len(pending) > maxShow {
			shown = pending[:maxShow]
		}
		for _, t := range shown {
			sb.WriteString(fmt.Sprintf("  • %s\n", stripPrefix(t.Title)))
		}
		if len(pending) > maxShow {
			sb.WriteString(fmt.Sprintf("  …及其他 %d 个\n", len(pending)-maxShow))
		}
	}

	// 失败
	if len(failed) > 0 {
		sb.WriteString(fmt.Sprintf("\n❌ **失败**（%d 个）\n", len(failed)))
		for _, t := range failed {
			sb.WriteString(fmt.Sprintf("  • %s\n", stripPrefix(t.Title)))
		}
	}

	// 被驳回
	if len(rejected) > 0 {
		sb.WriteString(fmt.Sprintf("\n🔁 **被驳回待返工**（%d 个）\n", len(rejected)))
		for _, t := range rejected {
			sb.WriteString(fmt.Sprintf("  • %s\n", stripPrefix(t.Title)))
		}
	}

	// 总进度
	total := len(tasks)
	sb.WriteString(fmt.Sprintf("\n---\n总进度：%d / %d 完成", totalDone, total))
	if total > 0 {
		pct := totalDone * 100 / total
		sb.WriteString(fmt.Sprintf("（%d%%）", pct))
	}
	sb.WriteString("\n")

	return sb.String()
}
