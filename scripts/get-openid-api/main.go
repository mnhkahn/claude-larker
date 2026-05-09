package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	mobile := flag.String("mobile", "", "Find open_id by mobile number (with country code, e.g. +86138xxxx)")
	email := flag.String("email", "", "Find open_id by email")
	list := flag.Bool("list", false, "List joined group chats")
	flag.Parse()

	fmt.Println("=== Larker Open ID / Chat ID API Helper ===")
	fmt.Println()

	// Read config
	home, _ := os.UserHomeDir()
	configPath := home + "/.larker/config.yaml"
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot read config: %v\n", configPath)
		fmt.Println("Run ./install.sh first, or create ~/.larker/config.yaml")
		os.Exit(1)
	}

	config := parseConfig(string(data))
	if config["app_id"] == "" || config["app_secret"] == "" {
		fmt.Println("app_id or app_secret not found in config")
		os.Exit(1)
	}
	if config["base_url"] == "" {
		config["base_url"] = "https://open.feishu.cn"
	}

	fmt.Printf("Platform: %s\n", config["base_url"])
	fmt.Printf("App ID: %s\n", config["app_id"])
	fmt.Println()

	token, err := getToken(config["base_url"], config["app_id"], config["app_secret"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get token: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Token obtained successfully")
	fmt.Println()

	if *mobile != "" {
		findByMobile(config["base_url"], token, *mobile)
		return
	}
	if *email != "" {
		findByEmail(config["base_url"], token, *email)
		return
	}
	if *list {
		listChats(config["base_url"], token)
		return
	}

	// Default: list chats
	listChats(config["base_url"], token)
}

func getToken(baseURL, appID, appSecret string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     appID,
		"app_secret": appSecret,
	})
	resp, err := http.Post(baseURL+"/open-apis/auth/v3/tenant_access_token/internal", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", fmt.Errorf("API error: %s", result.Msg)
	}
	return result.TenantAccessToken, nil
}

func findByMobile(baseURL, token, mobile string) {
	reqBody, _ := json.Marshal(map[string]any{
		"mobiles":          []string{mobile},
		"include_resigned": true,
	})
	callAPI(baseURL+"/open-apis/contact/v3/users/batch_get_id", token, reqBody)
}

func findByEmail(baseURL, token, email string) {
	reqBody, _ := json.Marshal(map[string]any{
		"emails":           []string{email},
		"include_resigned": true,
	})
	callAPI(baseURL+"/open-apis/contact/v3/users/batch_get_id", token, reqBody)
}

func listChats(baseURL, token string) {
	req, _ := http.NewRequest("GET", baseURL+"/open-apis/im/v1/chats?page_size=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]any
	json.Unmarshal(body, &result)

	if code, ok := result["code"].(float64); ok && code != 0 {
		fmt.Printf("API Error: %v\n", result["msg"])
		return
	}

	data, _ := result["data"].(map[string]any)
	items, _ := data["items"].([]any)

	fmt.Printf("Found %d chats:\n", len(items))
	for _, item := range items {
		chat, _ := item.(map[string]any)
		if chat == nil {
			continue
		}
		chatID, _ := chat["chat_id"].(string)
		name, _ := chat["name"].(string)
		chatType, _ := chat["chat_type"].(string)
		fmt.Printf("  chat_id: %s  |  name: %s  |  type: %s\n", chatID, name, chatType)
	}
}

func callAPI(url, token string, body []byte) {
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	var result map[string]any
	json.Unmarshal(bodyBytes, &result)

	if code, ok := result["code"].(float64); ok && code != 0 {
		fmt.Printf("API Error: %v\n", result["msg"])
		return
	}

	data, _ := result["data"].(map[string]any)
	users, _ := data["user_list"].([]any)

	if len(users) == 0 {
		fmt.Println("No user found")
		return
	}

	for _, u := range users {
		user, _ := u.(map[string]any)
		if user == nil {
			continue
		}
		mobile, _ := user["mobile"].(string)
		email, _ := user["email"].(string)
		openID, _ := user["open_id"].(string)
		unionID, _ := user["union_id"].(string)
		userID, _ := user["user_id"].(string)

		fmt.Println("User found:")
		if mobile != "" {
			fmt.Printf("   Mobile: %s\n", mobile)
		}
		if email != "" {
			fmt.Printf("   Email: %s\n", email)
		}
		fmt.Printf("   open_id:  %s\n", openID)
		fmt.Printf("   union_id: %s\n", unionID)
		fmt.Printf("   user_id:  %s\n", userID)
	}
}

func parseConfig(content string) map[string]string {
	result := make(map[string]string)
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			result[key] = val
		}
	}
	return result
}
