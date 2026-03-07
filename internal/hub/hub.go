package hub

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

type MessageType string

const (
	MsgTypeTaskAssigned  MessageType = "task.assigned"
	MsgTypeAgentMessage  MessageType = "agent.message"
	MsgTypeBroadcast     MessageType = "broadcast"
)

type Message struct {
	Type    MessageType `json:"type"`
	From    string      `json:"from,omitempty"`
	To      string      `json:"to,omitempty"` // empty = broadcast
	Payload interface{} `json:"payload"`
}

type Client struct {
	AgentID string
	conn    *websocket.Conn
	send    chan []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client // agentID -> client
}

func New() *Hub {
	return &Hub{clients: make(map[string]*Client)}
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
	return c
}

func (h *Hub) Unregister(agentID string) {
	h.mu.Lock()
	delete(h.clients, agentID)
	h.mu.Unlock()
}

func (h *Hub) Send(to string, msg Message) {
	h.mu.RLock()
	c, ok := h.clients[to]
	h.mu.RUnlock()
	if !ok {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		log.Printf("hub: send buffer full for agent %s", to)
	}
}

func (h *Hub) Broadcast(msg Message) {
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
	for data := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
}

func (c *Client) ReadPump(h *Hub, onMsg func(agentID string, msg Message)) {
	defer func() {
		h.Unregister(c.AgentID)
		close(c.send)
		c.conn.Close()
	}()
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
		onMsg(c.AgentID, msg)
	}
}
