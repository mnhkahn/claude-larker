# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Larker is a macOS service that bridges Claude Code to Lark/Feishu (飞书). When Claude Code shows a permission prompt or asks a question, Larker sends an interactive card to a Feishu/Lark group chat. The user can confirm operations from their phone. The confirmation is injected back into the terminal as keyboard input, so Claude Code's TUI continues to function normally.

**Critical requirement:** Claude Code must run inside a tmux session for terminal injection to work.

## Build and Run

```bash
# Build the binary
go build -o larker ./cmd/larker

# Run the server (reads ~/.larker/config.yaml)
./larker server

# Run a hook manually for testing
echo '{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"test","session_id":"xxx","cwd":"/tmp"}' | ./larker hook --phase=notification

# Install and configure everything (builds binary, writes launchd plist, configures Claude Code hooks)
bash install.sh
```

## Architecture

### Hook Scheme: Notification (Non-Blocking)

Larker uses Claude Code's `Notification` hook, **not** `PreToolUse`. This is the key architectural decision:

- `Notification` hooks fire when CC shows a prompt (permission, idle, elicitation dialog)
- The hook process sends the event to the Larker server and **immediately exits** — it does not block CC
- CC's TUI remains active; the user can still interact directly in the terminal
- When the user clicks a button in Feishu, the Larker server injects the corresponding key sequence into the terminal via `tmux send-keys`
- This makes Feishu act as a "remote keyboard" rather than an approval gate

The `PreToolUse` hook is still defined but returns deny with an error message directing users to use the Notification hook instead.

### Process Model

Two process types:

1. **Larker server** (`./larker server`) — single long-running process. Opens an HTTP/WebSocket server on `:8765` and maintains a Lark WebSocket long connection for receiving card callbacks.
2. **Hook clients** (`./larker hook --phase=...`) — short-lived processes forked by Claude Code. Connect to the server via WebSocket, send the event, and exit.

### Terminal Injection

`internal/terminal/terminal.go` resolves the terminal target by walking up the process tree from the hook PID to find a TTY device, then maps it to a tmux pane via `tmux list-panes -a`. Injection is done via `tmux send-keys` with proper key name mapping for ANSI sequences (`\x1b[B` → `Down`, `\r` → `Enter`, `\x1b` → `Escape`).

### Key Components

| Package | Responsibility |
|---------|---------------|
| `cmd/larker` | Entry point: `server` or `hook` subcommand |
| `internal/server` | HTTP/WebSocket server; session state; routes card callbacks to terminal injection |
| `internal/hook` | Reads CC hook JSON from stdin, forwards to server via WebSocket |
| `internal/terminal` | Resolves TTY/tmux target from PID, injects keys via `tmux send-keys` |
| `internal/lark` | Lark OpenAPI client for sending/updating cards and text messages; WebSocket long connection client |
| `internal/config` | Loads `~/.larker/config.yaml` |
| `internal/logger` | File-based logging with levels |
| `internal/sleep` | macOS sleep prevention via CGO + IOKit `IOPMAssertionCreateWithName` |
| `scripts/get-ids` | CLI tool to look up Feishu chat IDs and user open_ids via API |
| `scripts/ws-client.go` | Standalone WebSocket client for testing Lark event subscription |

## Configuration

Config lives at `~/.larker/config.yaml` (see `config.yaml.example`). Key fields:

- `lark.base_url`: `https://open.feishu.cn` (国内) or `https://open.larksuite.com` (国际)
- `lark.app_id` / `lark.app_secret`: Feishu/Lark custom app credentials
- `lark.receiver_type` / `lark.receiver_id`: `chat_id` for groups, `open_id` for individuals

## Claude Code Hook Configuration

`install.sh` writes to `~/.claude/settings.json` (or `settings.local.json`). The hooks registered are:

- `Notification` (matcher: `permission_prompt|idle_prompt|elicitation_dialog`) → `larker hook --phase=notification`
- `PostToolUse` → `larker hook --phase=post`
- `Stop` → `larker hook --phase=stop` + `afplay` sound
- `StopFailure` → `larker hook --phase=stopfailure`

The install script appends to existing hooks (does not overwrite) and backs up the original file.

## Common Tasks

```bash
# Check server logs
tail -f ~/.larker/larker.log

# Restart the launchd service
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.larker.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.larker.plist

# Test terminal injection manually
tmux new-session -d -s test "cat"
# In another terminal:
go run ./scripts/get-ids --list  # verify API connectivity
echo '{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"test","session_id":"test","cwd":"/tmp"}' | go run ./cmd/larker hook --phase=notification
```

## Important Notes

- **Do not suggest ngrok or any tunneling service.** This project uses Lark's WebSocket long connection exclusively. The user's memory explicitly forbids ngrok.
- The binary must be run inside tmux for terminal injection to work. The `LARKER_TMUX_TARGET` env var can override the auto-detected tmux pane.
- Cards use Feishu interactive card schema 1.0 (`config` + `header` + `elements`). Schema 2.0 tags like `action` inside `elements` are supported, but some newer tags are not.
- The `internal/lark/wsclient.go` uses the official `larksuite/oapi-sdk-go/v3` WebSocket types for frame parsing.
- `scripts/get-ids/` and `scripts/get-openid-api/` are standalone Go programs, each with their own `main.go` in a subdirectory.
