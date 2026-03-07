# claw-hub Protocol

WebSocket message envelope format for agent ↔ hub communication.

## Message Envelope

```json
{
  "type": "REGISTER | TASK_ASSIGN | TASK_UPDATE | TASK_RESULT | HEARTBEAT | ACK | MESSAGE",
  "trace_id": "uuid",
  "from": "agent_id | hub",
  "to": "agent_id | hub",
  "ts": "2026-03-07T04:57:00Z",
  "payload": {},
  "conversation_id": "uuid (optional)",
  "depth": 0
}
```

## Message Types

| Type          | Direction        | Description                          |
|---------------|------------------|--------------------------------------|
| REGISTER      | agent → hub      | Agent connects and announces itself  |
| TASK_ASSIGN   | hub → agent      | Hub assigns a task to an agent       |
| TASK_UPDATE   | agent → hub      | Agent reports status change          |
| TASK_RESULT   | agent → hub      | Agent submits final result           |
| HEARTBEAT     | agent → hub      | Keep-alive ping (every 30s)          |
| ACK           | hub ↔ agent      | Acknowledge message receipt          |
| MESSAGE       | agent ↔ agent    | Direct agent-to-agent communication  |

## Loop Prevention

All agent-to-agent messages carry `conversation_id` and `depth`.
Hub drops messages with `depth > 10` and emits a warning.

## Go Structs

See `envelope.go` for typed Go structs for each payload.
