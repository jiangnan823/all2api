# all2api — Multi-Platform AI API Gateway

A unified reverse proxy gateway that pools free-tier quotas from multiple AI coding tools (WorkBuddy, Qoder, Trae...) into a single OpenAI-compatible API endpoint.

## Features

- **Multi-source pooling**: Independent account pools per platform with weighted random selection (health × credit-based)
- **Per-source circuit breaker**: 429 exponential backoff / 404 short cooldown / 402 hard cooldown / consecutive-failure trip
- **Session stickiness**: conversation_id→account binding with TTL roll-over, graceful unbinding on failure
- **OpenAI-compatible**: `/v1/chat/completions` (full) + `/v1/responses` (feature flag gated, M1)
- **Web admin panel**: 7 views (overview, accounts, tasks, models, logs, config, usage examples) — dark/light theme
- **Task scheduler**: Signin (09/21), active check (10), cat travel (09/21), token keepalive (22) — per-task on/off
- **Zero-config deploy**: GHCR images (amd64+arm64) / Docker Compose / Windows exe / fnOS .fpk

## Quick Start

### Install

**Docker Compose (recommended):**
```bash
git clone https://github.com/jiangnan823/all2api.git
cd all2api
docker compose up -d
```

**Windows executable:**
Download from [GitHub Releases](https://github.com/jiangnan823/all2api/releases).

### Configure

1. Place `config.json` in the same directory as the binary (or mount via Docker):
```json
{
  "listen": ":7800",
  "api_key": "your-secret-api-key",
  "auth_dir": "./auths",
  "pool": { "credit_floor": 100, "max_retries": 3, "cooldown_sec": 60 },
  "schedule": {
    "signin_cron": "0 9,21 * * *",
    "task_cron": "0 10 * * *",
    "token_keepalive_cron": "0 22 * * *"
  },
  "sources": {},
  "router": { "responses_api": { "enabled": false } }
}
```

2. Add accounts via the Web admin panel at `http://localhost:7800`.

### Use as OpenAI Client

Set your client configuration:
- **Base URL**: `http://localhost:7800/v1`
- **API Key**: Whatever you set as `api_key` in config.json
- **Model**: e.g. `workbuddy/deepseek-v4-flash`, or just `deepseek-v4-flash` (auto-routed)

Works with Cherry Studio, Claude Code, Codex CLI, OpenCode, etc.

## Architecture

```
Client ──→ all2api (:7800) ──→ Pool Selector ──→ Platform Adapter (W&B/Qoder/Trae)
                    │                                    │
                   Web Panel                            Health / Tasks / Auth
```

- **Adapter pattern**: Each platform is an isolated module (`internal/platform/<name>/adapter.go`) compiled via build tags (`-tags workbuddy`). Adding a new platform requires zero changes to core code.
- **Capabilities matrix**: `CapSet` bitmask (ChatCompletion, ResponsesStreaming, ToolChain, StructuredOutput, Reasoning, SSE re-build) replaces boolean flags.
- **Display primitives v2**: Closed-set of 5 UI rendering primitives (`calendar_streak`, `tier_progress`, `quota_countdown`, `validity_gauge`, `status_plain`) — unknown values fall back safely without frontend case statements.

## Build from Source

Requires Go 1.24+.

```bash
go build -o all2api ./cmd/gateway
go build -tags full -o all2api-full ./cmd/gateway      # all platforms
go build -tags workbuddy -o all2api-wb ./cmd/gateway   # WorkBuddy only
```

## Distribution Formats

| Format | Target | Command |
|--------|--------|---------|
| GHCR image (multi-arch) | Linux amd64/arm64 | CI automated |
| Docker Compose | Any container host | `docker compose up -d` |
| Windows exe | Windows desktop | Download `.exe` release |
| fnOS .fpk | Flying Network OS | Plugin installer |

## Project Structure

```
cmd/gateway/main.go          # Entry point (three endpoints: healthz, chat, responses)
internal/adapter/            # Adapter interface + registry + base class
internal/config/config.go    # Hot-reloadable JSON config
internal/circuit/            # Per-source circuit breaker
internal/pool/               # Weighted-random account selector
internal/schedule/           # Cron task scheduler
internal/sessions/           # Conversation stickiness manager
internal/observability/      # Structured logging scaffold
internal/platform/{name}/    # Per-platform adapter modules
```

## License

This project is for personal learning/self-hosted use only.
