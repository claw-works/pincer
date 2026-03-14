package hub

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/claw-works/pincer/pkg/protocol"
)

type MessageType string

const (
	MsgTypeTaskAssigned  MessageType = "task.assigned"
	MsgTypeAgentMessage  MessageType = "agent.message"
	MsgTypeBroadcast     MessageType = "broadcast"
	MsgTypeInboxDelivery MessageType = "inbox.delivery"
)

type Message struct {
	ID             string      `json:"id"`
	Type           MessageType `json:"type"`
	From           string      `json:"from,omitempty"`
	To             string      `json:"to,omitempty"`
	ConversationID string      `json:"conversation_id,omitempty"`
	Depth          int         `json:"depth,omitempty"`
	Payload        interface{} `json:"payload"`
	Timestamp      time.Time   `json:"timestamp"`
}

type Client struct {
	AgentID       string
	MessagingMode string // "ws" | "poll"
	conn          *websocket.Conn
	send          chan []byte
}

type inboxBackend interface {
	SaveOffline(agentID string, msg Message)
	SaveDM(agentID string, msg Message)
	PopOffline(agentID string) []Message
	ListMessages(agentID string, fromAgentID string, limit int) []InboxMessage
	ListConversation(agentA, agentB string, limit int) []InboxMessage
	SearchDM(agentA, agentB, keyword string, limit, offset int) ([]InboxMessage, int64)
}

type nopInbox struct{}

func (nopInbox) SaveOffline(_ string, _ Message)                               {}
func (nopInbox) SaveDM(_ string, _ Message)                                    {}
func (nopInbox) PopOffline(_ string) []Message                                 { return nil }
func (nopInbox) ListMessages(_ string, _ string, _ int) []InboxMessage         { return nil }
func (nopInbox) ListConversation(_ string, _ string, _ int) []InboxMessage     { return nil }
func (nopInbox) SearchDM(_, _, _ string, _, _ int) ([]InboxMessage, int64)     { return nil, 0 }

// OnRegisterFunc is called when a REGISTER message arrives over WS.
// The hub calls this so main.go can update the agent DB record.
type OnRegisterFunc func(agentID string, p protocol.RegisterPayload)

// OnHeartbeatFunc is called on each WS HEARTBEAT to update last_seen.
type OnHeartbeatFunc func(agentID string)

// OnTaskUpdateFunc is called when an agent sends a TASK_UPDATE message.
type OnTaskUpdateFunc func(agentID string, p protocol.TaskUpdatePayload)

// OnTaskResultFunc is called when an agent sends a TASK_RESULT message.
type OnTaskResultFunc func(agentID string, p protocol.TaskResultPayload)

type Hub struct {
	mu           sync.RWMutex
	clients      map[string]*Client
	inbox        inboxBackend
	pubsub       PubSub
	ctx          context.Context
	cancel       context.CancelFunc
	onRegister   OnRegisterFunc
	onHeartbeat  OnHeartbeatFunc
	onTaskUpdate OnTaskUpdateFunc
	onTaskResult OnTaskResultFunc
}

func New() *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		clients: make(map[string]*Client),
		inbox:   nopInbox{},
		pubsub:  nopPubSub{},
		ctx:     ctx,
		cancel:  cancel,
	}
}

// SetPubSub configures the cross-instance pub/sub backend.
// Call before any agents connect.
func (h *Hub) SetPubSub(ps PubSub) {
	h.pubsub = ps
}

// Close shuts down the hub and pub/sub connections.
func (h *Hub) Close() {
	h.cancel()
	h.pubsub.Close()
}

func (h *Hub) SetInbox(ib inboxBackend)            { h.inbox = ib }
func (h *Hub) SetOnRegister(f OnRegisterFunc)      { h.onRegister = f }
func (h *Hub) SetOnHeartbeat(f OnHeartbeatFunc)    { h.onHeartbeat = f }
func (h *Hub) SetOnTaskUpdate(f OnTaskUpdateFunc)  { h.onTaskUpdate = f }
func (h *Hub) SetOnTaskResult(f OnTaskResultFunc)  { h.onTaskResult = f }

// IsOnlineWS reports whether the agent has an active WS connection.
func (h *Hub) IsOnlineWS(agentID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[agentID]
	return ok
}

// PopInboxHTTP is for HTTP-polling agents.
// If the agent is connected via WS, we do NOT pop inbox here — messages will
// be delivered over the WS HEARTBEAT ACK instead (fixes WS/HTTP mix bug).
func (h *Hub) PopInboxHTTP(agentID string) []Message {
	if h.IsOnlineWS(agentID) {
		// Agent is on WS; don't drain inbox via HTTP to avoid message loss.
		return nil
	}
	return h.inbox.PopOffline(agentID)
}

