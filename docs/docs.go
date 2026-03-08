package docs

import _ "embed"

//go:embed AGENTS.md
var AgentsMD []byte

//go:embed AGENTS.zh.md
var AgentsZhMD []byte
