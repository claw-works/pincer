package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"

	"github.com/claw-works/pincer/docs"
	"github.com/claw-works/pincer/internal/agent"
	"github.com/claw-works/pincer/internal/auth"
	"github.com/claw-works/pincer/internal/hub"
	"github.com/claw-works/pincer/internal/notify"
	"github.com/claw-works/pincer/internal/project"
	"github.com/claw-works/pincer/internal/report"
	"github.com/claw-works/pincer/internal/room"
	"github.com/claw-works/pincer/internal/store"
	"github.com/claw-works/pincer/internal/task"
	"github.com/claw-works/pincer/pkg/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	agents   *agent.PGRegistry
	tasks    *task.PGStore
	projects *project.PGStore
	reports  *report.PGStore
	hub      *hub.Hub
	monitor  *hub.MonitorHub
	roomHub  *hub.RoomHub
	rooms    *room.Store
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()

	pgDSN := getenv("PG_DSN", "postgres://clawhub:clawhub2026@10.0.1.24:5432/clawhub")
	mongoURI := getenv("MONGO_URI", "mongodb://clawhub:clawhub2026@10.0.1.24:27017/clawhub?authSource=admin")

	db, err := store.Connect(ctx, pgDSN, mongoURI, "clawhub")
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	h := hub.New()
	if db.Mongo != nil {
		h.SetInbox(hub.NewMongoInbox(db.Mongo))
	} else {
		log.Println("hub: MongoDB not available, inbox disabled (nopInbox)")
	}

	s := &Server{
		agents:   agent.NewPGRegistry(db),
		tasks:    task.NewPGStore(db),
		projects: project.NewPGStore(db),
		reports:  report.NewPGStore(db),
		hub:      h,
		monitor:  hub.NewMonitorHub(),
		roomHub:  hub.NewRoomHub(),
		rooms: func() *room.Store {
			if db.Mongo != nil {
				return room.NewStore(db.Mongo)
			}
			return nil
		}(),
	}

	// Start daily report scheduler (fires at 15:30 UTC = 23:30 CST)
	go report.ScheduleDaily(ctx, func(ctx context.Context) {
		s.generateDailyReports(ctx)
	})

	// Wire up WS REGISTER → update agent capabilities + last_seen in DB
	h.SetOnRegister(func(agentID string, p protocol.RegisterPayload) {
		if len(p.Capabilities) > 0 {
			_ = s.agents.UpdateCapabilities(context.Background(), agentID, p.Capabilities)
		}
		s.agents.Heartbeat(context.Background(), agentID)
		s.monitor.Broadcast("agent.online", map[string]interface{}{"agent_id": agentID})
	})
	// Wire up WS HEARTBEAT → update last_seen in DB
	h.SetOnHeartbeat(func(agentID string) {
		s.agents.Heartbeat(context.Background(), agentID)
		s.monitor.Broadcast("agent.heartbeat", map[string]interface{}{"agent_id": agentID})
	})
	// Wire up WS TASK_UPDATE → transition task state
	h.SetOnTaskUpdate(func(agentID string, p protocol.TaskUpdatePayload) {
		ctx := context.Background()
		s.tasks.LogEvent(ctx, p.TaskID, "task_update", p)
		switch p.Status {
		case "running":
			s.tasks.Start(ctx, p.TaskID)
			s.monitor.Broadcast("task.update", map[string]interface{}{"task_id": p.TaskID, "status": "running", "agent_id": agentID})
		case "done":
			if s.tasks.Complete(ctx, p.TaskID, p.Message) {
				s.agents.SetOnline(ctx, agentID)
				s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task_id": p.TaskID}})
				s.monitor.Broadcast("task.update", map[string]interface{}{"task_id": p.TaskID, "status": "done", "agent_id": agentID})
			}
		case "failed":
			if s.tasks.Fail(ctx, p.TaskID, p.Message) {
				s.agents.SetOnline(ctx, agentID)
				s.monitor.Broadcast("task.update", map[string]interface{}{"task_id": p.TaskID, "status": "failed", "agent_id": agentID})
			}
		}
	})
	// Wire up WS TASK_RESULT → store final result/error
	h.SetOnTaskResult(func(agentID string, p protocol.TaskResultPayload) {
		ctx := context.Background()
		switch p.Status {
		case "done":
			if s.tasks.Complete(ctx, p.TaskID, p.Result) {
				s.agents.SetOnline(ctx, agentID)
				t, _ := s.tasks.Get(ctx, p.TaskID)
				s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task": t}})
				s.monitor.Broadcast("task.result", map[string]interface{}{"task_id": p.TaskID, "status": "done", "agent_id": agentID, "task": t})
				if t != nil && t.ReportChannel != nil && t.ReportChannel.WebhookURL != "" {
					go func() {
						evt := notify.TaskEvent{
							Event:           "task.done",
							TaskID:          t.ID,
							TaskTitle:       t.Title,
							Status:          string(t.Status),
							Result:          t.Result,
							AssignedAgentID: t.AssignedAgentID,
							CompletedAt:     *t.CompletedAt,
							DiscordThreadID: t.ReportChannel.DiscordThreadID,
							FeishuChatID:    t.ReportChannel.FeishuChatID,
						}
						if err := notify.Send(ctx, t.ReportChannel.WebhookURL, evt); err != nil {
							log.Printf("taskResult: webhook error: %v", err)
						}
					}()
				}
			}
		case "failed":
			if s.tasks.Fail(ctx, p.TaskID, p.Error) {
				s.agents.SetOnline(ctx, agentID)
				t, _ := s.tasks.Get(ctx, p.TaskID)
				s.monitor.Broadcast("task.result", map[string]interface{}{"task_id": p.TaskID, "status": "failed", "agent_id": agentID, "task": t})
				if t != nil && t.ReportChannel != nil && t.ReportChannel.WebhookURL != "" {
					go func() {
						evt := notify.TaskEvent{
							Event:           "task.failed",
							TaskID:          t.ID,
							TaskTitle:       t.Title,
							Status:          string(t.Status),
							Error:           t.ErrorMsg,
							AssignedAgentID: t.AssignedAgentID,
							CompletedAt:     *t.CompletedAt,
							DiscordThreadID: t.ReportChannel.DiscordThreadID,
							FeishuChatID:    t.ReportChannel.FeishuChatID,
						}
						if err := notify.Send(ctx, t.ReportChannel.WebhookURL, evt); err != nil {
							log.Printf("taskResult: webhook error: %v", err)
						}
					}()
				}
			}
		}
	})

	// Background: mark stale agents offline every 60s
	go func() {
		for range time.Tick(60 * time.Second) {
			s.agents.MarkOfflineStale(ctx, 3*time.Minute)
		}
	}()

	// Background: reassign tasks stuck in 'running' > 5min (agent didn't ACK)
	go func() {
		for range time.Tick(30 * time.Second) {
			staleAgentIDs := s.tasks.ReassignStale(ctx, 5*time.Minute)
			for _, agentID := range staleAgentIDs {
				s.agents.SetOnline(ctx, agentID)
				log.Printf("reassign: agent %s freed (task ack timeout)", agentID)
			}
		}
	}()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	// Public routes (no auth) — bootstrap only
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "pincer"})
	})

	// Agent onboarding docs (no auth — meant to be shared with agents)
	r.Get("/agents.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(docs.AgentsMD)
	})
	r.Get("/agents.zh.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(docs.AgentsZhMD)
	})

	r.Get("/ws", s.wsHandler)
	r.Post("/api/v1/users", s.createUser) // bootstrap: create user and get first API key

	// All /api/v1/* routes require X-API-Key
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(auth.Middleware(s.projects))

		// Auth routes
		r.Post("/auth/reset-key", s.resetAPIKey)
		r.Post("/auth/register-human", s.registerHuman)

		// User routes
		r.Get("/users", s.listUsers)

		// Agent routes
		r.Post("/agents/register", s.registerAgent)
		r.Get("/agents", s.listAgents)
		r.Delete("/agents/{id}", s.deleteAgent)
		r.Post("/agents/{id}/heartbeat", s.agentHeartbeat)
		r.Get("/agents/{id}/inbox", s.getInbox)
		r.Get("/agents/{id}/messages", s.listAgentMessages)
		r.Get("/conversations", s.listConversation)

		// Project routes
		r.Post("/projects", s.createProject)
		r.Get("/projects", s.listProjects)
		r.Get("/projects/{id}", s.getProject)
		r.Patch("/projects/{id}", s.updateProject)
		r.Get("/projects/{id}/tasks", s.listProjectTasks)
		r.Get("/projects/{id}/reports", s.listProjectReports)

		// Task routes
		r.Post("/tasks", s.createTask)
		r.Get("/tasks", s.listTasks)
		r.Get("/tasks/recent", s.listRecentTasks)
		r.Get("/tasks/{id}", s.getTask)
		r.Get("/tasks/{id}/events", s.getTaskEvents)
		r.Patch("/tasks/{id}/claim", s.claimTask)
		r.Patch("/tasks/{id}/start", s.startTask)
		r.Patch("/tasks/{id}/complete", s.completeTask)
		r.Patch("/tasks/{id}/fail", s.failTask)
		r.Patch("/tasks/{id}/submit", s.submitTask)
		r.Patch("/tasks/{id}/approve", s.approveTask)
		r.Patch("/tasks/{id}/reject", s.rejectTask)
		r.Delete("/tasks/{id}", s.deleteTask)
		r.Post("/tasks/reassign", s.reassignPending)

		// Message routes
		r.Post("/messages/send", s.sendMessage)

		// Room routes
		r.Get("/rooms", s.listRooms)
		r.Post("/rooms/{room_id}/messages", s.postRoomMessage)
		r.Get("/rooms/{room_id}/messages", s.listRoomMessages)
		r.Get("/rooms/{room_id}/messages/search", s.searchRoomMessages)
		r.Get("/messages/search", s.searchDMMessages)
		// Room chat WebSocket — real-time push for group chat
		r.Get("/rooms/{room_id}/ws", s.roomChatWsHandler)

		// Human inbox WebSocket — real-time push for human-participated single chat.
		// Connect with: ws://<host>/api/v1/inbox/ws?api_key=<key>
		// Agents can reach the connected human by calling hub.Send(userID, msg).
		r.Get("/inbox/ws", s.inboxWsHandler)

		// Monitor WebSocket — real-time push events to browser dashboards
		r.Get("/ws", s.monitorWsHandler)
	})

	addr := getenv("ADDR", ":8080")
	log.Printf("pincer listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}

// ─── Agent Handlers ────────────────────────────────────────────────────────

func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Type         agent.AgentType `json:"type"`
		Capabilities []string        `json:"capabilities"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Capabilities == nil {
		req.Capabilities = []string{}
	}
	// Human registration: upsert by name to prevent duplicate records.
	// Bind current session's API key to the named human identity.
	if req.Type == agent.TypeHuman && req.Name != "" {
		user := auth.FromContext(r.Context())
		if user == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		u, err := s.projects.UpsertHumanByName(r.Context(), user.ID, req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Find or create a human agent linked to this user and return it.
		// We return an agent (not a user) so the frontend never stores user.ID.
		a, err := s.agents.FindOrCreateHumanAgent(r.Context(), u.ID, u.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResp(w, http.StatusOK, a)
		return
	}
	a, err := s.agents.Register(r.Context(), req.ID, req.Name, req.Capabilities, req.Type)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 200 OK for upsert of existing agent, 201 Created for new registration.
	status := http.StatusCreated
	if req.ID != "" {
		status = http.StatusOK
	}
	jsonResp(w, status, a)
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.agents.Heartbeat(r.Context(), id) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	// deliver pending inbox messages via HTTP polling
	msgs := s.hub.PopInboxHTTP(id)
	jsonResp(w, http.StatusOK, map[string]interface{}{"status": "ok", "inbox": msgs})
}

func (s *Server) getInbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	msgs := s.hub.PopInboxHTTP(id)
	jsonResp(w, http.StatusOK, msgs)
}

func (s *Server) listAgentMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	from := r.URL.Query().Get("from")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	msgs := s.hub.ListAgentMessages(id, from, limit)
	if msgs == nil {
		msgs = []hub.InboxMessage{}
	}
	jsonResp(w, http.StatusOK, msgs)
}

func (s *Server) listConversation(w http.ResponseWriter, r *http.Request) {
	a := r.URL.Query().Get("a")
	b := r.URL.Query().Get("b")
	if a == "" || b == "" {
		http.Error(w, `{"error":"params a and b required"}`, http.StatusBadRequest)
		return
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	msgs := s.hub.ListConversation(a, b, limit)
	if msgs == nil {
		msgs = []hub.InboxMessage{}
	}
	jsonResp(w, http.StatusOK, msgs)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.agents.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, agents)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.agents.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Task Handlers ─────────────────────────────────────────────────────────

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title                string              `json:"title"`
		Description          string              `json:"description"`
		Guidance             string              `json:"guidance"`
		AcceptanceCriteria   string              `json:"acceptance_criteria"`
		RequiredCapabilities []string            `json:"required_capabilities"`
		Priority             int                 `json:"priority"`
		ReportChannel        *task.ReportChannel `json:"report_channel"`
		ProjectID            string              `json:"project_id"`
		AssignedTo           string              `json:"assigned_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := s.tasks.Create(r.Context(), req.Title, req.Description, req.Guidance, req.AcceptanceCriteria, req.RequiredCapabilities, task.Priority(req.Priority), req.ReportChannel, req.ProjectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Use explicit assigned_to if provided, otherwise find capable agent
	assignAgentID := req.AssignedTo
	if assignAgentID == "" && len(req.RequiredCapabilities) > 0 {
		agentID, err := s.agents.FindCapableAtomic(r.Context(), req.RequiredCapabilities)
		if err == nil {
			assignAgentID = agentID
		}
	}

	if assignAgentID != "" {
		if s.tasks.Claim(r.Context(), t.ID, assignAgentID) {
			t, _ = s.tasks.Get(r.Context(), t.ID)
			var rc *protocol.ReportChannel
			if t.ReportChannel != nil {
				rc = &protocol.ReportChannel{
					Type:      "webhook",
					ChannelID: t.ReportChannel.WebhookURL,
				}
				if t.ReportChannel.DiscordThreadID != "" {
					rc.Type = "discord_thread"
					rc.ChannelID = t.ReportChannel.DiscordThreadID
				}
			}
			s.hub.Send(assignAgentID, hub.Message{
				Type: hub.MsgTypeTaskAssigned,
				Payload: protocol.TaskAssignPayload{
					TaskID:             t.ID,
					Title:              t.Title,
					Description:        t.Description,
					Guidance:           t.Guidance,
					AcceptanceCriteria: t.AcceptanceCriteria,
					Requirements:       t.RequiredCapabilities,
					Priority:           int(t.Priority),
					ReportChannel:      rc,
				},
			})
			log.Printf("createTask: assigned %s → agent %s", t.ID, assignAgentID)
		} else {
			s.agents.SetOnline(r.Context(), assignAgentID)
		}
	}

	jsonResp(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	f := task.ListFilter{
		Status:     r.URL.Query().Get("status"),
		AssignedTo: r.URL.Query().Get("assigned_to"),
		ProjectID:  r.URL.Query().Get("project_id"),
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &f.Limit)
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		fmt.Sscanf(o, "%d", &f.Offset)
	}
	tasks, err := s.tasks.ListFiltered(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, tasks)
}

