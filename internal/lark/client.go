package lark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mnhkahn/larker/internal/config"
	"github.com/mnhkahn/gogogo/logger"
)

type Client struct {
	cfg      config.LarkConfig
	log      *logger.Logger
	http     *http.Client
	token    string
	tokenExp time.Time
}

func New(cfg config.LarkConfig, log *logger.Logger) *Client {
	return &Client{
		cfg:  cfg,
		log:  log,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) baseURL() string {
	return c.cfg.BaseURL + "/open-apis"
}

func (c *Client) tokenURL() string     { return c.baseURL() + "/auth/v3/tenant_access_token/internal" }
func (c *Client) sendMsgURL() string   { return c.baseURL() + "/im/v1/messages?receive_id_type=" + c.cfg.ReceiverType }
func (c *Client) patchMsgURL(id string) string { return c.baseURL() + "/im/v1/messages/" + id }
func (c *Client) replyMsgURL(id string) string { return c.baseURL() + "/im/v1/messages/" + id + "/reply" }
func (c *Client) botInfoURL() string          { return c.baseURL() + "/bot/v3/info" }
func (c *Client) reactionsURL(msgID string) string     { return c.baseURL() + "/im/v1/messages/" + msgID + "/reactions" }
func (c *Client) deleteReactionURL(msgID, reactionID string) string { return c.baseURL() + "/im/v1/messages/" + msgID + "/reactions/" + reactionID }

func (c *Client) ensureToken() error {
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return nil
	}

	body, _ := json.Marshal(map[string]string{
		"app_id":     c.cfg.AppID,
		"app_secret": c.cfg.AppSecret,
	})

	resp, err := c.http.Post(c.tokenURL(), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("token api error: %s", result.Msg)
	}

	c.token = result.TenantAccessToken
	c.tokenExp = time.Now().Add(time.Duration(result.Expire-60) * time.Second)
	return nil
}

func (c *Client) doRequest(method, url string, body []byte) ([]byte, error) {
	if err := c.ensureToken(); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func buildCardJSON(title, content, status string, buttons []map[string]any, needInput bool) (string, error) {
	elements := []any{
		map[string]any{
			"tag":     "markdown",
			"content": content,
		},
	}

	if len(buttons) > 0 {
		// Add text input field alongside buttons for permission prompts.
		actions := make([]any, 0, len(buttons)+1)
		for _, btn := range buttons {
			actions = append(actions, btn)
		}
		// Add input field.
		actions = append(actions, map[string]any{
			"tag": "input",
			"name": "user_input",
			"placeholder": map[string]any{"tag": "plain_text", "content": "输入指令..."},
			"width": "fill",
		})
		elements = append(elements, map[string]any{
			"tag":     "action",
			"actions": actions,
		})
	} else if needInput {
		// Standalone input field for idle prompts without buttons.
		elements = append(elements, map[string]any{
			"tag": "input",
			"name": "user_input",
			"placeholder": map[string]any{"tag": "plain_text", "content": "输入回复..."},
			"width": "fill",
		})
	} else if status != "" {
		var icon string
		switch status {
		case "allow_once":
			icon = "允许 (一次)"
		case "allow_session":
			icon = "允许 (会话)"
		case "denied":
			icon = "已拒绝"
		case "timeout":
			icon = "已超时"
		default:
			icon = status
		}
		ts := time.Now().Format("2006-01-02 15:04:05")
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": fmt.Sprintf("**%s** · %s", icon, ts),
		})
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"elements": elements,
	}

	if title != "" {
		card["header"] = map[string]any{
			"title": map[string]any{
				"tag":     "plain_text",
				"content": title,
			},
			"template": "blue",
		}
	}

	b, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *Client) SendCard(title, content, sessionID string, buttons []map[string]any, needInput bool) (string, error) {
	cardJSON, err := buildCardJSON(title, content, "", buttons, needInput)
	if err != nil {
		return "", fmt.Errorf("build card: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"receive_id": c.cfg.ReceiverID,
		"msg_type":   "interactive",
		"content":    cardJSON,
	})

	respBody, err := c.doRequest("POST", c.sendMsgURL(), reqBody)
	if err != nil {
		return "", fmt.Errorf("send card: %w", err)
	}

	var result struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode send card response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("send card api error: %s", result.Msg)
	}

	c.log.Info("Lark card sent: msg_id=%s session=%s", result.Data.MessageID, sessionID)
	return result.Data.MessageID, nil
}

