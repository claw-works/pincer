// Package protocol defines the WebSocket message protocol between agents and the hub.
package protocol

import "time"

// MessageType defines the type of a message in the envelope.
type MessageType string

const (
	TypeRegister   MessageType = "REGISTER"
	TypeTaskAssign MessageType = "TASK_ASSIGN"
	TypeTaskUpdate MessageType = "TASK_UPDATE"
	TypeTaskResult MessageType = "TASK_RESULT"
	TypeHeartbeat  MessageType = "HEARTBEAT"
	TypeACK        MessageType = "ACK"
	TypeMessage    MessageType = "MESSAGE" // agent-to-agent direct message
)

// Envelope is the standard message wrapper for all WebSocket communication.
type Envelope struct {
	Type        MessageType `json:"type"`
	TraceID     string      `json:"trace_id"`
	From        string      `json:"from"`         // agent_id or "hub"
	To          string      `json:"to"`           // agent_id or "hub"
	Timestamp   time.Time   `json:"ts"`
	Payload     interface{} `json:"payload,omitempty"`

	// Loop prevention fields
	ConversationID string `json:"conversation_id,omitempty"`
	Depth          int    `json:"depth,omitempty"`
}

// RegisterPayload is sent by an agent on connection to identify itself.
type RegisterPayload struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// TaskAssignPayload is sent from hub to agent when a task is assigned.
type TaskAssignPayload struct {
	TaskID       string            `json:"task_id"`
	Title        string            `json:"title"`
	Requirements []string          `json:"requirements"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// TaskUpdatePayload is sent from agent to hub with a status update.
type TaskUpdatePayload struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // RUNNING | DONE | FAILED
	Message string `json:"message,omitempty"`
}

// TaskResultPayload is sent from agent to hub with the final result.
type TaskResultPayload struct {
	TaskID string `json:"task_id"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// HeartbeatPayload is sent periodically by agents to maintain online status.
type HeartbeatPayload struct {
	AgentID string `json:"agent_id"`
}

// ACKPayload acknowledges receipt of a message.
type ACKPayload struct {
	TraceID string `json:"trace_id"`
	Status  string `json:"status"` // ok | error
	Error   string `json:"error,omitempty"`
}