// ListAgentMessages returns inbox history for monitor/observability use (read-only, no pop).
func (h *Hub) ListAgentMessages(agentID string, fromAgentID string, limit int) []InboxMessage {
	return h.inbox.ListMessages(agentID, fromAgentID, limit)
}

// ListConversation returns bidirectional message history between two agents.
func (h *Hub) ListConversation(agentA, agentB string, limit int) []InboxMessage {
	return h.inbox.ListConversation(agentA, agentB, limit)
}

// SearchDM searches DM messages between two agents by keyword.
func (h *Hub) SearchDM(agentA, agentB, keyword string, limit, offset int) ([]InboxMessage, int64) {
	return h.inbox.SearchDM(agentA, agentB, keyword, limit, offset)
}

func (h *Hub) Register(agentID string, conn *websocket.Conn) *Client {
	c := &Client{
		AgentID:       agentID,
		MessagingMode: "poll", // default until REGISTER sets it to "ws"
		conn:          conn,
		send:          make(chan []byte, 64),
	}
	h.mu.Lock()
	h.clients[agentID] = c
	h.mu.Unlock()
	go c.writePump()

	// Subscribe to Redis channel for cross-instance delivery.
	_ = h.pubsub.Subscribe(h.ctx, func(msg Message) {
		h.deliverLocal(agentID, msg)
	}, agentID)

	// Deliver any pending offline messages immediately on connect.
	go h.deliverInbox(agentID, c)
	return c
}

func (h *Hub) deliverInbox(agentID string, c *Client) {
	msgs := h.inbox.PopOffline(agentID)
	for _, msg := range msgs {
		msg.Type = MsgTypeInboxDelivery
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		select {
		case c.send <- data:
		default:
		}
	}
}

func (h *Hub) Unregister(agentID string) {
	h.mu.Lock()
	delete(h.clients, agentID)
	h.mu.Unlock()
	_ = h.pubsub.Unsubscribe(h.ctx, agentID)
}

// RegisterHuman registers a human user's WebSocket connection.
// Unlike Register (used for agents with full protocol), human clients
// use ReadLoopHuman which only handles keepalive — no REGISTER/HEARTBEAT protocol.
// Messages sent via hub.Send(userID, ...) are delivered to this connection in real-time.
func (h *Hub) RegisterHuman(userID string, conn *websocket.Conn) *Client {
	c := &Client{
		AgentID:       userID,
		MessagingMode: "ws",
		conn:          conn,
		send:          make(chan []byte, 64),
	}
	h.mu.Lock()
	h.clients[userID] = c
	h.mu.Unlock()
	go c.writePump()

	// Subscribe to Redis channel for cross-instance delivery.
	_ = h.pubsub.Subscribe(h.ctx, func(msg Message) {
		h.deliverLocal(userID, msg)
	}, userID)

	// Deliver any pending offline messages immediately.
	go h.deliverInbox(userID, c)
	return c
}

// ReadLoopHuman is the read loop for human WebSocket clients.
// It only processes ping/pong keepalive frames; unregisters on disconnect.
func (h *Hub) ReadLoopHuman(c *Client) {
	defer func() {
		h.Unregister(c.AgentID)
		close(c.send)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		return nil
	})
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

const maxDepth = 10

func (h *Hub) Send(to string, msg Message) {
	if msg.Depth > maxDepth {
		log.Printf("hub: message depth exceeded (%d), dropping", msg.Depth)
		return
	}
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	if msg.ConversationID == "" {
		msg.ConversationID = uuid.New().String()
	}

	h.mu.RLock()
	c, online := h.clients[to]
	h.mu.RUnlock()

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	if online {
		select {
		case c.send <- data:
		default:
			log.Printf("hub: send buffer full for agent %s, saving to inbox", to)
			h.inbox.SaveOffline(to, msg)
		}
	} else {
		// Publish to Redis so another instance that has this agent's WS can deliver it.
		if pubErr := h.pubsub.Publish(h.ctx, to, msg); pubErr != nil {
			log.Printf("hub: redis publish to %s failed: %v — falling back to inbox", to, pubErr)
			h.inbox.SaveOffline(to, msg)
		} else {
			// Also save offline copy in case no instance currently holds the WS.
			h.inbox.SaveOffline(to, msg)
		}
	}

	// Persist all agent DMs for monitor/history visibility
	if msg.Type == MsgTypeAgentMessage {
		h.inbox.SaveDM(to, msg)
	}
}

