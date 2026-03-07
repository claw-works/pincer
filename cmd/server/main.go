package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"

	"github.com/claw-works/claw-hub/internal/agent"
	"github.com/claw-works/claw-hub/internal/hub"
	"github.com/claw-works/claw-hub/internal/notify"
	"github.com/claw-works/claw-hub/internal/store"
	"github.com/claw-works/claw-hub/internal/task"
	"github.com/claw-works/claw-hub/pkg/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	agents *agent.PGRegistry
	tasks  *task.PGStore
	hub    *hub.Hub
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
	h.SetInbox(hub.NewMongoInbox(db.Mongo))

	s := &Server{
		agents: agent.NewPGRegistry(db),
		tasks:  task.NewPGStore(db),
		hub:    h,
	}

	// Wire up WS REGISTER → update agent capabilities + last_seen in DB
	h.SetOnRegister(func(agentID string, p protocol.RegisterPayload) {
		if len(p.Capabilities) > 0 {
			_ = s.agents.UpdateCapabilities(context.Background(), agentID, p.Capabilities)
		}
		s.agents.Heartbeat(context.Background(), agentID)
	})
	// Wire up WS HEARTBEAT → update last_seen in DB
	h.SetOnHeartbeat(func(agentID string) {
		s.agents.Heartbeat(context.Background(), agentID)
	})
	// Wire up WS TASK_UPDATE → transition task state
	h.SetOnTaskUpdate(func(agentID string, p protocol.TaskUpdatePayload) {
		ctx := context.Background()
		s.tasks.LogEvent(ctx, p.TaskID, "task_update", p)
		switch p.Status {
		case "running":
			s.tasks.Start(ctx, p.TaskID)
		case "done":
			if s.tasks.Complete(ctx, p.TaskID, p.Message) {
				s.agents.SetOnline(ctx, agentID)
				s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task_id": p.TaskID}})
			}
		case "failed":
			if s.tasks.Fail(ctx, p.TaskID, p.Message) {
				s.agents.SetOnline(ctx, agentID)
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

	// Background: mark stale agents offline every 30s
	go func() {
		for range time.Tick(30 * time.Second) {
			s.agents.MarkOfflineStale(ctx, 60*time.Second)
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

	// Health
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "claw-hub"})
	})

	// Agent routes
	r.Post("/api/v1/agents/register", s.registerAgent)
	r.Post("/api/v1/agents/{id}/heartbeat", s.agentHeartbeat)
	r.Get("/api/v1/agents", s.listAgents)
	r.Get("/api/v1/agents/{id}/inbox", s.getInbox)

	// Task routes
	r.Post("/api/v1/tasks", s.createTask)
	r.Get("/api/v1/tasks", s.listTasks)
	r.Get("/api/v1/tasks/recent", s.listRecentTasks)
	r.Get("/api/v1/tasks/{id}", s.getTask)
	r.Get("/api/v1/tasks/{id}/events", s.getTaskEvents)
	r.Patch("/api/v1/tasks/{id}/claim", s.claimTask)
	r.Patch("/api/v1/tasks/{id}/complete", s.completeTask)
	r.Patch("/api/v1/tasks/{id}/fail", s.failTask)
	r.Post("/api/v1/tasks/reassign", s.reassignPending)

	// Message routes
	r.Post("/api/v1/messages/send", s.sendMessage)

	// WebSocket
	r.Get("/ws", s.wsHandler)

	addr := getenv("ADDR", ":8080")
	log.Printf("claw-hub listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r))
}

// ─── Agent Handlers ────────────────────────────────────────────────────────

func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.agents.Register(r.Context(), req.Name, req.Capabilities)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusCreated, a)
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

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.agents.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, http.StatusOK, agents)
}

// ─── Task Handlers ─────────────────────────────────────────────────────────

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title                string              `json:"title"`
		Description          string              `json:"description"`
		RequiredCapabilities []string            `json:"required_capabilities"`
		Priority             int                 `json:"priority"`
		ReportChannel        *task.ReportChannel `json:"report_channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := s.tasks.Create(r.Context(), req.Title, req.Description, req.RequiredCapabilities, task.Priority(req.Priority), req.ReportChannel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Atomically find a capable online agent and mark it busy
	if len(req.RequiredCapabilities) > 0 {
		agentID, err := s.agents.FindCapableAtomic(r.Context(), req.RequiredCapabilities)
		if err == nil && agentID != "" {
			if s.tasks.Claim(r.Context(), t.ID, agentID) {
				t, _ = s.tasks.Get(r.Context(), t.ID)
				// Send TaskAssignPayload via protocol.Envelope
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
				s.hub.Send(agentID, hub.Message{
					Type: hub.MsgTypeTaskAssigned,
					Payload: protocol.TaskAssignPayload{
						TaskID:        t.ID,
						Title:         t.Title,
						Description:   t.Description,
						Requirements:  t.RequiredCapabilities,
						Priority:      int(t.Priority),
						Deadline:      (*time.Time)(nil), // set via assigned_at + 5min
						ReportChannel: rc,
					},
				})
				log.Printf("createTask: assigned %s → agent %s", t.ID, agentID)
			} else {
				// Claim failed (race), release agent back to online
				s.agents.SetOnline(r.Context(), agentID)
			}
		}
	}

	jsonResp(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	tasks, err := s.tasks.List(r.Context(), status)
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

	// Fire outgoing webhook if report_channel is configured
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
					TaskID:       t.ID,
					Title:        t.Title,
					Description:  t.Description,
					Requirements: t.RequiredCapabilities,
					Priority:     int(t.Priority),
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

// ─── Helpers ───────────────────────────────────────────────────────────────

func jsonResp(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
