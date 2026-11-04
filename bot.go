package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sendTelegramMessage All queries to the Telegram Bot API
// must be served over HTTPS and need to be presented in this form:
// https://api.telegram.org/bot<token>/METHOD_NAME.
//
// API doc: https://core.telegram.org/bots/api#sendmessage
//
// Required parameters: chat_id, text
func sendTelegramMessage(token, chatID, text string) error {
	// 普通用户可发送的单条消息长度最大 4096 字符
	if len(text) > 4090 {
		text = text[:4090] + "..."
	}
	fmt.Println("正在通知 Telegram bot...")

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	payload := map[string]string{
		"chat_id": chatID,
		"text":    text,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram api error, status: %d", resp.StatusCode)
	}
	return nil
}
