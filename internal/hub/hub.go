package hub

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
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

// InboxStore is the interface the hub uses to persist offline messages.
type InboxStore interface {
	SaveInboxMessage(ctx context.Context, msg interface{}) error
	PopInbox(ctx context.Context, agentID string) (interface{}, error)
}

type Client struct {
	AgentID string
	conn    *websocket.Conn
	send    chan []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
	inbox   inboxBackend
}

type inboxBackend interface {
	SaveOffline(agentID string, msg Message)
	PopOffline(agentID string) []Message
}

type nopInbox struct{}

func (nopInbox) SaveOffline(_ string, _ Message) {}
func (nopInbox) PopOffline(_ string) []Message    { return nil }

func New() *Hub {
	return &Hub{
		clients: make(map[string]*Client),
		inbox:   nopInbox{},
	}
}

// SetInbox attaches a persistent inbox backend.
func (h *Hub) SetInbox(ib inboxBackend) { h.inbox = ib }

// PopInboxHTTP is for HTTP-polling agents that don't maintain a WS connection.
func (h *Hub) PopInboxHTTP(agentID string) []Message {
	return h.inbox.PopOffline(agentID)
}

func (h *Hub) Register(agentID string, conn *websocket.Conn) *Client {
	c := &Client{
		AgentID: agentID,
		conn:    conn,
		send:    make(chan []byte, 64),
	}
	h.mu.Lock()
	h.clients[agentID] = c
	h.mu.Unlock()
	go c.writePump()

	// deliver pending offline messages
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
}

const maxDepth = 10

func (h *Hub) Send(to string, msg Message) {
	if msg.Depth > maxDepth {
		log.Printf("hub: message depth exceeded (%d), dropping to prevent loop", msg.Depth)
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
		log.Printf("hub: agent %s offline, saving to inbox", to)
		h.inbox.SaveOffline(to, msg)
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
			// send ping to keep connection alive
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
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		msg.From = c.AgentID
		msg.Depth++
		onMsg(c.AgentID, msg)
	}
}
