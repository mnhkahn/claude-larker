# CC Session-Lark Thread Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make each Claude Code session maintain its own Lark message thread, so multiple concurrent CC sessions no longer collide into a single thread.

**Architecture:** Move `threadMsgID` and `roundResults` from global `Server` fields into `CCSession` (keyed by `SessionID`). All handlers (`handleNotification`, `handlePostToolUse`, `handleStop`, `handleStopFailure`, `handleCardAction`) read/write these fields via the per-session `CCSession` instead of shared globals.

**Tech Stack:** Go, Lark OpenAPI

---

### File Map

- `internal/server/server.go` — only file to modify. Contains `Server` struct, `CCSession` struct, and all five handler methods that touch `threadMsgID`/`roundResults`.

---

### Task 1: Move fields from Server to CCSession

**Files:**
- Modify: `internal/server/server.go:33-59`

- [ ] **Step 1: Update CCSession struct**

Add `ThreadMsgID` and `RoundResults` fields to `CCSession`, remove `threadMsgID` and `roundResults` from `Server`.

```go
// CCSession tracks a single Claude Code instance lifecycle.
type CCSession struct {
	SessionID      string
	HookPID        int
	TerminalTarget string
	ThreadMsgID    string        // root Lark message ID for this CC session's thread
	RoundResults   []RoundResult // tool results accumulated during current round
	LastActivity   time.Time
}
```

`Server` struct becomes:

```go
type Server struct {
	cfg            *config.Config
	log            *logger.Logger
	lark           *lark.Client
	wsClient       *lark.WSClient
	terminal       *terminal.Injector

	sessions       map[string]*Session
	mu             sync.RWMutex

	allowedDevices map[string]bool // auto-approved terminals

	ccSessions     map[string]*CCSession // keyed by CC session_id
	botOpenID      string                // bot's own open_id for @mention filtering

	pendingReaction struct {
		MessageID  string
		ReactionID string
	}
}
```

- [ ] **Step 2: Build to verify no compile errors yet**

Run: `go build ./...`
Expected: compile errors in handlers (expected — will fix in next tasks)

---

### Task 2: Isolate handleNotification

**Files:**
- Modify: `internal/server/server.go:458-516`

- [ ] **Step 1: Replace global threadMsgID access with per-CCSession access**

Replace lines 462-483:

```go
	s.mu.Lock()
	threadID := ""
	cc, ccExists := s.ccSessions[req.SessionID]
	if ccExists {
		threadID = cc.ThreadMsgID
	}
	s.mu.Unlock()

	// idle_prompt and elicitation_dialog need a standalone input field.
	needInput := req.NotificationType == "idle_prompt" || req.NotificationType == "elicitation_dialog"

	var msgID string
	var sendErr error
	if threadID == "" {
		msgID, sendErr = s.lark.SendCard(title, cardMsg, sessionID, buttons, needInput)
	} else {
		msgID, sendErr = s.lark.ReplyCard(threadID, title, cardMsg, sessionID, buttons, needInput)
	}
	if sendErr != nil {
		s.log.Error("Failed to send Lark card: %v", sendErr)
		return
	}

	if threadID == "" && req.SessionID != "" {
		s.mu.Lock()
		if cc, ok := s.ccSessions[req.SessionID]; ok {
			cc.ThreadMsgID = msgID
			s.ccSessions[req.SessionID] = cc
		}
		s.mu.Unlock()
	}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: PASS (only handleNotification fixed so far)

---

### Task 3: Isolate handlePostToolUse

**Files:**
- Modify: `internal/server/server.go:528-550`

- [ ] **Step 1: Replace global threadMsgID and roundResults with per-CCSession**

Replace the entire method body:

```go
func (s *Server) handlePostToolUse(_ *websocket.Conn, req *HookRequest) {
	s.mu.RLock()
	var threadID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		threadID = cc.ThreadMsgID
	}
	s.mu.RUnlock()

	if threadID != "" {
		text := formatPostResult(req)
		if err := s.lark.ReplyMarkdown(threadID, req.ToolName, text); err != nil {
			s.log.Error("Failed to reply thread: %v", err)
		}
	}

	if req.ToolName != "" && req.SessionID != "" {
		s.mu.Lock()
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.RoundResults = append(cc.RoundResults, RoundResult{
			ToolName: req.ToolName,
			Result:   req.Result,
			Summary:  extractToolResult(req),
		})
		s.ccSessions[req.SessionID] = cc
		s.mu.Unlock()
		s.log.Info("PostToolUse: tool=%s session=%s", req.ToolName, req.SessionID)
	}
}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: PASS

---

### Task 4: Isolate handleStop

**Files:**
- Modify: `internal/server/server.go:552-623`

- [ ] **Step 1: Replace global threadMsgID and roundResults with per-CCSession**

Replace the entire method body:

