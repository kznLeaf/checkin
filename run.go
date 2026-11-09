package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/kznLeaf/checkin/mmtls"
)

// run 执行完整流程：login_buffer → SESSION → code → ticket → 打卡。
//
// 每一步的错误都带上上下文返回给 main，由 main 负责将报错完整信息通知Telegram bot.
//
// 成功打卡时的典型返回值：{"code":0,"msg":"操作成功","data":true,"ok":true}
func run(ctx context.Context, cfg Config, opts runOptions) (string, error) {
	// ① login_buffer：-buffer > -refresh > 扫码
	loginBuffer, err := loadLoginBuffer(opts.buffer)
	if err != nil {
		return "", err
	}
	if loginBuffer == "" {
		if !opts.noRefresh {
			// 尝试自动获取 login_buffer(默认)
			loginBuffer, err = RefreshLoginBuffer(ctx, opts.tokenFile)
			if err != nil {
				return "", fmt.Errorf("用 refresh token 获取 login_buffer 失败: %w", err)
			}
			fmt.Println("成功自动获取login_buffer")
		} else {
			// 重新扫码登录应用宝
			tokens, lb, err := LoginForLoginBuffer(ctx)
			if err != nil {
				return "", fmt.Errorf("扫码获取 login_buffer 失败: %w", err)
			}
			// 保存 token, 下次打卡可以不扫码，直接使用已有的 token 获取 login_buffer
			if err := SaveTokens(opts.tokenFile, tokens); err != nil {
				log.Printf("警告: 保存 token 到 %s 失败（本次仍可继续）: %v", opts.tokenFile, err)
			} else {
				fmt.Printf("token_file=%s\n", opts.tokenFile)
			}
			loginBuffer = lb
		}
	}

	if opts.codeOnly {
		// 只获取 code，获取成功之后马上退出
		code, err := mmtls.GetCode(ctx, loginBuffer, opts.appID)
		if err != nil {
			return "", fmt.Errorf("获取 code 失败: %w", err)
		}
		fmt.Printf("code=%s\n", code)
		return "", nil
	}

	// ② SESSION：如果命令行已经传入则跳过登录步骤，否则用 config.yaml 走 CAS 登录。
	sessionID := strings.TrimSpace(opts.session)
	if sessionID == "" {
		if sessionID, err = GetSessionCookie(ctx, cfg); err != nil {
			return "", fmt.Errorf("自动登录获取 SESSION 失败: %w", err)
		}
		fmt.Printf("session=%s\n", sessionID)
	}

	// ③ 换 ticket：必须带 SESSION，否则 4006。
	code, err := mmtls.GetCode(ctx, loginBuffer, opts.appID)
	if err != nil {
		return "", fmt.Errorf("获取 code 失败: %w", err)
	}
	// code 只有 5 分钟有效且一次性，先打印出来，后面失败也不至于白取。
	fmt.Printf("code=%s\n", code)

	ticket, err := GetCheckinTicket(ctx, sessionID, code)
	if err != nil {
		return "", fmt.Errorf("获取 ticket 失败: %w", err)
	}
	fmt.Printf("ticket=%s\n", ticket)

	result, err := Checkin(ctx, sessionID, ticket, opts.appID, cfg)
	if err != nil {
		return result, fmt.Errorf("打卡失败: %w\n晚安成电服务端的响应: %s", err, result)
	}
	return result, nil
}

// loadLoginBuffer 输入命令行传入的 login_buffer 文件路径(e.g. `buf.txt`) 或已经取得的login_buffer string
func loadLoginBuffer(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
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
