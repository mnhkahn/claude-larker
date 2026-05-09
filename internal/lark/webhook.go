package lark

import "encoding/json"

// WebhookEvent represents the common structure of Lark webhook events.
type WebhookEvent struct {
	Schema string `json:"schema"`
	Header struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		CreateTime string `json:"create_time"`
		Token      string `json:"token"`
		AppID      string `json:"app_id"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

// CardActionEvent represents card.action.trigger event.
type CardActionEvent struct {
	Operator struct {
		OpenID string `json:"open_id"`
	} `json:"operator"`
	Action struct {
		Value map[string]any `json:"value"`
		Tag   string         `json:"tag"`
	} `json:"action"`
	Context struct {
		OpenMessageID string `json:"open_message_id"`
		OpenChatID    string `json:"open_chat_id"`
	} `json:"context"`
}

// MessageReceiveEvent represents im.message.receive_v1 event.
type MessageReceiveEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"` // JSON string
		ThreadID    string `json:"thread_id"`
	} `json:"message"`
}