func (s *Server) listRecentTasks(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if n := r.URL.Query().Get("limit"); n != "" {
		fmt.Sscanf(n, "%d", &limit)
	}
	tasks, err := s.tasks.ListRecent(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, tasks)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := s.tasks.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) claimTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.tasks.Claim(r.Context(), id, req.AgentID) {
		http.Error(w, "cannot claim task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.tasks.Start(r.Context(), id) {
		http.Error(w, "cannot start task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) completeTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.tasks.Complete(r.Context(), id, req.Result) {
		http.Error(w, "cannot complete task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	// Release agent back to online
	if t != nil && t.AssignedAgentID != "" {
		s.agents.SetOnline(r.Context(), t.AssignedAgentID)
	}
	s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task": t}})
	s.monitor.Broadcast("task.result", map[string]interface{}{"task_id": id, "status": "done", "task": t})
	if t != nil && t.ReportChannel != nil && t.ReportChannel.WebhookURL != "" {
		go func() {
			evt := notify.TaskEvent{
				Event:           "task.done",
				TaskID:          t.ID,
				TaskTitle:       t.Title,
				Status:          string(t.Status),
				Result:          t.Result,
				AssignedAgentID: t.AssignedAgentID,
				CompletedAt:     *t.CompletedAt,
				DiscordThreadID: t.ReportChannel.DiscordThreadID,
				FeishuChatID:    t.ReportChannel.FeishuChatID,
			}
			if err := notify.Send(context.Background(), t.ReportChannel.WebhookURL, evt); err != nil {
				log.Printf("completeTask: webhook error: %v", err)
			}
		}()
	}

	jsonResp(w, http.StatusOK, t)
}

func (s *Server) failTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.tasks.Fail(r.Context(), id, req.Error) {
		http.Error(w, "cannot fail task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	// Release agent back to online
	if t != nil && t.AssignedAgentID != "" {
		s.agents.SetOnline(r.Context(), t.AssignedAgentID)
	}
	s.monitor.Broadcast("task.result", map[string]interface{}{"task_id": id, "status": "failed", "task": t})

	// Fire outgoing webhook if report_channel is configured
	if t != nil && t.ReportChannel != nil && t.ReportChannel.WebhookURL != "" {
		go func() {
			evt := notify.TaskEvent{
				Event:           "task.failed",
				TaskID:          t.ID,
				TaskTitle:       t.Title,
				Status:          string(t.Status),
				Error:           t.ErrorMsg,
				AssignedAgentID: t.AssignedAgentID,
				CompletedAt:     *t.CompletedAt,
				DiscordThreadID: t.ReportChannel.DiscordThreadID,
				FeishuChatID:    t.ReportChannel.FeishuChatID,
			}
			if err := notify.Send(context.Background(), t.ReportChannel.WebhookURL, evt); err != nil {
				log.Printf("failTask: webhook error: %v", err)
			}
		}()
	}

	jsonResp(w, http.StatusOK, t)
}

func (s *Server) submitTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.tasks.Submit(r.Context(), id, req.Result) {
		http.Error(w, "cannot submit task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	s.monitor.Broadcast("task.update", map[string]interface{}{"task_id": id, "status": "review", "task": t})
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) approveTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.tasks.Approve(r.Context(), id) {
		http.Error(w, "cannot approve task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	if t != nil && t.AssignedAgentID != "" {
		s.agents.SetOnline(r.Context(), t.AssignedAgentID)
	}
	s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task": t}})
	s.monitor.Broadcast("task.result", map[string]interface{}{"task_id": id, "status": "done", "task": t})
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) rejectTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.tasks.Reject(r.Context(), id, req.Reason) {
		http.Error(w, "cannot reject task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(r.Context(), id)
	s.monitor.Broadcast("task.update", map[string]interface{}{"task_id": id, "status": "rejected", "task": t})
	jsonResp(w, http.StatusOK, t)
}

func (s *Server) reassignPending(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.tasks.List(r.Context(), "pending")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	assigned := 0
	for _, t := range tasks {
		if len(t.RequiredCapabilities) == 0 {
			continue
		}
		agentID, err := s.agents.FindCapableAtomic(r.Context(), t.RequiredCapabilities)
		if err != nil || agentID == "" {
			continue
		}
		if s.tasks.Claim(r.Context(), t.ID, agentID) {
			updated, _ := s.tasks.Get(r.Context(), t.ID)
			s.hub.Send(agentID, hub.Message{
				Type: hub.MsgTypeTaskAssigned,
				Payload: protocol.TaskAssignPayload{
					TaskID:             t.ID,
					Title:              t.Title,
					Description:        t.Description,
					Guidance:           t.Guidance,
					AcceptanceCriteria: t.AcceptanceCriteria,
					Requirements:       t.RequiredCapabilities,
					Priority:           int(t.Priority),
				},
			})
			_ = updated
			assigned++
		} else {
			s.agents.SetOnline(r.Context(), agentID)
		}
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"reassigned": assigned})
}

func (s *Server) getTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	events, err := s.tasks.GetEvents(r.Context(), id)
	if err != nil {
		http.Error(w, "failed to fetch events", http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, events)
}

// ─── Message Handler ──────────────────────────────────────────────────────

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromAgentID    string                 `json:"from_agent_id"`
		ToAgentID      string                 `json:"to_agent_id"`
		Type           string                 `json:"type"`
		ConversationID string                 `json:"conversation_id"`
		Payload        map[string]interface{} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToAgentID == "" {
		http.Error(w, "to_agent_id required", http.StatusBadRequest)
		return
	}
	msgType := hub.MessageType(req.Type)
	if msgType == "" {
		msgType = hub.MsgTypeAgentMessage
	}
	s.hub.Send(req.ToAgentID, hub.Message{
		Type:           msgType,
		From:           req.FromAgentID,
		To:             req.ToAgentID,
		ConversationID: req.ConversationID,
		Payload:        req.Payload,
	})
	jsonResp(w, http.StatusOK, map[string]string{"status": "sent"})
}

// ─── WebSocket ─────────────────────────────────────────────────────────────

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	if agentID == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := s.hub.Register(agentID, conn)
	client.ReadPump(s.hub, func(from string, msg hub.Message) {
		if msg.To != "" {
			s.hub.Send(msg.To, msg)
		} else {
			s.hub.Broadcast(msg)
		}
	})
}

// monitorWsHandler upgrades authenticated browser/monitor clients to WebSocket
// and streams real-time events (room.message, task.update, agent.heartbeat, etc.).
// Auth: X-API-Key (same as REST API) OR ?api_key=<key> query param for browser clients.
func (s *Server) monitorWsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := s.monitor.Subscribe(conn)
	log.Printf("monitor: client %s connected", client.GetID())
	s.monitor.ReadLoop(client)
	log.Printf("monitor: client %s disconnected", client.GetID())
}

// roomChatWsHandler upgrades the connection to WebSocket and subscribes the
// client to real-time room.message events for the given room_id.
//
// Only the authenticated user's own default room is accessible.
// New messages pushed via postRoomMessage are delivered immediately to all
// connected subscribers without polling.
//
// Usage: ws://<host>/api/v1/rooms/{room_id}/ws?api_key=<key>
func (s *Server) roomChatWsHandler(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	roomID := chi.URLParam(r, "room_id")
	// Enforce room ownership: only the user's own room is permitted.
	if roomID != userRoomID(user) {
		http.Error(w, `{"error":"room not found"}`, http.StatusNotFound)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := s.roomHub.Subscribe(roomID, conn)
	log.Printf("roomws: client %s subscribed to room %s (%d subs)", client.GetID(), roomID, s.roomHub.CountSubscribers(roomID))
	s.roomHub.ReadLoop(client)
	log.Printf("roomws: client %s disconnected from room %s", client.GetID(), roomID)
}

// inboxWsHandler upgrades the connection to WebSocket for human-participated
// single chat. The connected user is registered in the hub under their user ID,
// so any agent that calls hub.Send(userID, msg) delivers the message in real-time.
//
// Pending offline inbox messages are flushed on connect.
//
// Usage: ws://<host>/api/v1/inbox/ws?api_key=<key>
func (s *Server) inboxWsHandler(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := s.hub.RegisterHuman(user.ID, conn)
	log.Printf("inboxws: user %s connected", user.ID)
	s.hub.ReadLoopHuman(client)
	log.Printf("inboxws: user %s disconnected", user.ID)
}

// ─── User Handlers ─────────────────────────────────────────────────────────

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	u, err := s.projects.CreateUser(r.Context(), req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusCreated, u)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.projects.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if users == nil {
		users = []*project.User{}
	}
	jsonResp(w, http.StatusOK, users)
}

func (s *Server) resetAPIKey(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	newKey, err := s.projects.ResetAPIKey(r.Context(), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"api_key": newKey})
}

func (s *Server) registerHuman(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	u, err := s.projects.UpsertHumanByName(r.Context(), user.ID, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Return agent object (not user) so frontend never stores user.ID.
	a, err := s.agents.FindOrCreateHumanAgent(r.Context(), u.ID, u.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, a)
}

// ─── Project Handlers ───────────────────────────────────────────────────────

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.tasks.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		Name        string `json:"name"`
		Repo        string `json:"repo"`
		Description string `json:"description"`
		Overview    string `json:"overview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	p, err := s.projects.CreateProject(r.Context(), user.ID, req.Name, req.Repo, req.Description, req.Overview)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusCreated, p)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	projects, err := s.projects.ListProjects(r.Context(), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = []*project.Project{}
	}
	jsonResp(w, http.StatusOK, projects)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := s.projects.GetProject(r.Context(), id)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, p)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Fetch existing first
	existing, err := s.projects.GetProject(r.Context(), id)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	var req struct {
		Repo        *string `json:"repo"`
		Description *string `json:"description"`
		Overview    *string `json:"overview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := existing.Repo
	description := existing.Description
	overview := existing.Overview
	if req.Repo != nil {
		repo = *req.Repo
	}
	if req.Description != nil {
		description = *req.Description
	}
	if req.Overview != nil {
		overview = *req.Overview
	}
	p, err := s.projects.UpdateProject(r.Context(), id, repo, description, overview)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, p)
}

