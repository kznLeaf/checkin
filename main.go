package main

// Command mini 是"自动登录 → 取 code → 换 ticket → 打卡"的一体化工具。
//
// 用法：
//
//	go run .                            # CAS 登录拿 SESSION → 扫码拿 login_buffer → code → ticket
//	go run . -refresh                   # 免扫码：用 token.json 续期重取 login_buffer
//	go run . -session <SESSION>         # 跳过 CAS 登录，直接用给定 SESSION
//	go run . -code-only                 # 只取一个 code（不需要 SESSION）
//	go run . -checkin                   # 换到 ticket 后直接打卡（位置来自 config.yaml）
//	go run . -buffer buf.txt            # 复用已有 login_buffer（跳过扫码）
//
// 流程：
//
//	config.yaml ──► CAS 登录（idas）──► jzsz 的 SESSION
//	扫码 / token.json ──► login_buffer
//	code → POST /platform-app/api/auth/checkin-ticket  → ticket（必须带 SESSION）
//	ticket + SESSION → POST /daily-checkin/api/student-checkin-record/checkin
//
// login_buffer 的来源优先级：-buffer > -refresh（读 token 文件续期）> 扫码。
// 扫码成功后 token 会写到 -token-file（默认 token.json），之后用 -refresh 即可免扫码。
//
// 学号/密码/浏览器指纹/经纬度/mmwebid 全部来自 config.yaml（默认当前目录），
// 与 faq-daily-checkin 工程的配置格式一致，不提供对应的命令行参数。

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("mini: ")

	buffer := flag.String("buffer", "", "login_buffer：文件路径或 base64 文本（省略则扫码登录获取）")
	// appID := flag.String("appid", "wxaa78bf2905f01cee", "目标小程序 appid")
	session := flag.String("session", "", "SESSION cookie 值；传入后跳过脚本登录，直接使用该session")
	configPath := flag.String("config", "config.yaml", "配置文件路径, 默认本目录下的config.yaml")
	codeOnly := flag.Bool("code-only", false, "只取 code，不换 ticket")
	doCheckin := flag.Bool("checkin", false, "拿到 ticket 后直接打卡")
	refresh := flag.Bool("refresh", false, "用 -token-file 续期并重新获取 login_buffer（免扫码）")
	tokenFile := flag.String("token-file", "token.json", "token 文件路径，扫码后写入本地磁盘，默认token.json")
	flag.Parse()

	// 硬编码。本程序只用于晚安成电打卡。
	appID := "wxaa78bf2905f01cee"

	// 整体上限 10min, 二维码的有效时间是5min
	// 握手、HTTP 请求各自的超时仍在函数内部（10~40s）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// ① login_buffer：-buffer > -refresh > 扫码。
	loginBuffer, err := loadLoginBuffer(*buffer)
	if err != nil {
		log.Fatal(err)
	}
	if loginBuffer == "" {
		if *refresh {
			// 启用自动获取 login_buffer
			loginBuffer, err = RefreshLoginBuffer(ctx, *tokenFile)
			if err != nil {
				log.Fatalf("用 refresh token 获取 login_buffer 失败: %v", err)
			}
		} else {
			tokens, lb, err := LoginForLoginBuffer(ctx)
			if err != nil {
				log.Fatalf("扫码获取 login_buffer 失败: %v", err)
			}
			// 保存 token, 下次打卡可以不扫码，直接使用已有的 token 获取 login_buffer
			if err := SaveTokens(*tokenFile, tokens); err != nil {
				log.Printf("警告: 保存 token 到 %s 失败（本次仍可继续）: %v", *tokenFile, err)
			} else {
				fmt.Printf("token_file=%s\n", *tokenFile)
			}
			loginBuffer = lb
		}
		// 展示 login_buffer. 有效时间约2h
		fmt.Printf("login_buffer=%s\n", loginBuffer)
	}

	if *codeOnly {
		// 只获取 code，获取成功之后马上退出
		code, err := GetCode(ctx, loginBuffer, appID)
		if err != nil {
			log.Fatalf("获取 code 失败: %v", err)
		}
		fmt.Printf("code=%s\n", code)
		return
	}

	// ② SESSION：如果命令行已经传入则跳过登录步骤，否则用 config.yaml 走 CAS 登录。
	var cfg Config
	if strings.TrimSpace(*session) == "" || *doCheckin {
		cfg, err = LoadConfig(*configPath)
		if err != nil {
			log.Fatal(err)
		}
	}

	sessionID := strings.TrimSpace(*session)
	if sessionID == "" {
		if sessionID, err = GetSessionCookie(ctx, cfg); err != nil {
			log.Fatalf("自动登录获取 SESSION 失败: %v", err)
		}
		fmt.Printf("session=%s\n", sessionID)
	}

	// ③ 换 ticket：必须带 SESSION，否则 4006。
	code, err := GetCode(ctx, loginBuffer, appID)
	if err != nil {
		log.Fatalf("获取 code 失败: %v", err)
	}
	// code 只有 5 分钟有效且一次性，先打印出来，后面失败也不至于白取。
	fmt.Printf("code=%s\n", code)

	ticket, err := GetCheckinTicket(ctx, sessionID, code)
	if err != nil {
		log.Fatalf("获取 ticket 失败: %v", err)
	}
	fmt.Printf("ticket=%s\n", ticket)

	// ④ 可选参数：打卡（位置来自 config.yaml）。
	if !*doCheckin {
		return
	}
	result, err := Checkin(ctx, sessionID, ticket, appID, cfg)
	if err != nil {
		log.Fatalf("打卡失败: %v（响应: %s）", err, result)
	}
	fmt.Printf("checkin=%s\n", result)
}

// loadLoginBuffer 输入命令行传入的 login_buffer 文件路径(e.g. `buf.txt`) 或已经取得的login_buffer string
func loadLoginBuffer(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	// 检查
	if st, err := os.Stat(v); err == nil && !st.IsDir() {
		raw, err := os.ReadFile(v)
		if err != nil {
			return "", fmt.Errorf("读取 login_buffer 文件失败: %w", err)
		}
		v = strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("login_buffer 文件为空")
		}
	}
	return v, nil
}
