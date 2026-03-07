package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnvelopeSerialization(t *testing.T) {
	env := Envelope{
		Type:      TypeRegister,
		TraceID:   "test-trace-id",
		From:      "agent-1",
		To:        "hub",
		Timestamp: time.Now(),
		Payload: RegisterPayload{
			Name:         "test-agent",
			Capabilities: []string{"code", "search"},
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal envelope: %v", err)
	}

	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal envelope: %v", err)
	}

	if decoded.Type != TypeRegister {
		t.Errorf("expected type %s, got %s", TypeRegister, decoded.Type)
	}
	if decoded.TraceID != "test-trace-id" {
		t.Errorf("expected trace_id test-trace-id, got %s", decoded.TraceID)
	}
}

func TestMessageTypes(t *testing.T) {
	types := []MessageType{
		TypeRegister, TypeTaskAssign, TypeTaskUpdate,
		TypeTaskResult, TypeHeartbeat, TypeACK, TypeMessage,
	}
	for _, mt := range types {
		if mt == "" {
			t.Error("empty message type found")
		}
	}
}
