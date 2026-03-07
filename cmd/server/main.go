package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"

	"github.com/claw-works/claw-hub/internal/agent"
	"github.com/claw-works/claw-hub/internal/hub"
	"github.com/claw-works/claw-hub/internal/task"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	agents *agent.Registry
	tasks  *task.Store
	hub    *hub.Hub
}

func main() {
	s := &Server{
		agents: agent.NewRegistry(),
		tasks:  task.NewStore(),
		hub:    hub.New(),
	}

	// Background: mark stale agents offline every 30s
	go func() {
		for range time.Tick(30 * time.Second) {
			s.agents.MarkOfflineStale(60 * time.Second)
		}
	}()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	// Agent routes
	r.Post("/api/v1/agents/register", s.registerAgent)
	r.Post("/api/v1/agents/{id}/heartbeat", s.agentHeartbeat)
	r.Get("/api/v1/agents", s.listAgents)

	// Task routes
	r.Post("/api/v1/tasks", s.createTask)
	r.Get("/api/v1/tasks", s.listTasks)
	r.Get("/api/v1/tasks/{id}", s.getTask)
	r.Patch("/api/v1/tasks/{id}/claim", s.claimTask)
	r.Patch("/api/v1/tasks/{id}/complete", s.completeTask)
	r.Patch("/api/v1/tasks/{id}/fail", s.failTask)

	// WebSocket
	r.Get("/ws", s.wsHandler)

	log.Println("claw-hub listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
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
	a := s.agents.Register(req.Name, req.Capabilities)
	jsonResp(w, http.StatusCreated, a)
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.agents.Heartbeat(id) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, s.agents.List())
}

// ─── Task Handlers ─────────────────────────────────────────────────────────

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title                string   `json:"title"`
		Description          string   `json:"description"`
		RequiredCapabilities []string `json:"required_capabilities"`
		Priority             int      `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t := s.tasks.Create(req.Title, req.Description, req.RequiredCapabilities, task.Priority(req.Priority))

	// Auto-assign to a capable online agent
	if len(req.RequiredCapabilities) > 0 {
		if capable := s.agents.FindCapable(req.RequiredCapabilities); len(capable) > 0 {
			picked := capable[0]
			s.tasks.Claim(t.ID, picked.ID)
			s.hub.Send(picked.ID, hub.Message{
				Type:    hub.MsgTypeTaskAssigned,
				Payload: t,
			})
		}
	}

	jsonResp(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	jsonResp(w, http.StatusOK, s.tasks.List(status))
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, ok := s.tasks.Get(id)
	if !ok {
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
	if !s.tasks.Claim(id, req.AgentID) {
		http.Error(w, "cannot claim task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(id)
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
	if !s.tasks.Complete(id, req.Result) {
		http.Error(w, "cannot complete task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(id)
	s.hub.Broadcast(hub.Message{Type: hub.MsgTypeBroadcast, Payload: map[string]interface{}{"event": "task.done", "task": t}})
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
	if !s.tasks.Fail(id, req.Error) {
		http.Error(w, "cannot fail task", http.StatusConflict)
		return
	}
	t, _ := s.tasks.Get(id)
	jsonResp(w, http.StatusOK, t)
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