func (c *Client) UpdateCard(msgID, title, content, status string) error {
	cardJSON, err := buildCardJSON(title, content, status, nil, false)
	if err != nil {
		return fmt.Errorf("build card: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"content": cardJSON,
	})

	respBody, err := c.doRequest("PATCH", c.patchMsgURL(msgID), reqBody)
	if err != nil {
		return fmt.Errorf("update card: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode update card response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("update card api error: %s", result.Msg)
	}

	c.log.Info("Lark card updated: msg_id=%s status=%s", msgID, status)
	return nil
}

func (c *Client) ReplyThread(msgID, text string) error {
	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	reqBody, _ := json.Marshal(map[string]any{
		"msg_type":        "text",
		"content":         string(contentJSON),
		"reply_in_thread": true,
	})

	respBody, err := c.doRequest("POST", c.replyMsgURL(msgID), reqBody)
	if err != nil {
		return fmt.Errorf("reply thread: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode reply response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("reply api error: %s", result.Msg)
	}

	c.log.Info("Lark reply sent: msg_id=%s", msgID)
	return nil
}

func (c *Client) ReplyMarkdown(msgID, title, markdown string) error {
	cardJSON, err := buildCardJSON(title, markdown, "", nil, false)
	if err != nil {
		return fmt.Errorf("build card: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"msg_type":        "interactive",
		"content":         cardJSON,
		"reply_in_thread": true,
	})

	respBody, err := c.doRequest("POST", c.replyMsgURL(msgID), reqBody)
	if err != nil {
		return fmt.Errorf("reply markdown: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode reply markdown response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("reply markdown api error: %s", result.Msg)
	}

	c.log.Info("Lark reply markdown sent: msg_id=%s", msgID)
	return nil
}

func (c *Client) ReplyPostTable(msgID, title string, headers []string, rows [][]string) error {
	if msgID == "" {
		return fmt.Errorf("msgID is empty")
	}

	// Compute column widths.
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for i := range widths {
		if widths[i] < 3 {
			widths[i] = 3
		}
	}

	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}

	var text strings.Builder

	// Header row.
	for i, h := range headers {
		if i > 0 {
			text.WriteString(" | ")
		}
		text.WriteString(pad(h, widths[i]))
	}
	text.WriteString("\n")

	// Separator.
	for i := range widths {
		if i > 0 {
			text.WriteString("-+-")
		}
		text.WriteString(strings.Repeat("-", widths[i]))
	}
	text.WriteString("\n")

	// Data rows.
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				text.WriteString(" | ")
			}
			if i < len(widths) {
				text.WriteString(pad(cell, widths[i]))
			} else {
				text.WriteString(cell)
			}
		}
		text.WriteString("\n")
	}

	postContent := map[string]any{
		"zh_cn": map[string]any{
			"title": title,
			"content": []any{
				[]any{
					map[string]any{
						"tag": "code_block",
						"text": text.String(),
					},
				},
			},
		},
	}

	contentJSON, err := json.Marshal(postContent)
	if err != nil {
		return fmt.Errorf("marshal post content: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"msg_type":        "post",
		"content":         string(contentJSON),
		"reply_in_thread": true,
	})

	respBody, err := c.doRequest("POST", c.replyMsgURL(msgID), reqBody)
	if err != nil {
		return fmt.Errorf("reply post table: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode reply post table response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("reply post table api error: %s", result.Msg)
	}

	c.log.Info("Lark reply post table sent: msg_id=%s", msgID)
	return nil
}

