package main

// Command mini 是"扫码/复用登录态 → 取 code → 换 ticket → 打卡"的最小可运行示例。
//
// 用法：
//
//	go run . -session <SESSION>                       # 扫码 → 取 code → 换 ticket
//	go run . -session <SESSION> -buffer buf.txt       # 复用已有 login_buffer（跳过扫码）
//	go run . -session <SESSION> -refresh              # 用 token 文件续期并重取 login_buffer（免扫码）
//	go run . -session <SESSION> -code-only            # 只取一个 code
//	go run . -session <SESSION> -checkin -lat 30.75 -lng 103.93   # 换到 ticket 后直接打卡
//
// 流程（code 一次性）：
//
//	扫码 → login_buffer
//	code → POST /platform-app/api/auth/checkin-ticket  → ticket（必须带 SESSION）
//	ticket + SESSION → POST /daily-checkin/api/student-checkin-record/checkin
//
// login_buffer 的来源优先级：-buffer > -refresh（读 token 文件续期）> 扫码。
// 扫码成功后 refresh_token 会写到 -token-file（token.json），后续用 -refresh 即可免扫码；
// 只有 accesstoken 过期时才会调用 refresh_token_auth 续期，然后重新获取 login_buffer。
//
// 暂时还没有实现自动获取 session cookie 的逻辑，需要通过 -session 手动传入.

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
	appID := flag.String("appid", "wxaa78bf2905f01cee", "目标小程序 appid")
	session := flag.String("session", "", "SESSION cookie 值（必填；-code-only 时可省略）")
	codeOnly := flag.Bool("code-only", false, "只取 code，不换 ticket")
	doCheckin := flag.Bool("checkin", false, "拿到 ticket 后直接打卡")
	lat := flag.Float64("lat", 0, "打卡纬度（-checkin 时必填）")
	lng := flag.Float64("lng", 0, "打卡经度（-checkin 时必填）")
	address := flag.String("address", "", "reverseGeocode 文本，可空")
	refresh := flag.Bool("refresh", false, "用 -token-file 续期并重新获取 login_buffer（免扫码）")
	tokenFile := flag.String("token-file", "token.json", "token 文件路径，扫码后写入本地磁盘，默认token.json")
	flag.Parse()

	// 整体上限 10min, 二维码的有效时间是5min
	// 握手、HTTP 请求各自的超时仍在函数内部（10~40s）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

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
			// 保存 token, 下次打卡可以不扫码，直接使用已有的token获取loginbuffer
			if err := SaveTokens(*tokenFile, tokens); err != nil {
				log.Printf("警告: 保存 token 到 %s 失败（本次仍可继续）: %v", *tokenFile, err)
			} else {
				fmt.Printf("token_file=%s\n", *tokenFile)
			}
			loginBuffer = lb
		}
		// 打印出来便于保存复用（约 2 小时有效）。
		fmt.Printf("login_buffer=%s\n", loginBuffer)
	}

	if *codeOnly {
		// 只获取 code，获取成功之后马上退出
		code, err := GetCode(ctx, loginBuffer, *appID)
		if err != nil {
			log.Fatalf("获取 code 失败: %v", err)
		}
		fmt.Printf("code=%s\n", code)
		return
	}

	// 登录态只从命令行来：缺了就直接报错，不做任何自动登录。
	sessionID := strings.TrimSpace(*session)
	if sessionID == "" {
		log.Fatal("必须用 -session 传入 SESSION cookie 值")
	}

	// 换 ticket：必须带 SESSION，否则 4006。
	code, err := GetCode(ctx, loginBuffer, *appID)
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

	if !*doCheckin {
		return
	}
	result, err := Checkin(ctx, sessionID, ticket, CheckinRequest{
		Latitude:  *lat,
		Longitude: *lng,
		Address:   *address,
	})
	if err != nil {
		log.Fatalf("打卡失败: %v（响应: %s）", err, result)
	}
	fmt.Printf("checkin=%s\n", result)
}

// loadLoginBuffer 输入命令行传入的login_buffer文件路径
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
