package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"github.com/mnhkahn/larker/internal/config"
	"github.com/mnhkahn/larker/internal/lark"
	"github.com/mnhkahn/gogogo/logger"
	"github.com/mnhkahn/larker/internal/sleep"
	"github.com/mnhkahn/larker/internal/terminal"
)

type Server struct {
	cfg            *config.Config
	log            *logger.Logger
	lark           *lark.Client
	wsClient       *lark.WSClient
	terminal       *terminal.Injector
	sleepMgr       *sleep.Manager

	activeLoops map[string]*ActiveLoop // keyed by NotificationID; quick lookup for card callbacks
	mu          sync.RWMutex

	allowedDevices map[string]bool // auto-approved terminals

	ccSessions   map[string]*CCSession // keyed by CC session_id
	botOpenID    string                // bot's own open_id for @mention filtering
	excludePaths []string              // CWD substrings to filter out (e.g. CodexBar paths)

	pendingReaction struct {
		MessageID  string
		ReactionID string
	}
}

// LoopType 表示 Loop 的交互类型。
type LoopType string

const (
	LoopTypePermissionPrompt  LoopType = "permission_prompt"  // 需用户确认的工具调用
	LoopTypeIdlePrompt        LoopType = "idle_prompt"        // 等待用户输入
	LoopTypeElicitationDialog LoopType = "elicitation_dialog" // 询问对话框
)

// Loop 记录一次 Claude Code 交互的完整生命周期。
// 无论是工具调用（需用户确认）还是普通消息（等待输入），都会生成一个 Loop。
type Loop struct {
	LoopID           string                   // Loop 唯一标识（工具调用时取 req.ToolUseID，其他情况用 genID() 生成）
	Type             LoopType                 // 交互类型
	ToolName         string                   // 工具名称（Type=permission_prompt 时有效）
	Params           map[string]any           // 工具调用参数
	Result           map[string]any           // 工具执行结果（PostToolUse 回填）
	Summary          string                   // 人类可读的结果摘要（如文件内容、命令输出）
	Status           string                   // 生命周期状态：pending(待确认) / allowed(已允许) / denied(已拒绝) / completed(已执行完成)
	CardMsgID        string                   // 卡片消息 ID
	NotificationID   string                   // 回调路由 ID（用于卡片回调查找）
	Responses        map[string]ResponseEntry // 按钮映射
	NotificationType string
	CreatedAt        time.Time
}

// ActiveLoop bundles a Loop with its parent Turn for card callback lookup.
type ActiveLoop struct {
	Loop *Loop
	Turn *Turn
}

// Turn 表示一次 CC 回合（PromptSubmit → Stop），与 ThreadMsgID 1:1。
type Turn struct {
	TurnID         string
	SessionID      string // CC session_id
	ThreadMsgID    string // 飞书话题根消息 ID
	TerminalTarget string
	Loops          []Loop
	Status         string // running / completed / failed
	StartTime      time.Time
	EndTime        *time.Time
	Cwd            string
	ProjectName    string
	FirstMessage        string // 当前 Turn 的第一个 Prompt 内容（用于 Lark 标题展示）
	WorkingReactionID   string // 飞书消息 reaction ID，turn 结束时撤回
}

// CCSession tracks a single Claude Code instance lifecycle.
type CCSession struct {
	SessionID      string
	HookPID        int
	AIPid          int    // AI 助手进程 PID（从 HookPID 的父进程推导）
	TerminalTarget string
	LastActivity   time.Time
	Active         bool // true while Claude Code is running
	Turns          []*Turn
	Cwd            string
	Service        string
	StartMsgID     string // 飞书 sessionstart 消息的 ID，作为 thread 根消息
}

// NewTurn creates a new Turn for this CC session.
func (cc *CCSession) NewTurn(cwd, projectName, terminalTarget string) *Turn {
	turn := &Turn{
		TurnID:         genID(),
		SessionID:      cc.SessionID,
		StartTime:      time.Now(),
		Status:         "running",
		Cwd:            cwd,
		ProjectName:    projectName,
		TerminalTarget: terminalTarget,
	}
	cc.Turns = append(cc.Turns, turn)
	return turn
}

// CurrentTurn returns the active (latest) Turn for this CC session, if any.
func (cc *CCSession) CurrentTurn() *Turn {
	if len(cc.Turns) == 0 {
		return nil
	}
	return cc.Turns[len(cc.Turns)-1]
}

// EndCurrentTurn finalizes the active Turn with the given status.
func (cc *CCSession) EndCurrentTurn(status string) *Turn {
	turn := cc.CurrentTurn()
	if turn == nil {
		return nil
	}
	now := time.Now()
	turn.Status = status
	turn.EndTime = &now
	return turn
}

type ResponseEntry struct {
	Keys  string
	Label string
}

type HookRequest struct {
	ID               string                 `json:"id"`
	Phase            string                 `json:"phase"`
	ToolUseID        string                 `json:"tool_use_id"`
	ToolName         string                 `json:"tool_name"`
	Params           map[string]any         `json:"params,omitempty"`
	Result           map[string]any         `json:"result,omitempty"`
	Message          string                 `json:"message,omitempty"`
	NotificationType string                 `json:"notification_type,omitempty"`
	SessionID        string                 `json:"session_id,omitempty"`
	Cwd              string                 `json:"cwd,omitempty"`
	TranscriptPath   string                 `json:"transcript_path,omitempty"`
	ToolInput        map[string]any         `json:"tool_input,omitempty"`
	HookPid          int                    `json:"hook_pid,omitempty"`
	TmuxTarget       string                 `json:"tmux_target,omitempty"`
	Service          string                 `json:"service,omitempty"`
}

