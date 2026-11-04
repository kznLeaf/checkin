package main

// Command mini 是"自动登录 → 取 code → 换 ticket → 打卡"的一体化工具。
//
// 用法：
//
//	go run .                            # CAS 登录拿 SESSION → 自动续期 → code → ticket → 打卡
//	go run . -no-refresh                # 强制重新扫码登录，不管是否已经有token.json
//	go run . -session <SESSION>         # 直接用给定session cookie，跳过自动获取的步骤
//	go run . -code-only                 # 获取 code 后直接结束
//	go run . -buffer buf.txt            # 复用已有 login_buffer(需要确保没有过期)
//
// 流程：
//
//	config.yaml ──► CAS 登录（idas）──► jzsz 的 SESSION
//	扫码 / token.json ──► login_buffer
//	code → POST /platform-app/api/auth/checkin-ticket  → ticket（必须带 SESSION）
//	ticket + SESSION → POST /daily-checkin/api/student-checkin-record/checkin
//
// login_buffer 的来源优先级：-buffer > -refresh（读 token 文件续期）> 扫码。
// 扫码成功后 token 会写到 -token-file（默认工程目录下token.json），默认自动续期
//
// 学号/密码/浏览器指纹/经纬度/mmwebid 全部来自 config.yaml
//
// 可选：config.yaml 里填写 telegram_bot 后，打卡成功后会给 bot 发一条通知

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"
)

type runOptions struct {
	buffer    string // -buffer
	session   string // -session
	tokenFile string // -token-file
	noRefresh bool   // -no-refresh
	codeOnly  bool   // -code-only
	appID     string
}

func main() {
	// TODO: 更好的日志
	log.SetFlags(0)
	log.SetPrefix("checkin: ")

	buffer := flag.String("buffer", "", "login_buffer：文件路径或 base64 文本（省略则扫码登录获取）")
	session := flag.String("session", "", "SESSION cookie 值；传入后跳过脚本登录，直接使用该session")
	configPath := flag.String("config", "config.yaml", "配置文件路径, 默认本目录下的config.yaml")
	codeOnly := flag.Bool("code-only", false, "只取 code，不换 ticket")
	noRefresh := flag.Bool("no-refresh", false, "强制重新扫码登录应用宝")
	tokenFile := flag.String("token-file", "token.json", "token 文件路径，扫码后写入本地磁盘，默认token.json")
	flag.Parse()

	// 整体上限 10min, 二维码的有效时间是5min
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		if !*codeOnly {
			log.Fatal(err)
		}
		cfg = Config{}
	}

	result, err := run(ctx, cfg, runOptions{
		buffer:    *buffer,
		session:   *session,
		tokenFile: *tokenFile,
		noRefresh: *noRefresh,
		codeOnly:  *codeOnly,
		appID:     "wxaa78bf2905f01cee",
	})
	if err != nil {
		log.Print(err)
		if cfg.TelegramBot.Enabled() {
			// 将错误信息通知给Telegram bot
			t := currentTime()
			msg := fmt.Sprintf("[%s] %s", t, err.Error())
			if sendErr := sendTelegramMessage(cfg.TelegramBot.Token, cfg.TelegramBot.ChatID, msg); sendErr != nil {
				log.Printf("发送 Telegram 消息失败: %v", sendErr)
			}
			fmt.Println("成功将报错上报bot")
		}
		return
	}

	if result != "" && cfg.TelegramBot.Enabled() {
		// 拼接发送给 bot 的信息
		message := fmt.Sprintf("%s 打卡成功\n服务端响应: %s",
			currentTime(),
			result)
		if err := sendTelegramMessage(cfg.TelegramBot.Token, cfg.TelegramBot.ChatID, message); err != nil {
			log.Fatalf("发送 Telegram 消息失败: %v", err)
		}
		fmt.Println("上报本次打卡信息给bot")
	}
}
