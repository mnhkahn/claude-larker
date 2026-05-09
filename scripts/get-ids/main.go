package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := "8766"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	fmt.Println("=== Larker ID Helper ===")
	fmt.Println()
	fmt.Println("This tool helps you find your open_id and chat_id.")
	fmt.Println()
	fmt.Println("Steps:")
	fmt.Println("  1. Go to your Lark/Feishu app console:")
	fmt.Println("     - Feishu: https://open.feishu.cn/app")
	fmt.Println("     - Lark:   https://open.larksuite.com/app")
	fmt.Println()
	fmt.Println("  2. In your app, go to: Event Subscriptions -> Request URL")
	fmt.Println("     Enter: http://YOUR_IP:" + port + "/webhook")
	fmt.Println("     (use ngrok or localtunnel if your machine has no public IP)")
	fmt.Println()
	fmt.Println("  3. Subscribe to these events:")
	fmt.Println("     - im.message.receive_v1")
	fmt.Println("     - card.action.trigger")
	fmt.Println()
	fmt.Println("  4. Send a message to your bot (direct or in a group)")
	fmt.Println("     The IDs will appear below.")
	fmt.Println()
	fmt.Println("  5. Press Ctrl+C to stop")
	fmt.Println()
	fmt.Println("Listening on http://0.0.0.0:" + port + "/webhook ...")
	fmt.Println()

	http.HandleFunc("/webhook", handleWebhook)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("[%s] %s %s\n", r.Method, r.URL.Path, r.RemoteAddr)
		w.Write([]byte("OK - use /webhook for Lark webhook"))
	})

	if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		fmt.Printf("[ERROR] Failed to decode JSON: %v\n", err)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	pretty, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println("========================================")
	fmt.Println("Webhook received:")
	fmt.Println(string(pretty))
	fmt.Println()

	// Try to extract IDs
	extractIDs(payload)
	fmt.Println("========================================")
	fmt.Println()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"msg":"ok"}`))
}

func extractIDs(payload map[string]any) {
	header, _ := payload["header"].(map[string]any)
	if header != nil {
		eventType, _ := header["event_type"].(string)
		fmt.Printf("Event type: %s\n", eventType)
	}

	event, _ := payload["event"].(map[string]any)
	if event == nil {
		fmt.Println("No event data found")
		return
	}

	// Extract sender open_id (from message event)
	sender, _ := event["sender"].(map[string]any)
	if sender != nil {
		senderID, _ := sender["sender_id"].(map[string]any)
		if senderID != nil {
			if openID, ok := senderID["open_id"].(string); ok {
				fmt.Printf("✅ Sender open_id: %s\n", openID)
			}
			if unionID, ok := senderID["union_id"].(string); ok {
				fmt.Printf("   Sender union_id: %s\n", unionID)
			}
			if userID, ok := senderID["user_id"].(string); ok {
				fmt.Printf("   Sender user_id: %s\n", userID)
			}
		}
	}

	// Extract chat_id (from message event)
	message, _ := event["message"].(map[string]any)
	if message != nil {
		if chatID, ok := message["chat_id"].(string); ok {
			fmt.Printf("✅ Chat ID (chat_id): %s\n", chatID)
		}
		if chatType, ok := message["chat_type"].(string); ok {
			fmt.Printf("   Chat type: %s\n", chatType)
		}
		if msgType, ok := message["message_type"].(string); ok {
			fmt.Printf("   Message type: %s\n", msgType)
		}
		if content, ok := message["content"].(string); ok {
			fmt.Printf("   Content: %s\n", content)
		}
	}

	// Extract operator open_id (from card action event)
	operator, _ := event["operator"].(map[string]any)
	if operator != nil {
		if openID, ok := operator["open_id"].(string); ok {
			fmt.Printf("✅ Operator open_id: %s\n", openID)
		}
	}

	// Extract context (from card action event)
	context, _ := event["context"].(map[string]any)
	if context != nil {
		if msgID, ok := context["open_message_id"].(string); ok {
			fmt.Printf("   Message ID (open_message_id): %s\n", msgID)
		}
		if chatID, ok := context["open_chat_id"].(string); ok {
			fmt.Printf("   Chat ID (open_chat_id): %s\n", chatID)
		}
	}
}
