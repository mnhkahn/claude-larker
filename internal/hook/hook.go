package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type ServiceType string

const (
	ServiceCC   ServiceType = "cc"
	ServiceKimi ServiceType = "kimi"
	ServiceTrae ServiceType = "trae"
)

// HookRequest is sent from hook client to larker server via WebSocket.
type HookRequest struct {
	ID               string         `json:"id"`
	Phase            string         `json:"phase"`
	ToolUseID        string         `json:"tool_use_id,omitempty"`
	ToolName         string         `json:"tool_name,omitempty"`
	Params           map[string]any `json:"params,omitempty"`
	Result           map[string]any `json:"result,omitempty"`
	Message          string         `json:"message,omitempty"`
	NotificationType string         `json:"notification_type,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	Cwd              string         `json:"cwd,omitempty"`
	TranscriptPath   string         `json:"transcript_path,omitempty"`
	ToolInput        map[string]any `json:"tool_input,omitempty"`
	HookPid          int            `json:"hook_pid,omitempty"`
	TmuxTarget       string         `json:"tmux_target,omitempty"`
	Service          string         `json:"service,omitempty"`
}

// HookResponse is received from larker server via WebSocket.
type HookResponse struct {
	Result string `json:"result"` // allow, deny
	Error  string `json:"error,omitempty"`
}

func Run(phase string, overrideService string) error {
	stdinBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to read stdin: %v\n", err)
		if phase == "pre" {
			return deny(ServiceCC)
		}
		return nil
	}

	var raw map[string]any
	if err := json.Unmarshal(stdinBytes, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to parse stdin JSON: %v\n", err)
		if phase == "pre" {
			return deny(ServiceCC)
		}
		return nil
	}

	service := detectService(raw, phase)
	if overrideService != "" {
		service = ServiceType(overrideService)
	}

	switch phase {
	case "pre":
		return runPre(service, raw)
	case "post":
		return runPost(service, raw)
	case "stop":
		return runStop(service, raw)
	case "stopfailure":
		return runStopFailure(service, raw)
	case "notification":
		return runNotification(service, raw)
	case "sessionstart":
		return runSessionStart(service, raw)
	case "promptsubmit":
		fmt.Fprintf(os.Stderr, "[larker] PromptSubmit raw input: %s\n", string(stdinBytes))
		return runPromptSubmit(service, raw)
	default:
		return fmt.Errorf("unknown phase: %s", phase)
	}
}

// detectService inspects the raw JSON to determine which CLI emitted it.
func detectService(raw map[string]any, phase string) ServiceType {
	switch phase {
	case "pre", "post":
		if hasKey(raw, "tool_call_id") {
			return ServiceKimi
		}
		if hasKey(raw, "tool_use_id") {
			return ServiceCC
		}
	case "stop":
		if hasKey(raw, "last_assistant_message") {
			return ServiceCC
		}
		if hasKey(raw, "stop_hook_active") {
			return ServiceKimi
		}
	case "stopfailure":
		if hasKey(raw, "error_details") {
			return ServiceCC
		}
		if hasKey(raw, "error_message") {
			return ServiceKimi
		}
	case "notification":
		if hasKey(raw, "sink") || hasKey(raw, "body") {
			return ServiceKimi
		}
		if hasKey(raw, "message") {
			return ServiceCC
		}
	case "sessionstart":
		if hasKey(raw, "source") {
			return ServiceKimi
		}
		if hasKey(raw, "transcript_path") {
			return ServiceCC
		}
	case "promptsubmit":
		if hasKey(raw, "prompt") {
			return ServiceKimi
		}
		if hasKey(raw, "message") {
			return ServiceCC
		}
	}
	// Default to CC for backward compatibility.
	return ServiceCC
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func stringField(raw map[string]any, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

func mapField(raw map[string]any, key string) map[string]any {
	if v, ok := raw[key].(map[string]any); ok {
		return v
	}
	return nil
}

func runPre(service ServiceType, raw map[string]any) error {
	toolUseID := ""
	toolName := ""
	params := make(map[string]any)

	switch service {
	case ServiceKimi:
		toolUseID = stringField(raw, "tool_call_id")
		toolName = stringField(raw, "tool_name")
		params = mapField(raw, "tool_input")
	case ServiceCC:
		toolUseID = stringField(raw, "tool_use_id")
		toolName = stringField(raw, "tool_name")
		params = mapField(raw, "tool_parameters")
	}

	if params == nil {
		params = make(map[string]any)
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return deny(service)
	}
	defer conn.Close()

	req := HookRequest{
		ID:        genID(),
		Phase:     "pre",
		ToolUseID: toolUseID,
		ToolName:  toolName,
		Params:    params,
		Service:   string(service),
	}
	if err := conn.WriteJSON(req); err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to send request: %v\n", err)
		return deny(service)
	}

	conn.SetReadDeadline(time.Now().Add(3600 * time.Second))
	var resp HookResponse
	if err := conn.ReadJSON(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to read response: %v\n", err)
		return deny(service)
	}

	if resp.Result == "allow" {
		return allow(service)
	}
	return deny(service)
}

func runPost(service ServiceType, raw map[string]any) error {
	toolUseID := ""
	toolName := ""
	params := make(map[string]any)
	result := make(map[string]any)

	switch service {
	case ServiceKimi:
		toolUseID = stringField(raw, "tool_call_id")
		toolName = stringField(raw, "tool_name")
		params = mapField(raw, "tool_input")
		result = mapField(raw, "tool_output")
	case ServiceCC:
		toolUseID = stringField(raw, "tool_use_id")
		toolName = stringField(raw, "tool_name")
		params = mapField(raw, "tool_parameters")
		result = mapField(raw, "result")
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:        genID(),
		Phase:     "post",
		ToolUseID: toolUseID,
		ToolName:  toolName,
		Params:    params,
		Result:    result,
		Service:   string(service),
	}
	_ = conn.WriteJSON(req)
	return nil
}

func runStop(service ServiceType, raw map[string]any) error {
	sessionID := stringField(raw, "session_id")
	cwd := stringField(raw, "cwd")
	message := ""

	switch service {
	case ServiceCC:
		message = stringField(raw, "last_assistant_message")
	case ServiceKimi:
		// kimi Stop has no direct message equivalent.
		message = ""
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:         genID(),
		Phase:      "stop",
		Message:    message,
		SessionID:  sessionID,
		Cwd:        cwd,
		HookPid:    os.Getpid(),
		TmuxTarget: detectTmuxPane(),
		Service:    string(service),
	}
	_ = conn.WriteJSON(req)
	return nil
}

func runStopFailure(service ServiceType, raw map[string]any) error {
	sessionID := stringField(raw, "session_id")
	cwd := stringField(raw, "cwd")
	errorType := ""
	message := ""

	switch service {
	case ServiceKimi:
		errorType = stringField(raw, "error_type")
		message = stringField(raw, "error_message")
	case ServiceCC:
		errorType = stringField(raw, "error")
		message = stringField(raw, "error_details")
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:               genID(),
		Phase:            "stopfailure",
		Message:          message,
		NotificationType: errorType,
		SessionID:        sessionID,
		Cwd:              cwd,
		HookPid:          os.Getpid(),
		TmuxTarget:       detectTmuxPane(),
		Service:          string(service),
	}
	_ = conn.WriteJSON(req)
	return nil
}

func runNotification(service ServiceType, raw map[string]any) error {
	notificationType := stringField(raw, "notification_type")
	sessionID := stringField(raw, "session_id")
	cwd := stringField(raw, "cwd")
	message := ""

	switch service {
	case ServiceKimi:
		title := stringField(raw, "title")
		body := stringField(raw, "body")
		message = body
		if title != "" {
			if message != "" {
				message = title + "\n\n" + message
			} else {
				message = title
			}
		}
	case ServiceCC:
		message = stringField(raw, "message")
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:               genID(),
		Phase:            "notification",
		NotificationType: notificationType,
		Message:          message,
		SessionID:        sessionID,
		Cwd:              cwd,
		HookPid:          os.Getpid(),
		TmuxTarget:       detectTmuxPane(),
		Service:          string(service),
	}
	_ = conn.WriteJSON(req)
	// Notification hook is non-blocking; exit immediately after sending.
	return nil
}

func runSessionStart(service ServiceType, raw map[string]any) error {
	sessionID := stringField(raw, "session_id")
	cwd := stringField(raw, "cwd")
	source := ""

	switch service {
	case ServiceKimi:
		source = stringField(raw, "source")
	case ServiceCC:
		// CC has no source field.
		source = ""
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:         genID(),
		Phase:      "sessionstart",
		SessionID:  sessionID,
		Cwd:        cwd,
		Message:    source,
		HookPid:    os.Getpid(),
		TmuxTarget: detectTmuxPane(),
		Service:    string(service),
	}
	_ = conn.WriteJSON(req)
	return nil
}

func runPromptSubmit(service ServiceType, raw map[string]any) error {
	sessionID := stringField(raw, "session_id")
	cwd := stringField(raw, "cwd")
	prompt := ""

	switch service {
	case ServiceKimi:
		prompt = stringField(raw, "prompt")
	case ServiceCC:
		prompt = stringField(raw, "message")
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[larker] failed to connect to server: %v\n", err)
		return nil
	}
	defer conn.Close()

	req := HookRequest{
		ID:         genID(),
		Phase:      "promptsubmit",
		SessionID:  sessionID,
		Cwd:        cwd,
		Message:    prompt,
		HookPid:    os.Getpid(),
		TmuxTarget: detectTmuxPane(),
		Service:    string(service),
	}
	_ = conn.WriteJSON(req)
	return nil
}

func allow(service ServiceType) error {
	switch service {
	case ServiceCC:
		json.NewEncoder(os.Stdout).Encode(map[string]string{
			"permissionDecision": "allow",
		})
		return nil
	case ServiceKimi:
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"hookSpecificOutput": map[string]any{
				"permissionDecision": "allow",
			},
		})
		return nil
	}
	return nil
}

func deny(service ServiceType) error {
	switch service {
	case ServiceCC:
		json.NewEncoder(os.Stdout).Encode(map[string]string{
			"permissionDecision": "deny",
		})
		return fmt.Errorf("denied")
	case ServiceKimi:
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"hookSpecificOutput": map[string]any{
				"permissionDecision":       "deny",
				"permissionDecisionReason": "denied by larker",
			},
		})
		return nil
	}
	return nil
}

// detectTmuxPane tries to find the current tmux pane from the hook process.
// This works because the hook inherits TMUX env from the CLI running inside tmux.
func detectTmuxPane() string {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		// Fallback to common install locations.
		candidates := []string{
			"/opt/homebrew/bin/tmux",
			"/usr/local/bin/tmux",
			"/usr/bin/tmux",
		}
		for _, p := range candidates {
			if _, stErr := os.Stat(p); stErr == nil {
				tmuxBin = p
				break
			}
		}
		if tmuxBin == "" {
			fmt.Fprintf(os.Stderr, "[larker] detectTmuxPane: tmux not found in PATH or common locations, PATH=%s\n", os.Getenv("PATH"))
			return ""
		}
	}
	cmd := exec.Command(tmuxBin, "display-message", "-p", "#{session_name}:#{window_index}.#{pane_index}")
	// Inherit TMUX from parent process (the CLI in tmux).
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(os.Stderr, "[larker] detectTmuxPane: tmux display-message failed: %v, stderr: %s, TMUX=%s\n", err, string(exitErr.Stderr), os.Getenv("TMUX"))
		} else {
			fmt.Fprintf(os.Stderr, "[larker] detectTmuxPane: tmux display-message failed: %v, TMUX=%s\n", err, os.Getenv("TMUX"))
		}
		return ""
	}
	pane := strings.TrimSpace(string(out))
	fmt.Fprintf(os.Stderr, "[larker] detectTmuxPane: detected pane=%s TMUX=%s\n", pane, os.Getenv("TMUX"))
	return pane
}

func genID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