```go
func (s *Server) handleStop(_ *websocket.Conn, req *HookRequest) {
	s.removeWorkingReaction()

	var b strings.Builder
	b.WriteString("✅ 本轮对话已结束")
	if req.Message != "" {
		b.WriteString("\n\n" + req.Message)
	}

	// Summarize accumulated tool results for the round.
	s.mu.Lock()
	var results []RoundResult
	var threadID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		results = cc.RoundResults
		threadID = cc.ThreadMsgID
		cc.RoundResults = nil
		cc.ThreadMsgID = ""
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	if len(results) > 0 {
		b.WriteString("\n\n**执行结果汇总：**\n")
		for _, r := range results {
			if r.Summary == "" {
				b.WriteString(fmt.Sprintf("\n- ✅ `%s`", r.ToolName))
			} else {
				b.WriteString(fmt.Sprintf("\n- ✅ `%s`：\n%s", r.ToolName, r.Summary))
			}
		}
	}
	msg := b.String()

	if threadID != "" {
		if err := s.lark.ReplyMarkdown(threadID, "", msg); err != nil {
			s.log.Error("Failed to send stop reply: %v", err)
		}
	} else {
		s.lark.SendMarkdown("", msg)
	}

	// Resolve terminal target for message forwarding.
	var targetStr string
	if req.TmuxTarget != "" {
		targetStr = "tmux:" + req.TmuxTarget
	} else if req.HookPid > 0 {
		if target, err := s.terminal.ResolveTarget(req.HookPid); err == nil && target != nil {
			targetStr = target.String()
		}
	}

	// Update CC session activity; do NOT clear sessions or state.
	s.mu.Lock()
	if req.SessionID != "" {
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.HookPID = req.HookPid
		cc.LastActivity = time.Now()
		if targetStr != "" {
			cc.TerminalTarget = targetStr
		}
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	s.log.Info("Stop handled: round ended, session=%s tmux=%s", req.SessionID, targetStr)
}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: PASS

---

### Task 5: Isolate handleStopFailure

**Files:**
- Modify: `internal/server/server.go:625-681`

- [ ] **Step 1: Replace global threadMsgID and roundResults with per-CCSession**

Replace the entire method body:

```go
func (s *Server) handleStopFailure(_ *websocket.Conn, req *HookRequest) {
	s.removeWorkingReaction()

	s.mu.Lock()
	var threadID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		cc.RoundResults = nil
		threadID = cc.ThreadMsgID
		cc.ThreadMsgID = ""
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	msg := fmt.Sprintf("❌ 异常退出: %s", req.NotificationType)
	if req.Message != "" {
		msg += "\n\n" + req.Message
	}

	if threadID != "" {
		if err := s.lark.ReplyMarkdown(threadID, "", msg); err != nil {
			s.log.Error("Failed to send stop failure reply: %v", err)
		}
	} else {
		s.lark.SendMarkdown("", msg)
	}

	// Resolve terminal target for message forwarding.
	var targetStr string
	if req.TmuxTarget != "" {
		targetStr = "tmux:" + req.TmuxTarget
	} else if req.HookPid > 0 {
		if target, err := s.terminal.ResolveTarget(req.HookPid); err == nil && target != nil {
			targetStr = target.String()
		}
	}

	// Update CC session activity; do NOT clear sessions or state.
	s.mu.Lock()
	if req.SessionID != "" {
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.HookPID = req.HookPid
		cc.LastActivity = time.Now()
		if targetStr != "" {
			cc.TerminalTarget = targetStr
		}
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	s.log.Info("StopFailure handled: error=%s, session=%s tmux=%s", req.NotificationType, req.SessionID, targetStr)
}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: PASS

---

### Task 6: Isolate handleCardAction error reply

**Files:**
- Modify: `internal/server/server.go:730-745`

- [ ] **Step 1: Replace global threadMsgID with per-CCSession in error path**

Replace lines 732-744:

```go
	// Resolve target.
	if session.TerminalTarget == "" {
		s.log.Error("No terminal target for session %s", sessionID)
		s.mu.RLock()
		var threadID string
		if cc, ok := s.ccSessions[session.SessionID]; ok {
			threadID = cc.ThreadMsgID
		}
		s.mu.RUnlock()
		msg := fmt.Sprintf("⚠️ Session `%s` 没有可注入的终端目标，请直接在 Claude Code 终端中操作", sessionID)
		if threadID != "" {
			_ = s.lark.ReplyMarkdown(threadID, "", msg)
		} else {
			_, _ = s.lark.SendMarkdown("", msg)
		}
		return
	}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: PASS

---

### Task 7: Final verification

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: PASS with zero errors

- [ ] **Step 2: Run tests if any**

Run: `go test ./...`
Expected: PASS (project may have no tests)

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go docs/superpowers/plans/2026-04-20-cc-session-lark-thread-isolation.md
git commit -m "fix(server): isolate Lark thread and round results per CC session

Move threadMsgID and roundResults from global Server fields into CCSession.
Each Claude Code session now maintains its own Lark message thread,
preventing concurrent sessions from colliding into a single thread."
```

---

## Self-Review

**1. Spec coverage:**
- `threadMsgID` global → per-CCSession: covered in Tasks 1, 2, 4, 5, 6
- `roundResults` global → per-CCSession: covered in Tasks 1, 3, 4, 5
- `handleNotification` thread creation/reply: covered in Task 2
- `handlePostToolUse` reply + accumulation: covered in Task 3
- `handleStop` summary + reset: covered in Task 4
- `handleStopFailure` reset: covered in Task 5
- `handleCardAction` error reply: covered in Task 6
- No gaps identified.

**2. Placeholder scan:**
- No TBD/TODO/fill in details found.
- All code blocks contain exact replacement code.

**3. Type consistency:**
- `CCSession.ThreadMsgID` is `string` everywhere.
- `CCSession.RoundResults` is `[]RoundResult` everywhere.
- Field names match across all tasks.
