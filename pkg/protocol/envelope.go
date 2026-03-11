// Package protocol defines the WebSocket message protocol between agents and the hub.
package protocol

import "time"

// ProtocolVersion is the current protocol version.
const ProtocolVersion = "1.0"

// MessageType defines the type of a message in the envelope.
type MessageType string

const (
	// Agent lifecycle
	TypeRegister  MessageType = "REGISTER"
	TypeHeartbeat MessageType = "HEARTBEAT"
	TypeACK       MessageType = "ACK"
	TypeAuth      MessageType = "AUTH"

	// Task lifecycle
	TypeTaskAssign MessageType = "TASK_ASSIGN"
	TypeTaskUpdate MessageType = "TASK_UPDATE"
	TypeTaskResult MessageType = "TASK_RESULT"

	// Communication
	TypeMessage   MessageType = "MESSAGE"   // agent-to-agent direct message
	TypeBroadcast MessageType = "BROADCAST" // hub-to-all broadcast

	// Connection management
	TypePing  MessageType = "PING"
	TypePong  MessageType = "PONG"
	TypeError MessageType = "ERROR"

	// Hub internal (mirrored from hub.go for cross-package use)
	TypeAgentMessage  MessageType = "agent.message"
	TypeTaskAssigned  MessageType = "task.assigned"
	TypeBroadcastEvt  MessageType = "broadcast"
	TypeInboxDelivery MessageType = "inbox.delivery"
)

// Envelope is the standard message wrapper for all WebSocket communication.
//
// All messages between agents and the hub must use this format.
// Fields:
//   - ID: unique message ID (UUID, assigned by sender or hub)
//   - Type: message type (see MessageType constants)
//   - From: sender agent_id or "hub"
//   - To: recipient agent_id, "hub", or "*" for broadcast
//   - TraceID: optional trace ID for request correlation
//   - ConversationID: groups messages in a logical conversation (loop prevention)
//   - Depth: hop count, incremented at each agent hop; hub drops if Depth > MaxDepth
//   - Timestamp: message creation time
//   - Payload: type-specific payload (see Payload types below)
type Envelope struct {
	ID        string      `json:"id"`
	Type      MessageType `json:"type"`
	From      string      `json:"from"`                    // agent_id or "hub"
	To        string      `json:"to"`                      // agent_id, "hub", or "*"
	TraceID   string      `json:"trace_id,omitempty"`
	Timestamp time.Time   `json:"ts"`
	Payload   interface{} `json:"payload,omitempty"`

	// Loop prevention
	ConversationID string `json:"conversation_id,omitempty"`
	Depth          int    `json:"depth,omitempty"` // hub drops if > MaxDepth
}

// MaxDepth is the maximum number of agent hops before a message is dropped.
const MaxDepth = 5

// --- Payload types ---

// RegisterPayload is sent by an agent on WS connection to declare its identity.
type RegisterPayload struct {
	Name            string   `json:"name"`
	Capabilities    []string `json:"capabilities"`
	RuntimeVersion  string   `json:"runtime_version,omitempty"`  // e.g. "openclaw/2026.3.2"
	MessagingMode   string   `json:"messaging_mode,omitempty"`   // "ws" | "poll"
}

// AuthPayload is sent after REGISTER to authenticate via API key.
// The hub responds with ACK (ok or error).
type AuthPayload struct {
	APIKey string `json:"api_key"`
}

// HeartbeatPayload is sent periodically to maintain online status.
// Hub responds with HeartbeatACKPayload which may include inbox messages.
type HeartbeatPayload struct {
	AgentID string `json:"agent_id"`
}

// HeartbeatACKPayload is the hub's response to a heartbeat.
type HeartbeatACKPayload struct {
	Status          string      `json:"status"` // "ok"
	NextHeartbeatIn int         `json:"next_heartbeat_in,omitempty"` // seconds
	Inbox           []Envelope  `json:"inbox,omitempty"`             // pending offline messages
}

// TaskAssignPayload is sent from hub to agent when a task is assigned.
type TaskAssignPayload struct {
	TaskID             string            `json:"task_id"`
	Title              string            `json:"title"`
	Description        string            `json:"description,omitempty"`
	Guidance           string            `json:"guidance,omitempty"`
	AcceptanceCriteria string            `json:"acceptance_criteria,omitempty"`
	Requirements       []string          `json:"requirements"`
	Priority           int               `json:"priority,omitempty"`
	Deadline           *time.Time        `json:"deadline,omitempty"`
	ReportChannel      *ReportChannel    `json:"report_channel,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// ReportChannel specifies where a task result should be reported.
// Used to close the "agent communicates → reports to channel" loop.
type ReportChannel struct {
	Type      string `json:"type"`       // "discord_thread" | "webhook" | "feishu"
	ChannelID string `json:"channel_id"` // Discord thread ID, webhook URL, etc.
}

// TaskUpdatePayload is sent from agent to hub with an interim status update.
type TaskUpdatePayload struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "running" | "done" | "failed"
	Message string `json:"message,omitempty"`
}

// TaskResultPayload is sent from agent to hub with the final task result.
// Depth should NOT be incremented when forwarding results.
type TaskResultPayload struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // "done" | "failed"
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// MessagePayload is used for agent-to-agent direct messages.
type MessagePayload struct {
	Action   string `json:"action,omitempty"`  // optional hint for receiver (e.g. "discord_thread_reply")
	Text     string `json:"text"`
	ThreadID string `json:"thread_id,omitempty"` // for discord_thread_reply
}

// ACKPayload acknowledges receipt of a message.
type ACKPayload struct {
	TraceID string `json:"trace_id"`
	Status  string `json:"status"` // "ok" | "error"
	Error   string `json:"error,omitempty"`
}

// ErrorPayload is sent by the hub when an error occurs (Type = TypeError).
type ErrorPayload struct {
	Code    string `json:"code"`              // e.g. "AUTH_FAILED", "DEPTH_EXCEEDED"
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"` // original message trace_id if applicable
}
