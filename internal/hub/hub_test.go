package hub_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claw-works/claw-hub/internal/hub"
	"github.com/claw-works/claw-hub/pkg/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// testInbox is a simple in-memory inbox backend for tests.
type testInbox struct {
	mu      sync.Mutex
	offline map[string][]hub.Message
}

func newTestInbox() *testInbox { return &testInbox{offline: make(map[string][]hub.Message)} }

func (b *testInbox) SaveOffline(agentID string, msg hub.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.offline[agentID] = append(b.offline[agentID], msg)
}

func (b *testInbox) PopOffline(agentID string) []hub.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.offline[agentID]
	delete(b.offline, agentID)
	return msgs
}

// startTestServer spins up an httptest.Server running a hub WS endpoint.
func startTestServer(t *testing.T, h *hub.Hub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.URL.Query().Get("agent_id")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		client := h.Register(agentID, conn)
		client.ReadPump(h, func(from string, msg hub.Message) {})
	})
	return httptest.NewServer(mux)
}

// dial connects a test WS client to the server.
func dial(t *testing.T, serverURL, agentID string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws?agent_id=" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func sendEnvelope(t *testing.T, conn *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	data, _ := json.Marshal(env)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

func readEnvelope(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

// TestRegisterACK verifies that a REGISTER message gets an ACK back.
func TestRegisterACK(t *testing.T) {
	var registered string
	h := hub.New()
	h.SetOnRegister(func(agentID string, p protocol.RegisterPayload) {
		registered = agentID
	})

	srv := startTestServer(t, h)
	defer srv.Close()

	agentID := uuid.New().String()
	conn := dial(t, srv.URL, agentID)
	defer conn.Close()

	sendEnvelope(t, conn, protocol.Envelope{
		ID:        uuid.New().String(),
		Type:      protocol.TypeRegister,
		From:      agentID,
		To:        "hub",
		Timestamp: time.Now(),
		Payload: protocol.RegisterPayload{
			Name:           "test-agent",
			Capabilities:   []string{"coding"},
			MessagingMode:  "ws",
			RuntimeVersion: "test/1.0",
		},
	})

	ack := readEnvelope(t, conn)
	if ack.Type != protocol.TypeACK {
		t.Fatalf("expected ACK, got %s", ack.Type)
	}

	// Give onRegister goroutine time to run
	time.Sleep(50 * time.Millisecond)
	if registered != agentID {
		t.Fatalf("onRegister not called: got %q, want %q", registered, agentID)
	}
}

// TestHeartbeatACK verifies that a HEARTBEAT returns a HeartbeatACK with inbox.
func TestHeartbeatACK(t *testing.T) {
	var heartbeated string
	inbox := newTestInbox()

	h := hub.New()
	h.SetInbox(inbox)
	h.SetOnHeartbeat(func(agentID string) { heartbeated = agentID })

	srv := startTestServer(t, h)
	defer srv.Close()

	agentID := uuid.New().String()

	// Pre-load an offline message into inbox
	inbox.SaveOffline(agentID, hub.Message{
		ID:        uuid.New().String(),
		Type:      "agent.message",
		From:      "other-agent",
		To:        agentID,
		Payload:   map[string]string{"text": "hello from inbox"},
		Timestamp: time.Now(),
	})

	conn := dial(t, srv.URL, agentID)
	defer conn.Close()

	// Consume the inbox.delivery message sent on connect
	readEnvelope(t, conn)

	// Now send HEARTBEAT
	sendEnvelope(t, conn, protocol.Envelope{
		ID:        uuid.New().String(),
		Type:      protocol.TypeHeartbeat,
		From:      agentID,
		To:        "hub",
		Timestamp: time.Now(),
	})

	ack := readEnvelope(t, conn)
	if ack.Type != protocol.TypeACK {
		t.Fatalf("expected ACK for heartbeat, got %s", ack.Type)
	}

	// Verify HeartbeatACKPayload
	payloadBytes, _ := json.Marshal(ack.Payload)
	var hbAck protocol.HeartbeatACKPayload
	if err := json.Unmarshal(payloadBytes, &hbAck); err != nil {
		t.Fatalf("unmarshal HeartbeatACKPayload: %v", err)
	}
	if hbAck.Status != "ok" {
		t.Fatalf("expected status ok, got %s", hbAck.Status)
	}

	time.Sleep(50 * time.Millisecond)
	if heartbeated != agentID {
		t.Fatalf("onHeartbeat not called: got %q, want %q", heartbeated, agentID)
	}
}

// TestPopInboxHTTPBlockedWhenWSActive verifies that PopInboxHTTP returns nil
// when the agent has an active WS connection (fixes WS/HTTP mix bug).
func TestPopInboxHTTPBlockedWhenWSActive(t *testing.T) {
	inbox := newTestInbox()
	h := hub.New()
	h.SetInbox(inbox)

	srv := startTestServer(t, h)
	defer srv.Close()

	agentID := uuid.New().String()
	inbox.SaveOffline(agentID, hub.Message{
		ID: uuid.New().String(), Type: "agent.message", Timestamp: time.Now(),
	})

	// Before WS connect: HTTP pop should work
	msgs := h.PopInboxHTTP(agentID)
	if len(msgs) == 0 {
		t.Fatal("expected HTTP pop to work before WS connect")
	}

	// Re-add message, then connect via WS
	inbox.SaveOffline(agentID, hub.Message{
		ID: uuid.New().String(), Type: "agent.message", Timestamp: time.Now(),
	})
	conn := dial(t, srv.URL, agentID)
	defer conn.Close()
	// consume inbox.delivery
	readEnvelope(t, conn)

	// Now HTTP pop should return nil (WS is active)
	msgs = h.PopInboxHTTP(agentID)
	if len(msgs) != 0 {
		t.Fatalf("expected HTTP pop to be blocked when WS active, got %d messages", len(msgs))
	}
}