// deliverLocal delivers a message received from Redis pub/sub to a locally-connected client.
func (h *Hub) deliverLocal(agentID string, msg Message) {
	h.mu.RLock()
	c, online := h.clients[agentID]
	h.mu.RUnlock()
	if !online {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		log.Printf("hub: deliverLocal buffer full for agent %s", agentID)
	}
}

func (h *Hub) Broadcast(msg Message) {
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		select {
		case c.send <- data:
		default:
		}
	}
}

// sendACK sends an ACK envelope back to the client.
func (c *Client) sendACK(traceID string) {
	ack := protocol.Envelope{
		ID:        uuid.New().String(),
		Type:      protocol.TypeACK,
		From:      "hub",
		To:        c.AgentID,
		Timestamp: time.Now(),
		Payload: protocol.ACKPayload{
			TraceID: traceID,
			Status:  "ok",
		},
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
	}
}

// sendHeartbeatACK responds to a HEARTBEAT with inbox messages.
func (c *Client) sendHeartbeatACK(inbox []Message) {
	inboxEnvelopes := make([]protocol.Envelope, 0, len(inbox))
	for _, m := range inbox {
		payload, _ := json.Marshal(m.Payload)
		inboxEnvelopes = append(inboxEnvelopes, protocol.Envelope{
			ID:        m.ID,
			Type:      protocol.MessageType(m.Type),
			From:      m.From,
			To:        m.To,
			Timestamp: m.Timestamp,
			Payload:   json.RawMessage(payload),
		})
	}

	ack := protocol.Envelope{
		ID:        uuid.New().String(),
		Type:      protocol.TypeACK,
		From:      "hub",
		To:        c.AgentID,
		Timestamp: time.Now(),
		Payload: protocol.HeartbeatACKPayload{
			Status:          "ok",
			NextHeartbeatIn: 30,
			Inbox:           inboxEnvelopes,
		},
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case data, ok := <-c.send:
			if !ok {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) ReadPump(h *Hub, onMsg func(agentID string, msg Message)) {
	defer func() {
		h.Unregister(c.AgentID)
		close(c.send)
		c.conn.Close()
	}()
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		// Try to parse as protocol.Envelope first (REGISTER, HEARTBEAT, etc.)
		var env protocol.Envelope
		if jsonErr := json.Unmarshal(data, &env); jsonErr == nil {
			switch env.Type {
			case protocol.TypeRegister:
				var p protocol.RegisterPayload
				if b, _ := json.Marshal(env.Payload); b != nil {
					_ = json.Unmarshal(b, &p)
				}
				// Update messaging mode on client
				h.mu.Lock()
				c.MessagingMode = p.MessagingMode
				if c.MessagingMode == "" {
					c.MessagingMode = "ws"
				}
				h.mu.Unlock()
				// Notify main.go to update DB
				if h.onRegister != nil {
					go h.onRegister(c.AgentID, p)
				}
				c.sendACK(env.TraceID)
				log.Printf("hub: agent %s registered (mode=%s, caps=%v)", c.AgentID, c.MessagingMode, p.Capabilities)
				continue

			case protocol.TypeHeartbeat:
				// Update last_seen in DB
				if h.onHeartbeat != nil {
					go h.onHeartbeat(c.AgentID)
				}
				// Pop inbox and return via HeartbeatACK
				inbox := h.inbox.PopOffline(c.AgentID)
				c.sendHeartbeatACK(inbox)
				continue

			case protocol.TypeTaskUpdate:
				var p protocol.TaskUpdatePayload
				if b, _ := json.Marshal(env.Payload); b != nil {
					_ = json.Unmarshal(b, &p)
				}
				if h.onTaskUpdate != nil {
					go h.onTaskUpdate(c.AgentID, p)
				}
				c.sendACK(env.TraceID)
				continue

			case protocol.TypeTaskResult:
				var p protocol.TaskResultPayload
				if b, _ := json.Marshal(env.Payload); b != nil {
					_ = json.Unmarshal(b, &p)
				}
				if h.onTaskResult != nil {
					go h.onTaskResult(c.AgentID, p)
				}
				c.sendACK(env.TraceID)
				continue
			}
		}

		// Fall through: handle as legacy hub.Message for routing
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		msg.From = c.AgentID
		msg.Depth++
		onMsg(c.AgentID, msg)
	}
}