type HookResponse struct {
	Result string `json:"result"` // allow, deny
	Error  string `json:"error,omitempty"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func New(cfg *config.Config, log *logger.Logger) *Server {
	s := &Server{
		cfg:            cfg,
		log:            log,
		lark:           lark.New(cfg.Lark, log),
		terminal:       terminal.New(log),
		sleepMgr:       sleep.New(log),
		activeLoops:    make(map[string]*ActiveLoop),
		allowedDevices: make(map[string]bool),
		ccSessions:     make(map[string]*CCSession),
		excludePaths:   cfg.Filter.ExcludePaths,
	}

	s.wsClient = lark.NewWSClient(cfg.Lark.AppID, cfg.Lark.AppSecret, cfg.Lark.BaseURL, log)
	s.wsClient.SetOnCardAction(s.handleCardAction)
	s.wsClient.SetOnIMMessage(s.handleIMMessage)

	// Async fetch bot open_id for @mention filtering.
	go func() {
		if id, err := s.lark.GetBotInfo(); err == nil && id != "" {
			s.mu.Lock()
			s.botOpenID = id
			s.mu.Unlock()
			s.log.Info("Bot open_id resolved: %s", id)
		} else {
			s.log.Error("Failed to resolve bot open_id: %v", err)
		}
	}()

	return s
}

func (s *Server) updateSleepState() {
	s.mu.RLock()
	pidSet := make(map[int]struct{})
	for _, cc := range s.ccSessions {
		if cc.Active && cc.AIPid > 1 {
			pidSet[cc.AIPid] = struct{}{}
		}
	}
	// 如果 activeLoop 存在但未收集到 AIPid，尝试从 activeLoop 关联的 session 获取
	if len(pidSet) == 0 && len(s.activeLoops) > 0 {
		for _, al := range s.activeLoops {
			if al.Turn != nil && al.Turn.SessionID != "" {
				if cc, ok := s.ccSessions[al.Turn.SessionID]; ok && cc.AIPid > 1 {
					pidSet[cc.AIPid] = struct{}{}
				}
			}
		}
	}
	activePids := make([]int, 0, len(pidSet))
	for pid := range pidSet {
		activePids = append(activePids, pid)
	}
	s.mu.RUnlock()

	if len(activePids) > 0 {
		s.sleepMgr.SyncPids(activePids)
	} else {
		s.sleepMgr.AllowSleep()
	}
}

// getAIPid 从 hook 进程 PID 向上查找一层父进程，作为 AI 助手进程 PID。
func getAIPid(hookPid int) int {
	if hookPid <= 1 {
		return 0
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(hookPid)).Output()
	if err != nil {
		return 0
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid <= 1 {
		return 0
	}
	return ppid
}

func (s *Server) Run() error {
	http.HandleFunc("/ws", s.handleWS)

	go func() {
		if err := s.wsClient.Start(context.Background()); err != nil {
			s.log.Error("Lark WS client failed: %v", err)
		}
	}()

	go s.cleanupLoop()

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.log.Info("Server listening on %s", addr)
	return http.ListenAndServe(addr, nil)
}

// cleanupLoop periodically removes expired turns and ccSessions.
func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		timeout := time.Duration(s.cfg.Timeout.Confirmation) * time.Second
		if timeout == 0 {
			timeout = 30 * time.Minute
		}
		s.mu.Lock()
		for _, cc := range s.ccSessions {
			valid := make([]*Turn, 0, len(cc.Turns))
			for _, turn := range cc.Turns {
				endTime := turn.StartTime
				if turn.EndTime != nil {
					endTime = *turn.EndTime
				}
				if time.Since(endTime) <= timeout {
					valid = append(valid, turn)
				}
			}
			cc.Turns = valid
		}
		for id, cc := range s.ccSessions {
			if time.Since(cc.LastActivity) > timeout {
				delete(s.ccSessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// isCwdExcluded checks if the hook event's CWD matches any excluded path substring.
// Returns true if the event should be silently dropped.
func (s *Server) isCwdExcluded(cwd string) bool {
	if cwd == "" || len(s.excludePaths) == 0 {
		return false
	}
	for _, p := range s.excludePaths {
		if strings.Contains(cwd, p) {
			return true
		}
	}
	return false
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	for {
		var req HookRequest
		if err := conn.ReadJSON(&req); err != nil {
			s.log.Debug("WebSocket read error: %v", err)
			return
		}

		s.log.Info("WS request: phase=%s tool=%s notification_type=%s", req.Phase, req.ToolName, req.NotificationType)

		// Filter: silently drop events from excluded CWD paths (e.g. CodexBar probes).
		if s.isCwdExcluded(req.Cwd) {
			s.log.Debug("WS request filtered (excluded CWD): phase=%s cwd=%s", req.Phase, req.Cwd)
			continue
		}

		switch req.Phase {
		case "pre":
			s.handlePreToolUse(conn, &req)
		case "post":
			s.handlePostToolUse(conn, &req)
		case "stop":
			s.handleStop(conn, &req)
		case "stopfailure":
			s.handleStopFailure(conn, &req)
		case "notification":
			s.handleNotification(conn, &req)
		case "sessionstart":
			s.handleSessionStart(conn, &req)
		case "promptsubmit":
			reqJSON, _ := json.Marshal(req)
			s.log.Info("WS promptsubmit raw: %s", string(reqJSON))
			s.log.Info("WS promptsubmit: session=%s message=%q", req.SessionID, req.Message)
			s.handlePromptSubmit(conn, &req)
		}
	}
}

func (s *Server) handleNotification(_ *websocket.Conn, req *HookRequest) {
	var target *terminal.Target
	var targetStr string

	s.log.Info("handleNotification: hook_pid=%d session_id=%s tmux_target=%q notification_type=%s", req.HookPid, req.SessionID, req.TmuxTarget, req.NotificationType)

	// Strategy 0: use tmux target detected by the hook process itself.
	if req.TmuxTarget != "" {
		target = &terminal.Target{Type: "tmux", Target: req.TmuxTarget}
		targetStr = target.String()
		s.log.Info("Using tmux target from hook: %s", targetStr)
	}

	// Strategy 1: resolve from hook PID.
	var resolveErr error
	if target == nil {
		startPid := req.HookPid
		if startPid == 0 {
			startPid = os.Getpid()
		}
		var err error
		target, err = s.terminal.ResolveTarget(startPid)
		if err != nil {
			s.log.Error("Failed to resolve terminal target for pid %d: %v", startPid, err)
			target, err = s.terminal.ResolveTarget(os.Getpid())
			if err != nil {
				s.mu.RLock()
				if cc, ok := s.ccSessions[req.SessionID]; ok && cc.TerminalTarget != "" {
					targetStr = cc.TerminalTarget
					target = s.parseTarget(targetStr)
					s.log.Info("Using historical terminal target from session %s: %s", req.SessionID, targetStr)
				}
				s.mu.RUnlock()
				if target == nil {
					resolveErr = fmt.Errorf("no terminal device found for pid %d", startPid)
					s.log.Error("Failed to resolve any terminal target: %v", resolveErr)
				}
			}
		}
		if target != nil {
			targetStr = target.String()
		}
	}

	// Auto-approve: if this terminal is in allowedDevices, inject "2" (Allow for session) immediately.
	if req.NotificationType == "permission_prompt" && targetStr != "" && s.allowedDevices[targetStr] {
		_ = s.terminal.InjectKeys(target, "2")
		s.log.Info("Auto-approved for device %s", targetStr)
		return
	}

	notificationID := genID()

	// Build card content from tool info.
	var cardMsg string
	if req.ToolName != "" {
		cardMsg = formatToolDesc(req)
	} else {
		cardMsg = req.Message
	}

	if resolveErr != nil {
		cardMsg += fmt.Sprintf("\n\n---\n⚠️ **终端注入失败**: %v\n\n**排查建议**:\n1. 确保 Claude Code 在 tmux 会话中运行\n2. 设置环境变量 `LARKER_TMUX_TARGET=session:window.pane`\n3. 直接在终端中操作", resolveErr)
	}

	// Build buttons and response mapping.
	var buttons []map[string]any
	var responses map[string]ResponseEntry

	switch req.NotificationType {
	case "idle_prompt":
		buttons = nil
		responses = map[string]ResponseEntry{}
	case "permission_prompt":
		parsedOpts := parseOptionsFromMessage(req.Message)
		if len(parsedOpts) > 0 {
			isDropdown := false
			for _, opt := range parsedOpts {
				if opt.Selected {
					isDropdown = true
					break
				}
			}

			optionList := formatOptionsForCard(parsedOpts)
			if optionList != "" {
				cardMsg += "\n\n---\n\n" + optionList
			}

			if isDropdown {
				buttons = []map[string]any{
					{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": "⬆️ 上移"},
						"type":  "default",
						"value": map[string]any{"action": "up", "session_id": notificationID},
					},
					{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": "⬇️ 下移"},
						"type":  "default",
						"value": map[string]any{"action": "down", "session_id": notificationID},
					},
					{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": "✅ 确认"},
						"type":  "primary",
						"value": map[string]any{"action": "enter", "session_id": notificationID},
					},
					{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": "❌ 取消"},
						"type":  "danger",
						"value": map[string]any{"action": "deny", "session_id": notificationID},
					},
					{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": "🔓 全局允许"},
						"type":  "default",
						"value": map[string]any{"action": "bypass", "session_id": notificationID},
					},
				}
				responses = map[string]ResponseEntry{
					"up":     {Keys: "\x1b[A", Label: "上移"},
					"down":   {Keys: "\x1b[B", Label: "下移"},
					"enter":  {Keys: "\r", Label: "确认"},
					"deny":   {Keys: "\x1b", Label: "已拒绝"},
					"bypass": {Keys: "2", Label: "全局允许"},
				}
			} else {
				buttons = make([]map[string]any, 0, len(parsedOpts)+1)
				responses = make(map[string]ResponseEntry, len(parsedOpts)+1)
				for _, opt := range parsedOpts {
					btnColor := "default"
					if isPositive(opt.Text) {
						btnColor = "green"
					} else if isNegative(opt.Text) {
						btnColor = "red"
					}
					buttons = append(buttons, map[string]any{
						"tag":   "button",
						"text":  map[string]any{"tag": "plain_text", "content": fmt.Sprintf("%d. %s", opt.Num, opt.Text)},
						"type":  btnColor,
						"value": map[string]any{"action": fmt.Sprintf("opt_%d", opt.Num), "session_id": notificationID},
					})
					responses[fmt.Sprintf("opt_%d", opt.Num)] = ResponseEntry{
						Keys:  fmt.Sprintf("%d", opt.Num),
						Label: opt.Text,
					}
				}
				buttons = append(buttons, map[string]any{
					"tag":   "button",
					"text":  map[string]any{"tag": "plain_text", "content": "🔓 全局允许"},
					"type":  "default",
					"value": map[string]any{"action": "bypass", "session_id": notificationID},
				})
				responses["bypass"] = ResponseEntry{Keys: "2", Label: "全局允许"}
			}
		} else {
			buttons = []map[string]any{
				{
					"tag":   "button",
					"text":  map[string]any{"tag": "plain_text", "content": "✅ 允许一次"},
					"type":  "primary",
					"value": map[string]any{"action": "allow_once", "session_id": notificationID},
				},
				{
					"tag":   "button",
					"text":  map[string]any{"tag": "plain_text", "content": "🔓 会话允许"},
					"type":  "default",
					"value": map[string]any{"action": "allow_session", "session_id": notificationID},
				},
				{
					"tag":   "button",
					"text":  map[string]any{"tag": "plain_text", "content": "❌ 拒绝"},
					"type":  "danger",
					"value": map[string]any{"action": "deny", "session_id": notificationID},
				},
				{
					"tag":   "button",
					"text":  map[string]any{"tag": "plain_text", "content": "🔓 全局允许"},
					"type":  "default",
					"value": map[string]any{"action": "bypass", "session_id": notificationID},
				},
			}
			responses = map[string]ResponseEntry{
				"allow_once":    {Keys: "1", Label: "允许一次"},
				"allow_session": {Keys: "2", Label: "会话允许"},
				"deny":          {Keys: "\x1b", Label: "已拒绝"},
				"bypass":        {Keys: "2", Label: "全局允许"},
			}
		}
	default:
		buttons = nil
		responses = map[string]ResponseEntry{}
	}

	// Project name and session info.
	proj := projectName(req.Cwd)
	shortSess := req.SessionID
	if len(shortSess) > 8 {
		shortSess = shortSess[:8]
	}

	// Find the active Turn for this CC session.
	s.mu.Lock()
	var turn *Turn
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		turn = cc.CurrentTurn()
		if turn == nil {
			// Fallback: create a standalone turn if no active turn tracked.
			turn = cc.NewTurn(req.Cwd, proj, targetStr)
			if targetStr != "" {
				cc.TerminalTarget = targetStr
			}
			s.ccSessions[req.SessionID] = cc
		}
	}
	s.mu.Unlock()

	// Build prompt preview for title.
	promptPreview := ""
	if turn != nil && turn.FirstMessage != "" {
		promptPreview = turn.FirstMessage
		if len(promptPreview) > 10 {
			promptPreview = promptPreview[:10]
		}
	}

	// Title and template.
	title := "⏸️ 等待操作"
	switch req.NotificationType {
	case "permission_prompt":
		title = fmt.Sprintf("🔐 权限确认 | %s", proj)
	case "idle_prompt":
		title = fmt.Sprintf("💬 等待输入 | %s", proj)
	case "elicitation_dialog":
		title = fmt.Sprintf("📋 等待操作 | %s", proj)
	}
	if proj == "" {
		switch req.NotificationType {
		case "permission_prompt":
			title = "🔐 权限确认"
		case "idle_prompt":
			title = "💬 等待输入"
		case "elicitation_dialog":
			title = "📋 等待操作"
		}
	}
	if promptPreview != "" {
		title = title + " | " + promptPreview
	}

	// Prepend session info to card content.
	if shortSess != "" || req.Cwd != "" {
		sessionHeader := ""
		if shortSess != "" {
			sessionHeader += fmt.Sprintf("**Session**: `%s`  ", shortSess)
		}
		if req.Cwd != "" {
			sessionHeader += fmt.Sprintf("**路径**: `%s`", req.Cwd)
		}
		cardMsg = sessionHeader + "\n\n---\n\n" + cardMsg
	}

	// Send card.
	var msgID string
	var sendErr error

	needInput := req.NotificationType == "idle_prompt" || req.NotificationType == "elicitation_dialog"

	if turn.ThreadMsgID == "" {
		msgID, sendErr = s.lark.SendCard(title, cardMsg, notificationID, buttons, needInput)
	} else {
		msgID, sendErr = s.lark.ReplyCard(turn.ThreadMsgID, title, cardMsg, notificationID, buttons, needInput)
	}
	if sendErr != nil {
		s.log.Error("Failed to send Lark card: %v", sendErr)
		return
	}

	if turn.ThreadMsgID == "" {
		s.mu.Lock()
		turn.ThreadMsgID = msgID
		s.mu.Unlock()
	}

	// Create Loop for this notification.
	loopID := req.ToolUseID
	if loopID == "" {
		loopID = genID()
	}
	loop := Loop{
		LoopID:           loopID,
		Type:             LoopType(req.NotificationType),
		ToolName:         req.ToolName,
		Params:           req.Params,
		Status:           "pending",
		CardMsgID:        msgID,
		NotificationID:   notificationID,
		Responses:        responses,
		NotificationType: req.NotificationType,
		CreatedAt:        time.Now(),
	}

	s.mu.Lock()
	turn.Loops = append(turn.Loops, loop)
	s.activeLoops[notificationID] = &ActiveLoop{Loop: &turn.Loops[len(turn.Loops)-1], Turn: turn}
	if req.SessionID != "" {
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.HookPID = req.HookPid
		cc.AIPid = getAIPid(req.HookPid)
		cc.LastActivity = time.Now()
		if targetStr != "" {
			cc.TerminalTarget = targetStr
		}
		cc.Cwd = req.Cwd
		cc.Service = req.Service
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	s.log.Info("Notification loop created: notification=%s turn=%s loop=%s tmux=%s msg_id=%s type=%s", notificationID, turn.TurnID, loopID, targetStr, msgID, req.NotificationType)
	s.updateSleepState()
}

func (s *Server) handlePreToolUse(conn *websocket.Conn, req *HookRequest) {
	// PreToolUse kept for backward compatibility, but now it just denies
	// since we use Notification hook for interactive confirmations.
	s.log.Info("PreToolUse received for %s — denying (use Notification hook instead)", req.ToolName)
	conn.WriteJSON(HookResponse{Result: "deny", Error: "PreToolUse not supported; use Notification hook"})
}

func (s *Server) handlePostToolUse(_ *websocket.Conn, req *HookRequest) {
	s.mu.RLock()
	var threadID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		if turn := cc.CurrentTurn(); turn != nil {
			threadID = turn.ThreadMsgID
		}
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
		found := false
		if cc, ok := s.ccSessions[req.SessionID]; ok {
			if turn := cc.CurrentTurn(); turn != nil {
				for i := range turn.Loops {
					if turn.Loops[i].LoopID == req.ToolUseID {
						turn.Loops[i].Result = req.Result
						turn.Loops[i].Summary = extractToolResult(req)
						turn.Loops[i].Status = "completed"
						found = true
						break
					}
				}
				if !found {
					turn.Loops = append(turn.Loops, Loop{
						LoopID:   req.ToolUseID,
						Type:     LoopTypePermissionPrompt,
						ToolName: req.ToolName,
						Result:   req.Result,
						Summary:  extractToolResult(req),
						Status:   "completed",
					})
				}
			}
		}
		s.mu.Unlock()
		s.log.Info("PostToolUse: tool=%s session=%s", req.ToolName, req.SessionID)
	}
}

func (s *Server) handleStop(_ *websocket.Conn, req *HookRequest) {
	s.removeWorkingReaction()

	var b strings.Builder
	b.WriteString("✅ 本轮对话已结束")
	if req.Message != "" {
		b.WriteString("\n\n" + req.Message)
	}

	// Summarize accumulated loops for the turn.
	s.mu.Lock()
	var threadID string
	var reactionMsgID, reactionID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		if turn := cc.CurrentTurn(); turn != nil {
			if len(turn.Loops) > 0 {
				b.WriteString("\n\n**执行结果汇总：**\n")
				for _, loop := range turn.Loops {
					icon := "⏸️"
					if loop.Status == "completed" || loop.Status == "allowed" {
						icon = "✅"
					} else if loop.Status == "denied" {
						icon = "❌"
					}
					if loop.Summary == "" {
						b.WriteString(fmt.Sprintf("\n- %s `%s` (%s)", icon, loop.ToolName, loop.Status))
					} else {
						b.WriteString(fmt.Sprintf("\n- %s `%s` (%s)：\n%s", icon, loop.ToolName, loop.Status, loop.Summary))
					}
				}
			}
			threadID = turn.ThreadMsgID
			reactionMsgID = turn.ThreadMsgID
			reactionID = turn.WorkingReactionID
			turn.WorkingReactionID = ""
			cc.EndCurrentTurn("completed")
		}
	}
	s.mu.Unlock()

	// 撤回 turn 开始时的 reaction。
	if reactionMsgID != "" && reactionID != "" {
		if err := s.lark.DeleteReaction(reactionMsgID, reactionID); err != nil {
			s.log.Error("Failed to delete turn working reaction: %v", err)
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

	// Update CC session activity; mark as inactive since Claude Code stopped.
	s.mu.Lock()
	if req.SessionID != "" {
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.HookPID = req.HookPid
		cc.AIPid = getAIPid(req.HookPid)
		cc.LastActivity = time.Now()
		cc.Active = false
		cc.Cwd = req.Cwd
		cc.Service = req.Service
		if targetStr != "" {
			cc.TerminalTarget = targetStr
		}
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	s.log.Info("Stop handled: turn ended, session=%s tmux=%s", req.SessionID, targetStr)
	s.updateSleepState()
}

func (s *Server) handleStopFailure(_ *websocket.Conn, req *HookRequest) {
	s.removeWorkingReaction()

	s.mu.Lock()
	var threadID string
	if cc, ok := s.ccSessions[req.SessionID]; ok {
		if turn := cc.EndCurrentTurn("failed"); turn != nil {
			threadID = turn.ThreadMsgID
		}
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

	// Update CC session activity; mark as inactive since Claude Code stopped.
	s.mu.Lock()
	if req.SessionID != "" {
		cc, exists := s.ccSessions[req.SessionID]
		if !exists {
			cc = &CCSession{SessionID: req.SessionID}
		}
		cc.HookPID = req.HookPid
		cc.AIPid = getAIPid(req.HookPid)
		cc.LastActivity = time.Now()
		cc.Active = false
		cc.Cwd = req.Cwd
		cc.Service = req.Service
		if targetStr != "" {
			cc.TerminalTarget = targetStr
		}
		s.ccSessions[req.SessionID] = cc
	}
	s.mu.Unlock()

	s.log.Info("StopFailure handled: error=%s, session=%s tmux=%s", req.NotificationType, req.SessionID, targetStr)
	s.updateSleepState()
}

func (s *Server) handleSessionStart(_ *websocket.Conn, req *HookRequest) {
	s.mu.Lock()
	cc, exists := s.ccSessions[req.SessionID]
	if !exists {
		cc = &CCSession{SessionID: req.SessionID}
	}
	cc.LastActivity = time.Now()
	cc.Active = true
	cc.Cwd = req.Cwd
	cc.Service = req.Service
	if req.HookPid > 0 {
		cc.HookPID = req.HookPid
		cc.AIPid = getAIPid(req.HookPid)
	}

	// Resolve terminal target.
	var targetStr string
	if req.TmuxTarget != "" {
		targetStr = "tmux:" + req.TmuxTarget
	} else if req.HookPid > 0 {
		if target, err := s.terminal.ResolveTarget(req.HookPid); err == nil && target != nil {
			targetStr = target.String()
		}
	}
	if targetStr != "" {
		cc.TerminalTarget = targetStr
	}

	// If there is an unfinished turn, mark it as failed.
	cc.EndCurrentTurn("failed")

	s.ccSessions[req.SessionID] = cc
	s.mu.Unlock()

	proj := projectName(req.Cwd)
	clientName := "Claude"
	switch req.Service {
	case "kimi":
		clientName = "Kimi"
	case "trae":
		clientName = "Trae"
	}
	msg := fmt.Sprintf("🚀 新会话启动 | **%s** | `%s`", clientName, proj)
	if req.Cwd != "" {
		msg += fmt.Sprintf("\n路径: `%s`", req.Cwd)
	}
	startMsgID, _ := s.lark.SendMarkdown("", msg)
	if startMsgID != "" {
		s.mu.Lock()
		if cc2, ok := s.ccSessions[req.SessionID]; ok {
			cc2.StartMsgID = startMsgID
			s.ccSessions[req.SessionID] = cc2
		}
		s.mu.Unlock()
	}

	s.log.Info("SessionStart handled: session=%s proj=%s tmux=%s start_msg_id=%s", req.SessionID, proj, targetStr, startMsgID)
	s.updateSleepState()
}

func (s *Server) handlePromptSubmit(_ *websocket.Conn, req *HookRequest) {
	// Always resolve terminal target so pane changes are picked up.
	var targetStr string
	if req.TmuxTarget != "" {
		targetStr = "tmux:" + req.TmuxTarget
	} else if req.HookPid > 0 {
		if target, err := s.terminal.ResolveTarget(req.HookPid); err == nil && target != nil {
			targetStr = target.String()
		}
	}

	s.mu.Lock()
	cc, exists := s.ccSessions[req.SessionID]
	if !exists {
		cc = &CCSession{SessionID: req.SessionID}
	}
	cc.LastActivity = time.Now()
	cc.Active = true
	cc.Cwd = req.Cwd
	cc.Service = req.Service
	if req.HookPid > 0 {
		cc.HookPID = req.HookPid
		cc.AIPid = getAIPid(req.HookPid)
	}
	if targetStr != "" {
		cc.TerminalTarget = targetStr
	}

	// If there is an unfinished turn from a previous prompt, mark it as completed.
	if cur := cc.CurrentTurn(); cur != nil && cur.Status == "running" {
		cc.EndCurrentTurn("completed")
	}

	// Create a new Turn for this prompt.
	turn := cc.NewTurn(req.Cwd, projectName(req.Cwd), targetStr)
	turn.FirstMessage = req.Message

	s.ccSessions[req.SessionID] = cc
	s.mu.Unlock()

	// Sync prompt to Lark at the start of each turn.
	proj := turn.ProjectName
	if proj == "" {
		proj = projectName(req.Cwd)
	}
	var title, content string
	if req.Message != "" {
		preview := req.Message
		if len(preview) > 10 {
			preview = preview[:10]
		}
		title = fmt.Sprintf("🚀 %s | %s", proj, preview)
		if proj == "" {
			title = fmt.Sprintf("🚀 %s", preview)
		}
		content = fmt.Sprintf("**Prompt:**\n%s", req.Message)
	} else {
		title = fmt.Sprintf("🚀 %s | 新一轮工作开始", proj)
		if proj == "" {
			title = "🚀 新一轮工作开始"
		}
		content = fmt.Sprintf("**Turn:** %s\n**目录:** %s\n**时间:** %s",
			turn.TurnID,
			turn.Cwd,
			turn.StartTime.Format("15:04:05"),
		)
	}

	// Use session start message as thread root if available.
	if turn.ThreadMsgID == "" && cc.StartMsgID != "" {
		turn.ThreadMsgID = cc.StartMsgID
	}

	var msgID string
	var err error
	if turn.ThreadMsgID == "" {
		msgID, err = s.lark.SendMarkdown(title, content)
	} else {
		err = s.lark.ReplyMarkdown(turn.ThreadMsgID, title, content)
	}
	if err != nil {
		s.log.Error("Failed to send prompt to Lark: %v", err)
	} else if msgID != "" {
		s.mu.Lock()
		turn.ThreadMsgID = msgID
		s.mu.Unlock()

		// 随机选一个"在干活" reaction
		workEmojis := []string{"OnIt", "OneSecond", "StatusReading", "Status_PrivateMessage"}
		emoji := workEmojis[rand.Intn(len(workEmojis))]
		if reactionID, rerr := s.lark.AddReaction(msgID, emoji); rerr == nil {
			s.mu.Lock()
			turn.WorkingReactionID = reactionID
			s.mu.Unlock()
		} else {
			s.log.Error("Failed to add working reaction to turn start: %v", rerr)
		}
	}

	s.log.Info("PromptSubmit handled: session=%s turn=%s", req.SessionID, turn.TurnID)
	s.updateSleepState()
}

func (s *Server) handleCardAction(payload map[string]any) {
	s.log.Info("handleCardAction called: payload keys=%v", func() []string {
		var keys []string
		for k := range payload {
			keys = append(keys, k)
		}
		return keys
	}())

	// WebSocket format: payload directly contains action.
	action, _ := payload["action"].(map[string]any)
	if action == nil {
		// Fallback: HTTP webhook format with event wrapper.
		event, _ := payload["event"].(map[string]any)
		action, _ = event["action"].(map[string]any)
	}
	if action == nil {
		s.log.Info("handleCardAction: no action found in payload")
		return
	}
	value, _ := action["value"].(map[string]any)
	if value == nil {
		return
	}

	actionType, _ := value["action"].(string)
	notificationID, _ := value["session_id"].(string)

	if notificationID == "" || actionType == "" {
		return
	}

	s.mu.Lock()
	al, ok := s.activeLoops[notificationID]
	if !ok {
		s.mu.Unlock()
		return
	}

	// Parse input value if present.
	inputValue := ""
	if iv, ok := action["input_value"].(string); ok {
		inputValue = iv
	}

	s.mu.Unlock()

	s.log.Info("Card action: notification=%s action=%s input=%q", notificationID, actionType, inputValue)

	// Resolve target from Turn.
	var targetStr string
	if al.Turn != nil && al.Turn.TerminalTarget != "" {
		targetStr = al.Turn.TerminalTarget
	}
	if targetStr == "" {
		s.log.Error("No terminal target for notification %s", notificationID)
		var threadID string
		if al.Turn != nil {
			threadID = al.Turn.ThreadMsgID
		}
		msg := fmt.Sprintf("⚠️ Notification `%s` 没有可注入的终端目标，请直接在 Claude Code 终端中操作", notificationID)
		if threadID != "" {
			_ = s.lark.ReplyMarkdown(threadID, "", msg)
		} else {
			_, _ = s.lark.SendMarkdown("", msg)
		}
		return
	}
	target := s.parseTarget(targetStr)
	if target == nil {
		s.log.Error("Failed to parse terminal target: %s", targetStr)
		return
	}

	// Update loop status.
	updateLoopStatus := func(status string) {
		if al.Loop == nil {
			return
		}
		s.mu.Lock()
		al.Loop.Status = status
		s.mu.Unlock()
	}

	// Handle bypass (global allow).
	if actionType == "bypass" {
		resp, ok := al.Loop.Responses["bypass"]
		if ok {
			_ = s.terminal.InjectKeys(target, resp.Keys)
		}
		s.allowedDevices[targetStr] = true
		s.updateCardStatus(al.Loop, al.Turn, "allow_session")
		updateLoopStatus("allowed")
		s.log.Info("Global allow enabled for %s", targetStr)
		// Clean up active loop.
		s.mu.Lock()
		delete(s.activeLoops, notificationID)
		s.mu.Unlock()
		s.updateSleepState()
		return
	}

	// Handle text input.
	if inputValue != "" {
		if al.Loop.NotificationType == "permission_prompt" {
			// Permission prompts expect a raw keypress without Enter.
			_ = s.terminal.InjectKeys(target, inputValue)
		} else {
			_ = s.terminal.InjectText(target, inputValue)
		}
		s.updateCardStatus(al.Loop, al.Turn, "allowed")
		updateLoopStatus("allowed")
		// Clean up active loop.
		s.mu.Lock()
		delete(s.activeLoops, notificationID)
		s.mu.Unlock()
		s.updateSleepState()
		return
	}

	// Handle button click.
	resp, ok := al.Loop.Responses[actionType]
	if !ok {
		s.log.Error("Unknown action type %s for notification %s", actionType, notificationID)
		return
	}

	_ = s.terminal.InjectKeys(target, resp.Keys)
	s.updateCardStatus(al.Loop, al.Turn, actionType)
	s.log.Info("Injected keys to %s: %s", targetStr, resp.Label)

	if actionType == "deny" {
		updateLoopStatus("denied")
	} else {
		updateLoopStatus("allowed")
	}
	// Clean up active loop regardless of outcome.
	s.mu.Lock()
	delete(s.activeLoops, notificationID)
	s.mu.Unlock()
	s.updateSleepState()
}

func (s *Server) updateCardStatus(loop *Loop, turn *Turn, status string) {
	proj := ""
	if turn != nil {
		proj = turn.ProjectName
	}
	if proj == "" {
		proj = projectName("")
	}
	title := fmt.Sprintf("🖥️ %s | %s", proj, loop.ToolName)
	if loop.ToolName == "" {
		title = fmt.Sprintf("🖥️ %s", proj)
	}
	content := buildCardContentFromLoop(loop)
	if err := s.lark.UpdateCard(loop.CardMsgID, title, content, status); err != nil {
		s.log.Error("Failed to update card: %v", err)
	}
}

func (s *Server) addWorkingReaction(msgID string) {
	if msgID == "" {
		return
	}
	reactionID, err := s.lark.AddReaction(msgID, s.cfg.Lark.WorkingEmoji)
	if err != nil {
		s.log.Error("Failed to add working reaction: %v", err)
		return
	}
	s.mu.Lock()
	s.pendingReaction.MessageID = msgID
	s.pendingReaction.ReactionID = reactionID
	s.mu.Unlock()
}

func (s *Server) removeWorkingReaction() {
	s.mu.Lock()
	msgID := s.pendingReaction.MessageID
	reactionID := s.pendingReaction.ReactionID
	s.pendingReaction.MessageID = ""
	s.pendingReaction.ReactionID = ""
	s.mu.Unlock()

	if msgID == "" || reactionID == "" {
		return
	}
	if err := s.lark.DeleteReaction(msgID, reactionID); err != nil {
		s.log.Error("Failed to delete working reaction: %v", err)
	}
}

func (s *Server) parseTarget(targetStr string) *terminal.Target {
	if targetStr == "" {
		return nil
	}
	parts := make([]string, 0, 2)
	for i := 0; i < len(targetStr); i++ {
		if targetStr[i] == ':' {
			parts = append(parts, targetStr[:i])
			if i+1 < len(targetStr) {
				parts = append(parts, targetStr[i+1:])
			}
			break
		}
	}
	if len(parts) == 2 {
		return &terminal.Target{Type: parts[0], Target: parts[1]}
	}
	// Fallback: assume tmux if no prefix.
	return &terminal.Target{Type: "tmux", Target: targetStr}
}

func (s *Server) handleIMMessage(payload map[string]any) {
	// WebSocket format: payload directly contains message.
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		// Fallback: HTTP webhook format with event wrapper.
		event, _ := payload["event"].(map[string]any)
		message, _ = event["message"].(map[string]any)
	}
	if message == nil {
		return
	}
	contentStr, _ := message["content"].(string)

	var content map[string]any
	if err := json.Unmarshal([]byte(contentStr), &content); err != nil {
		s.log.Error("Failed to unmarshal IM message content: %v", err)
		return
	}
	text, _ := content["text"].(string)
	if text == "" {
		return
	}

	// If the message @mentions someone, only process if we are among them.
	if mentions, ok := message["mentions"].([]any); ok && len(mentions) > 0 {
		mentioned := false
		s.mu.RLock()
		botID := s.botOpenID
		s.mu.RUnlock()
		for _, m := range mentions {
			if mm, ok := m.(map[string]any); ok {
				// Prefer exact open_id match; fallback to mentioned_type if not yet known.
				if botID != "" {
					if openID, _ := mm["open_id"].(string); openID == botID {
						mentioned = true
						break
					}
				} else {
					if mtype, _ := mm["mentioned_type"].(string); mtype == "bot" {
						mentioned = true
						break
					}
				}
			}
		}
		if !mentioned {
			s.log.Debug("Ignoring IM message that @mentions others but not us")
			return
		}
	}

	// Strip Lark @mention prefix (e.g. "@_user_1 message" -> "message").
	text = stripAtMention(text)
	text = strings.TrimSpace(text)

	// Handle slash commands.
	if strings.HasPrefix(text, "/") {
		parts := strings.Fields(text)
		cmd := parts[0]
		switch cmd {
		case "/session":
			s.handleSessionCommand(message)
		default:
			msgID, _ := message["message_id"].(string)
			_ = s.lark.ReplyMarkdown(msgID, "", fmt.Sprintf("未知命令: `%s`", cmd))
		}
		return
	}

	// Find the most recently active CC session to forward message to.
	s.mu.RLock()
	var ccTarget *CCSession
	for _, cc := range s.ccSessions {
		if !cc.Active {
			continue
		}
		if ccTarget == nil || cc.LastActivity.After(ccTarget.LastActivity) {
			ccTarget = cc
		}
	}
	s.mu.RUnlock()

	var targetStr string
	if ccTarget != nil && ccTarget.TerminalTarget != "" {
		targetStr = ccTarget.TerminalTarget
	}

	if targetStr != "" {
		t := s.parseTarget(targetStr)
		if t != nil {
			if err := s.terminal.InjectText(t, text); err != nil {
				s.log.Error("Failed to inject text to %s: %v", targetStr, err)
			} else {
				s.log.Info("Forwarded message to %s: %s", targetStr, text)
			}
		}
	}

	// Add working reaction to the incoming message.
	msgID, _ := message["message_id"].(string)
	s.addWorkingReaction(msgID)
}

func (s *Server) handleSessionCommand(message map[string]any) {
	s.mu.RLock()

	headers := []string{"threadMsgID", "仓库名称", "Session", "CLI", "状态"}
	var rows [][]string

	for _, cc := range s.ccSessions {
		// Latest turn is the last one in the slice.
		var latestTurn *Turn
		if len(cc.Turns) > 0 {
			latestTurn = cc.Turns[len(cc.Turns)-1]
		}

		threadID := "-"
		proj := "-"
		status := "空闲"
		if latestTurn != nil {
			threadID = latestTurn.ThreadMsgID
			if threadID == "" {
				threadID = cc.StartMsgID
			}
			proj = latestTurn.ProjectName
			if proj == "" {
				proj = projectName(latestTurn.Cwd)
			}
			status = string(latestTurn.Status)
			if status == "running" {
				// Check if there are pending loops.
				hasPending := false
				for _, loop := range latestTurn.Loops {
					if loop.Status == "pending" {
						hasPending = true
						break
					}
				}
				if hasPending {
					status = "等待确认"
				} else {
					status = "执行中"
				}
			}
		} else {
			// No turns yet: derive project name from session-level Cwd.
			threadID = cc.StartMsgID
			proj = projectName(cc.Cwd)
			if proj == "" {
				proj = "-"
			}
			if cc.Active {
				status = "运行中"
			}
		}
		if threadID == "" {
			threadID = "-"
		}

		shortSess := cc.SessionID
		if len(shortSess) > 8 {
			shortSess = shortSess[:8]
		}

		cli := cc.Service
		switch cli {
		case "cc":
			cli = "Claude"
		case "kimi":
			cli = "Kimi"
		}
		if cli == "" {
			cli = "-"
		}

		rows = append(rows, []string{threadID, proj, shortSess, cli, status})
	}

	s.mu.RUnlock()

	msgID, _ := message["message_id"].(string)
	if len(rows) == 0 {
		if err := s.lark.ReplyMarkdown(msgID, "Session 列表", "**当前 Session 列表**\n\n无活跃 Session"); err != nil {
			s.log.Error("Failed to reply session list: %v", err)
		}
		return
	}

	if err := s.lark.ReplyPostTable(msgID, "Session 列表", headers, rows); err != nil {
		s.log.Error("Failed to reply session list: %v", err)
	}
}

func buildCardContentFromLoop(loop *Loop) string {
	var b strings.Builder

	if loop.ToolName == "" {
		b.WriteString("**状态:** 已操作")
	} else if len(loop.Params) == 0 {
		b.WriteString(fmt.Sprintf("**工具:** `%s`\n\n**参数:** 无", loop.ToolName))
	} else {
		paramsJSON, _ := json.MarshalIndent(loop.Params, "", "  ")
		b.WriteString(fmt.Sprintf("**工具:** `%s`\n\n**参数:**\n```json\n%s\n```", loop.ToolName, string(paramsJSON)))
	}
	return b.String()
}

func extractToolResult(req *HookRequest) string {
	switch req.ToolName {
	case "Read":
		content, _ := req.Result["content"].(string)
		if content != "" {
			if len(content) > 2000 {
				return content[:2000] + "\n... (内容已截断)"
			}
			return content
		}
	case "Bash":
		output, _ := req.Result["output"].(string)
		if output != "" {
			if len(output) > 2000 {
				return output[:2000] + "\n... (内容已截断)"
			}
			return output
		}
		stderr, _ := req.Result["stderr"].(string)
		if stderr != "" {
			if len(stderr) > 2000 {
				return stderr[:2000] + "\n... (内容已截断)"
			}
			return stderr
		}
	case "Edit":
		return "文件修改已应用"
	case "Write":
		return "文件已写入"
	}

	if len(req.Result) > 0 {
		resultJSON, _ := json.MarshalIndent(req.Result, "", "  ")
		return "```json\n" + string(resultJSON) + "\n```"
	}
	return ""
}

func formatPostResult(req *HookRequest) string {
	content := extractToolResult(req)
	if content == "" {
		return fmt.Sprintf("✅ `%s` 执行完成", req.ToolName)
	}
	return fmt.Sprintf("✅ `%s`:\n%s", req.ToolName, content)
}

func formatToolDesc(req *HookRequest) string {
	if req.ToolName == "" {
		return req.Message
	}

	var b strings.Builder

	switch req.ToolName {
	case "Bash":
		cmd, _ := req.ToolInput["command"].(string)
		if cmd == "" {
			cmd, _ = req.Params["command"].(string)
		}
		if cmd != "" {
			desc, _ := req.ToolInput["description"].(string)
			if desc == "" {
				desc, _ = req.Params["description"].(string)
			}
			b.WriteString(fmt.Sprintf("⚡ **Bash**\n```\n%s\n```", cmd))
			if desc != "" {
				b.WriteString("\n" + desc)
			}
		}
	case "Read", "Edit", "Write":
		fp, _ := req.ToolInput["file_path"].(string)
		if fp == "" {
			fp, _ = req.Params["file_path"].(string)
		}
		if fp != "" {
			icons := map[string]string{"Read": "📖", "Edit": "✏️", "Write": "📝"}
			b.WriteString(fmt.Sprintf("%s **%s**: `%s`", icons[req.ToolName], req.ToolName, fp))
		}
	default:
		b.WriteString(fmt.Sprintf("🔧 **%s**", req.ToolName))
	}

	// Append full parameters for all tool types.
	params := req.ToolInput
	if len(params) == 0 {
		params = req.Params
	}
	if len(params) > 0 {
		paramsJSON, _ := json.MarshalIndent(params, "", "  ")
		b.WriteString(fmt.Sprintf("\n\n**参数：**\n```json\n%s\n```", string(paramsJSON)))
	}

	// Also include the original message so nothing is lost.
	if req.Message != "" && req.Message != b.String() {
		b.WriteString("\n\n---\n\n" + req.Message)
	}

	return b.String()
}

// Option represents a parsed TUI option.
type Option struct {
	Num      int
	Text     string
	Selected bool // true if prefixed with '>' (dropdown current selection)
}

func parseOptionsFromMessage(msg string) []Option {
	var opts []Option
	lines := strings.Split(msg, "\n")

	// First pass: detect dropdown-style options prefixed with '> '.
	var dropdownOpts []Option
	foundDropdown := false
	for _, line := range lines {
		trimmedRight := strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(trimmedRight, "> ") {
			foundDropdown = true
			text := strings.TrimSpace(trimmedRight[2:])
			if text != "" {
				dropdownOpts = append(dropdownOpts, Option{Num: len(dropdownOpts) + 1, Text: text, Selected: true})
			}
		} else if foundDropdown {
			// After seeing a '> ' line, treat nearby indented lines as options too.
			// Stop if we hit a blank line or a line that is not indented.
			if strings.TrimSpace(trimmedRight) == "" {
				break
			}
			// Must be indented (starts with spaces or tab) to be considered an option.
			if !strings.HasPrefix(trimmedRight, "  ") && !strings.HasPrefix(trimmedRight, "\t") {
				break
			}
			text := strings.TrimSpace(trimmedRight)
			if text != "" {
				dropdownOpts = append(dropdownOpts, Option{Num: len(dropdownOpts) + 1, Text: text, Selected: false})
			}
		}
	}
	if len(dropdownOpts) > 0 {
		return dropdownOpts
	}

	// Fallback: numbered options like "1. Option" or "1) Option".
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for i := 0; i < len(line); i++ {
			if line[i] >= '0' && line[i] <= '9' {
				j := i
				for j < len(line) && line[j] >= '0' && line[j] <= '9' {
					j++
				}
				numStr := line[i:j]
				numVal, _ := strconv.Atoi(numStr)
				if j < len(line) && (line[j] == '.' || line[j] == ')') {
					rest := strings.TrimSpace(line[j+1:])
					if rest != "" {
						opts = append(opts, Option{Num: numVal, Text: rest})
					}
				}
				break
			}
		}
	}
	return opts
}

func formatOptionsForCard(opts []Option) string {
	var b strings.Builder
	b.WriteString("**选项：**\n")
	for _, opt := range opts {
		if opt.Selected {
			b.WriteString(fmt.Sprintf("> **%s**\n", opt.Text))
		} else {
			b.WriteString(fmt.Sprintf("  %s\n", opt.Text))
		}
	}
	return b.String()
}

func isPositive(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "yes") || strings.Contains(t, "allow") || strings.Contains(t, "确认") || strings.Contains(t, "允许")
}

func stripAtMention(text string) string {
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "@") {
		if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
			text = strings.TrimSpace(text[i+1:])
		} else {
			return ""
		}
	}
	return text
}

func isNegative(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "no") || strings.Contains(t, "deny") || strings.Contains(t, "拒绝") || strings.Contains(t, "取消")
}

func genID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// projectName extracts project name from cwd.
func projectName(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "/" {
		return ""
	}
	// Remove trailing slash.
	cwd = strings.TrimSuffix(cwd, "/")
	// Find last path component.
	if i := strings.LastIndex(cwd, "/"); i >= 0 {
		name := cwd[i+1:]
		if name != "" {
			return name
		}
	}
	return cwd
}
