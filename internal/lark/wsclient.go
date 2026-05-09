package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/mnhkahn/gogogo/logger"
)

// WSClient maintains a WebSocket long connection to Lark/Feishu event server.
type WSClient struct {
	appID        string
	appSecret    string
	baseURL      string
	log          *logger.Logger

	conn         *websocket.Conn
	serviceID    int32
	done         chan struct{}
	pingInterval time.Duration

	onCardAction func(map[string]any)
	onIMMessage  func(map[string]any)
}

// NewWSClient creates a new Lark WS event client.
func NewWSClient(appID, appSecret, baseURL string, log *logger.Logger) *WSClient {
	return &WSClient{
		appID:        appID,
		appSecret:    appSecret,
		baseURL:      baseURL,
		log:          log,
		done:         make(chan struct{}),
		pingInterval: 2 * time.Minute,
	}
}

// SetOnCardAction registers callback for card.action.trigger events.
func (c *WSClient) SetOnCardAction(fn func(map[string]any)) {
	c.onCardAction = fn
}

// SetOnIMMessage registers callback for im.message.receive_v1 events.
func (c *WSClient) SetOnIMMessage(fn func(map[string]any)) {
	c.onIMMessage = fn
}

// Start connects to Lark WS endpoint and begins receiving events.
// It retries indefinitely until successful or context cancelled.
func (c *WSClient) Start(ctx context.Context) error {
	for {
		if err := c.tryConnect(ctx); err != nil {
			c.log.Error("Lark WS connect failed: %v, retrying in 10s...", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.done:
				return nil
			case <-time.After(10 * time.Second):
				continue
			}
		}
		return nil
	}
}

func (c *WSClient) tryConnect(ctx context.Context) error {
	endpoint, err := c.getEndpoint(ctx)
	if err != nil {
		return err
	}
	if endpoint.Data == nil {
		return fmt.Errorf("ws endpoint data is nil")
	}

	wsURL := endpoint.Data.URL
	if endpoint.Data.ClientConfig != nil && endpoint.Data.ClientConfig.PingInterval > 0 {
		c.pingInterval = time.Duration(endpoint.Data.ClientConfig.PingInterval) * time.Second
	}

	u, err := url.Parse(wsURL)
	if err != nil {
		return fmt.Errorf("parse ws url: %w", err)
	}

	if svcStr := u.Query().Get("service_id"); svcStr != "" {
		if sid, err := strconv.ParseInt(svcStr, 10, 32); err == nil {
			c.serviceID = int32(sid)
		}
	}

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial ws: status=%d", resp.StatusCode)
		}
		return fmt.Errorf("dial ws: %w", err)
	}
	c.conn = conn
	c.log.Info("Lark WS connected: host=%s service_id=%d", u.Host, c.serviceID)

	go c.pingLoop(ctx)
	go c.receiveLoop(ctx)

	return nil
}

// Stop closes the WebSocket connection.
func (c *WSClient) Stop() {
	close(c.done)
	if c.conn != nil {
		c.conn.Close()
	}
}

type endpointResp struct {
	Code int       `json:"code"`
	Msg  string    `json:"msg"`
	Data *endpoint `json:"data"`
}

type endpoint struct {
	URL          string        `json:"URL"`
	ClientConfig *clientConfig `json:"ClientConfig"`
}

type clientConfig struct {
	ReconnectCount    int `json:"ReconnectCount"`
	ReconnectInterval int `json:"ReconnectInterval"`
	ReconnectNonce    int `json:"ReconnectNonce"`
	PingInterval      int `json:"PingInterval"`
}

func (c *WSClient) getEndpoint(ctx context.Context) (*endpointResp, error) {
	body, _ := json.Marshal(map[string]string{
		"AppID":     c.appID,
		"AppSecret": c.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/callback/ws/endpoint", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	c.log.Debug("WS endpoint response: %s", string(respBody))

	var result endpointResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("endpoint api error code=%d msg=%s", result.Code, result.Msg)
	}
	if result.Data == nil || result.Data.URL == "" {
		return nil, fmt.Errorf("endpoint api returned empty url")
	}
	return &result, nil
}

func (c *WSClient) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.sendPing(); err != nil {
				c.log.Error("WS ping failed: %v", err)
				return
			}
		}
	}
}

func (c *WSClient) sendPing() error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	ping := larkws.NewPingFrame(c.serviceID)
	data, err := ping.Marshal()
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *WSClient) receiveLoop(ctx context.Context) {
	for {
		if c.conn == nil {
			c.log.Info("Lark WS receive loop: no connection, will reconnect")
			return
		}

		c.conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.log.Error("WS read error: %v, will reconnect", err)
			c.conn.Close()
			c.conn = nil
			go c.reconnect(ctx)
			return
		}

		if err := c.handleRawMessage(data); err != nil {
			c.log.Error("WS handle message: %v", err)
		}
	}
}

func (c *WSClient) reconnect(ctx context.Context) {
	for {
		if err := c.tryConnect(ctx); err != nil {
			c.log.Error("Lark WS reconnect failed: %v, retrying in 10s...", err)
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}
		c.log.Info("Lark WS reconnected")
		return
	}
}

func (c *WSClient) handleRawMessage(data []byte) error {
	var frame larkws.Frame
	if err := frame.Unmarshal(data); err != nil {
		return fmt.Errorf("unmarshal frame: %w", err)
	}

	switch larkws.FrameType(frame.Method) {
	case larkws.FrameTypeControl:
		c.log.Debug("WS control frame received")
		return nil
	case larkws.FrameTypeData:
		return c.handleDataFrame(&frame)
	default:
		return fmt.Errorf("unknown frame method: %d", frame.Method)
	}
}

func (c *WSClient) handleDataFrame(frame *larkws.Frame) error {
	headers := larkws.Headers(frame.Headers)
	msgType := headers.GetString(larkws.HeaderType)

	c.log.Info("WS data frame type=%s payload=%s", msgType, string(frame.Payload))

	switch larkws.MessageType(msgType) {
	case larkws.MessageTypeEvent:
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return fmt.Errorf("unmarshal event: %w", err)
		}
		if c.onIMMessage != nil {
			c.onIMMessage(payload)
		}
		// Some platforms send card actions under the "event" type rather than "card".
		// Try card action callback as a fallback.
		if c.onCardAction != nil {
			c.onCardAction(payload)
		}

	case larkws.MessageTypeCard:
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return fmt.Errorf("unmarshal card: %w", err)
		}
		if c.onCardAction != nil {
			c.onCardAction(payload)
		}
	}

	// Acknowledge with response frame
	resp := larkws.NewResponseByCode(http.StatusOK)
	respBytes, _ := json.Marshal(resp)

	respFrame := &larkws.Frame{
		Method:  int32(larkws.FrameTypeData),
		Service: frame.Service,
		Headers: frame.Headers,
		Payload: respBytes,
	}
	data, err := respFrame.Marshal()
	if err != nil {
		return fmt.Errorf("marshal resp frame: %w", err)
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("write resp: %w", err)
	}
	return nil
}