func (c *Client) ReplyCard(msgID, title, content, sessionID string, buttons []map[string]any, needInput bool) (string, error) {
	cardJSON, err := buildCardJSON(title, content, "", buttons, needInput)
	if err != nil {
		return "", fmt.Errorf("build card: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]any{
		"msg_type":        "interactive",
		"content":         cardJSON,
		"reply_in_thread": true,
	})

	respBody, err := c.doRequest("POST", c.replyMsgURL(msgID), reqBody)
	if err != nil {
		return "", fmt.Errorf("reply card: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode reply card response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("reply card api error: %s", result.Msg)
	}

	c.log.Info("Lark reply card sent: msg_id=%s session=%s", result.Data.MessageID, sessionID)
	return result.Data.MessageID, nil
}

func (c *Client) SendText(text string) (string, error) {
	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	reqBody, _ := json.Marshal(map[string]string{
		"receive_id": c.cfg.ReceiverID,
		"msg_type":   "text",
		"content":    string(contentJSON),
	})

	respBody, err := c.doRequest("POST", c.sendMsgURL(), reqBody)
	if err != nil {
		return "", fmt.Errorf("send text: %w", err)
	}

	var result struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode send text response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("send text api error: %s", result.Msg)
	}

	c.log.Info("Lark text sent: msg_id=%s", result.Data.MessageID)
	return result.Data.MessageID, nil
}

func (c *Client) SendMarkdown(title, markdown string) (string, error) {
	cardJSON, err := buildCardJSON(title, markdown, "", nil, false)
	if err != nil {
		return "", fmt.Errorf("build card: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"receive_id": c.cfg.ReceiverID,
		"msg_type":   "interactive",
		"content":    cardJSON,
	})

	respBody, err := c.doRequest("POST", c.sendMsgURL(), reqBody)
	if err != nil {
		return "", fmt.Errorf("send markdown: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode send markdown response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("send markdown api error: %s", result.Msg)
	}

	c.log.Info("Lark markdown sent: msg_id=%s", result.Data.MessageID)
	return result.Data.MessageID, nil
}

func (c *Client) AddReaction(msgID, emojiType string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"reaction_type": map[string]string{"emoji_type": emojiType},
	})
	respBody, err := c.doRequest("POST", c.reactionsURL(msgID), reqBody)
	if err != nil {
		return "", fmt.Errorf("add reaction: %w", err)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ReactionID string `json:"reaction_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode add reaction response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("add reaction api error: %s", result.Msg)
	}
	c.log.Info("Lark reaction added: msg_id=%s reaction_id=%s emoji=%s", msgID, result.Data.ReactionID, emojiType)
	return result.Data.ReactionID, nil
}

func (c *Client) DeleteReaction(msgID, reactionID string) error {
	respBody, err := c.doRequest("DELETE", c.deleteReactionURL(msgID, reactionID), nil)
	if err != nil {
		return fmt.Errorf("delete reaction: %w", err)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode delete reaction response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("delete reaction api error: %s", result.Msg)
	}
	c.log.Info("Lark reaction deleted: msg_id=%s reaction_id=%s", msgID, reactionID)
	return nil
}

func (c *Client) GetBotInfo() (openID string, err error) {
	respBody, err := c.doRequest("GET", c.botInfoURL(), nil)
	if err != nil {
		return "", fmt.Errorf("get bot info: %w", err)
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenID string `json:"open_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		c.log.Error("Bot info response unmarshal failed: body=%s err=%v", string(respBody), err)
		return "", fmt.Errorf("decode bot info response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("bot info api error: %s", result.Msg)
	}

	c.log.Debug("Bot info response: open_id=%q", result.Data.OpenID)
	return result.Data.OpenID, nil
}