func (s *Server) listProjectTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	f := task.ListFilter{
		Status:     r.URL.Query().Get("status"),
		AssignedTo: r.URL.Query().Get("assigned_to"),
		ProjectID:  projectID,
	}
	tasks, err := s.tasks.ListFiltered(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, tasks)
}

// ─── Room Handlers ─────────────────────────────────────────────────────────

// listRooms returns the default room for the authenticated user.
func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roomID := userRoomID(user)
	jsonResp(w, http.StatusOK, []map[string]string{
		{"id": roomID, "name": "default"},
	})
}

// postRoomMessage posts a message to a room.
func (s *Server) postRoomMessage(w http.ResponseWriter, r *http.Request) {
	if s.rooms == nil {
		http.Error(w, "rooms unavailable: MongoDB not configured", http.StatusServiceUnavailable)
		return
	}
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roomID := chi.URLParam(r, "room_id")

	// Validate that the room belongs to the authenticated user
	expectedRoomID := userRoomID(user)
	if roomID != expectedRoomID {
		http.Error(w, `{"error":"room not found"}`, http.StatusNotFound)
		return
	}

	var req struct {
		SenderAgentID string                 `json:"sender_agent_id"`
		Content       string                 `json:"content"`
		Metadata      map[string]interface{} `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}
	if req.SenderAgentID == "" {
		http.Error(w, `{"error":"sender_agent_id required"}`, http.StatusBadRequest)
		return
	}

	msg, err := s.rooms.Post(r.Context(), roomID, req.SenderAgentID, req.Content, req.Metadata)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Push real-time event to monitor WebSocket clients (dashboard)
	s.monitor.Broadcast("room.message", msg)
	// Push real-time event to room-chat WebSocket subscribers
	s.roomHub.BroadcastToRoom(roomID, "room.message", msg)
	jsonResp(w, http.StatusCreated, msg)
}

// listRoomMessages returns recent messages from a room.
func (s *Server) listRoomMessages(w http.ResponseWriter, r *http.Request) {
	if s.rooms == nil {
		http.Error(w, "rooms unavailable: MongoDB not configured", http.StatusServiceUnavailable)
		return
	}
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roomID := chi.URLParam(r, "room_id")

	// Validate that the room belongs to the authenticated user
	expectedRoomID := userRoomID(user)
	if roomID != expectedRoomID {
		http.Error(w, `{"error":"room not found"}`, http.StatusNotFound)
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	beforeID := r.URL.Query().Get("before")

	msgs, err := s.rooms.List(r.Context(), roomID, limit, beforeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []*room.Message{}
	}
	jsonResp(w, http.StatusOK, msgs)
}

// searchRoomMessages searches room messages by keyword.
func (s *Server) searchRoomMessages(w http.ResponseWriter, r *http.Request) {
	if s.rooms == nil {
		http.Error(w, "rooms unavailable", http.StatusServiceUnavailable)
		return
	}
	user := auth.FromContext(r.Context())
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roomID := chi.URLParam(r, "room_id")
	if roomID != userRoomID(user) {
		http.Error(w, `{"error":"room not found"}`, http.StatusNotFound)
		return
	}
	q := r.URL.Query().Get("q")
	limit, offset := 20, 0
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)

	msgs, total, err := s.rooms.Search(r.Context(), roomID, q, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []*room.Message{}
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"items": msgs, "total": total})
}

// searchDMMessages searches DM messages by keyword between two agents.
func (s *Server) searchDMMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	agentA := r.URL.Query().Get("agent_a")
	agentB := r.URL.Query().Get("agent_b")
	limit, offset := 20, 0
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)

	msgs, total := s.hub.SearchDM(agentA, agentB, q, limit, offset)
	if msgs == nil {
		msgs = []hub.InboxMessage{}
	}
	jsonResp(w, http.StatusOK, map[string]interface{}{"items": msgs, "total": total})
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// userRoomID returns the user's opaque room ID.
// Falls back to the legacy format for existing users pending migration.
func userRoomID(u *project.User) string {
	if u.RoomID != "" {
		return u.RoomID
	}
	return room.DefaultRoomID(u.ID)
}

func jsonResp(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Report Handlers ────────────────────────────────────────────────────────

func (s *Server) listProjectReports(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	limit := 30
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	reports, err := s.reports.ListByProject(r.Context(), projectID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if reports == nil {
		reports = []*report.DailyReport{}
	}
	jsonResp(w, http.StatusOK, reports)
}

// generateDailyReports runs at 15:30 UTC and posts room summaries for all projects.
func (s *Server) generateDailyReports(ctx context.Context) {
	if s.rooms == nil {
		log.Println("daily-report: rooms unavailable, skipping")
		return
	}

	cst := time.FixedZone("CST", 8*3600)
	date := time.Now().In(cst).Format("2006-01-02")

	projects, err := s.projects.ListProjects(ctx, "")
	if err != nil {
		log.Printf("daily-report: list projects error: %v", err)
		return
	}

	// Build agent id → name map
	agentMap := map[string]string{}
	if agents, err := s.agents.List(ctx); err == nil {
		for _, a := range agents {
			agentMap[a.ID] = a.Name
		}
	}

	for _, p := range projects {
		tasks, err := s.tasks.ListFiltered(ctx, task.ListFilter{ProjectID: p.ID})
		if err != nil {
			log.Printf("daily-report: list tasks for %s error: %v", p.ID, err)
			continue
		}

		// Only generate report if there was activity today (CST)
		hasActivity := false
		for _, t := range tasks {
			if t.UpdatedAt.In(cst).Format("2006-01-02") == date || t.CreatedAt.In(cst).Format("2006-01-02") == date {
				hasActivity = true
				break
			}
		}
		if !hasActivity {
			log.Printf("daily-report: no activity today for project %s, skipping", p.Name)
			continue
		}

		// Convert tasks to report format
		reportTasks := make([]report.ReportTask, len(tasks))
		for i, t := range tasks {
			reportTasks[i] = report.ReportTask{
				Title:           t.Title,
				Status:          string(t.Status),
				AssignedAgentID: t.AssignedAgentID,
				CreatedAt:       t.CreatedAt,
				UpdatedAt:       t.UpdatedAt,
			}
		}

		proj := report.ReportProject{
			Name:        p.Name,
			Description: p.Description,
			Repo:        p.Repo,
			Overview:    p.Overview,
		}

		summary := report.FormatReport(proj, date, reportTasks, agentMap, cst)

		// Save to DB
		if _, err := s.reports.Save(ctx, p.ID, date, summary); err != nil {
			log.Printf("daily-report: save error for %s: %v", p.ID, err)
		}

		// Post to room (use user_id from project)
		roomID := "user:" + p.UserID + ":default"
		if _, err := s.rooms.Post(ctx, roomID, "hub", summary, nil); err != nil {
			log.Printf("daily-report: post room error for %s: %v", p.ID, err)
		} else {
			s.monitor.Broadcast("room.message", map[string]interface{}{"room_id": roomID, "content": summary})
			s.roomHub.BroadcastToRoom(roomID, "room.message", map[string]interface{}{"room_id": roomID, "content": summary})
			log.Printf("daily-report: posted for project %s (%s)", p.Name, date)
		}
	}
}
