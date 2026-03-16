# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Email Automation Agent - A Go-based system that polls QQ Email via IMAP, generates code using local LLM CLI (Claude Code/Codex), executes it in Docker sandbox, and replies results via SMTP.

## Common Commands

```bash
# Quick start (recommended)
./start.sh

# Run directly
go run cmd/main.go -config configs/default.yaml

# Build binary
go build -o bin/email-automation-agent cmd/main.go

# Install dependencies
go mod tidy
```

## Architecture Overview

### High-Level Flow
1. **Channel Layer** (`internal/channel/`): Abstracts Email/IM via `channel.Channel` interface
2. **Agent** (`internal/agent/agent.go`): Polls messages, manages task queue, handles caching
3. **LLM** (`internal/llm/`): Factory pattern supporting `claude_code` and `codex` CLI providers
4. **Executor** (`internal/executor/`): Runs generated code in Docker sandbox with resource limits

### Key Design Patterns

**Channel Abstraction** (`internal/channel/channel.go`):
- `Channel` interface unifies Email/IM interactions
- `EmailChannel` implements IMAP/SMTP via `internal/email/client.go`
- `IMChannel` is a skeleton for future IM platform integration

**Subagent Queue Mode**:
- When `email.use_subagent: true`, tasks are queued and processed concurrently
- Configurable queue size (`subagent_queue_size`) and max concurrent tasks (`max_concurrent_tasks`)

**Tool Caching** (`internal/agent/agent.go`):
- LLM-generated tools are cached with SHA256 hash of task description
- Cache has TTL (`cache.ttl`) and max entries limit (`cache.max_entries`)
- Cache file persisted at `/tmp/email-automation-cache.json`

**Hot Reload**:
- Config file changes are auto-detected via file modification time
- Affects: `allowed_senders`, `llm.*`, `executor.*`, `cache.*`

**Docker Sandbox** (`internal/executor/executor.go`):
- Generated code runs in isolated containers
- Resource limits: 1 CPU, 512MB memory, 128 PIDs
- Network control via `executor.sandbox_allow_network`
- Read-only root FS, `/tmp` as tmpfs (64MB)

### State Persistence

Agent maintains state in `/tmp/email-automation-state.json`:
- `processed_uids`: Seen message UIDs to avoid reprocessing
- `pending_tasks`: Tasks awaiting user reply (multi-turn conversations)
- `tool_cache`: Cached LLM results
- Token usage statistics

### Configuration

Uses Viper for config management (`internal/config/config.go`):
- Config file: YAML format (default: `configs/default.yaml`)
- Environment variables are expanded: `${VAR_NAME}`
- `.env` file support via `start.sh`

Key config sections:
- `email`: IMAP/SMTP settings, whitelist (`allowed_senders`), polling interval
- `llm`: Provider (`claude_code`/`codex`), command, timeout
- `executor`: Sandbox toggle, allowed languages, work directory
- `cache`: TTL, max entries
- `status_report`: Periodic status emails to recipients

### Security Model

1. **Whitelist**: Only `email.allowed_senders` can trigger tasks
2. **Sandbox**: All code runs in Docker with resource/network restrictions
3. **Language whitelist**: Only `executor.allowed_languages` permitted
4. **Status commands**: Special recipients can control reporting via email commands (`status show`, `status interval 1h`, etc.)

### Multi-turn Conversations

When task fails or needs clarification:
- Agent sends reply requesting more info
- User replies to same email thread
- Agent recognizes `ReplyToID`, loads `pending_tasks` state
- Continues in original context instead of starting fresh
