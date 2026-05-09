package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	c, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:8765/ws", nil)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	req := map[string]any{
		"id":          "test-1",
		"phase":       "pre",
		"tool_use_id": "tu-1",
		"tool_name":   "echo",
		"params":      map[string]any{"text": "hello"},
	}
	if err := c.WriteJSON(req); err != nil {
		log.Fatal(err)
	}
	fmt.Println("Sent PreToolUse request")

	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var resp map[string]any
	if err := c.ReadJSON(&resp); err != nil {
		log.Fatal(err)
	}
	pretty, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println("Received response:")
	fmt.Println(string(pretty))
}
