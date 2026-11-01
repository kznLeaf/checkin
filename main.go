package main

// Command mini 是"取微信登录 code + 换晚安成电打卡 ticket"的最小可运行示例。
//
// 用法：
//
//	go run . -buffer login_buffer.txt
//	go run . -buffer login_buffer.txt -session <SESSION>
//	go run . -buffer login_buffer.txt -code-only
//
// 参数：
//
//	-buffer     login_buffer：文件路径或 base64 文本本身（必填）
//	-appid      目标小程序 appid，默认晚安成电 wxaa78bf2905f01cee
//	-session    打卡后端登录态 SESSION cookie 值（可选）
//	-code-only  只取 code，不请求 ticket
//
// login_buffer 的来源：应用宝扫码登录后换取（见 YYB_GO：POST /qr → /qr/{id}/poll →
// /qr/{id}/confirm），或者用自己的 openid/accesstoken/refreshtoken 调
// https://yybadaccess.3g.qq.com/pc_yyb_auth/pcyyb_get_wx_login_buffer_auth 换取。
// 扫码只需一次，之后 login_buffer/凭证都能续期，日常取 code 无需人工。
//
// 输出：先打印 code（5 分钟有效、一次性），再打印 ticket。

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

	buffer := flag.String("buffer", "", "login_buffer：文件路径或 base64 文本（必填）")
	appID := flag.String("appid", "wxaa78bf2905f01cee", "目标小程序 appid")
	session := flag.String("session", "", "打卡后端的 SESSION cookie 值（可选）")
	codeOnly := flag.Bool("code-only", false, "只取 code，不请求 ticket")
	flag.Parse()

	loginBuffer, err := loadLoginBuffer(*buffer)
	if err != nil {
		log.Fatal(err)
	}

	// 整体上限：握手/登录 + 取 code + 换 ticket；单步超时在各自函数内另有 10~15s。
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	code, err := GetCode(ctx, loginBuffer, *appID)
	if err != nil {
		log.Fatalf("获取 code 失败: %v", err)
	}
	// 先输出 code：它 5 分钟内有效且一次性，后面的 ticket 失败也不至于白取。
	fmt.Printf("code=%s\n", code)

	if *codeOnly {
		return
	}

	ticket, err := GetCheckinTicket(ctx, *session, code)
	if err != nil {
		log.Fatalf("获取 ticket 失败: %v", err)
	}
	fmt.Printf("ticket=%s\n", ticket)
}

// loadLoginBuffer 支持两种输入：已存在的文件路径，或直接给出的 base64 文本。
func loadLoginBuffer(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("必须用 -buffer 提供 login_buffer（文件路径或 base64 文本）")
	}
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
