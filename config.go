package main

// 本文件读取 config.yaml。字段与 faq-daily-checkin 的配置完全一致：
//
//	student_id: "123456"                 # 学号
//	password: "123456"                   # 密码
//	browser_fingerprint: "ABCDEFG"       # idas 的 MULTIFACTOR_BROWSER_FINGERPRINT
//	position:
//	  latitude: 12.123456
//	  longitude: 123.123456
//	mmwebid: 578                         # 微信 WebView 实例号，缺省随机
//
// 学号/密码/指纹用于 CAS 登录换 SESSION；position 用于打卡；mmwebid 用于伪造微信 UA。

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是 config.yaml 的映射，字段名与 faq-daily-checkin 保持一致。
type Config struct {
	StudentID          string `yaml:"student_id"`
	Password           string `yaml:"password"`
	BrowserFingerprint string `yaml:"browser_fingerprint"`
	MMWebID            int    `yaml:"mmwebid"`
	Position           struct {
		Latitude  float64 `yaml:"latitude"`
		Longitude float64 `yaml:"longitude"`
	} `yaml:"position"`
	TelegramBot TelegramBotConfig `yaml:"telegram_bot"`
}

const (
	defaultTelegramToken  = "YOUR_TOKEN"
	defaultTelegramChatID = "YOUR_CHAT_ID"
)

type TelegramBotConfig struct {
	Token  string `yaml:"token"`
	ChatID string `yaml:"chat_id"`
}

// Enabled 判断用户是否填写了telegram_bot
func (t TelegramBotConfig) Enabled() bool {
	token := strings.TrimSpace(t.Token)
	chatID := strings.TrimSpace(t.ChatID)
	if token == "" || chatID == "" {
		return false
	}
	return token != defaultTelegramToken && chatID != defaultTelegramChatID
}

// LoadConfig 读取并解析 config.yaml
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	var cfg Config
	if err = yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}
