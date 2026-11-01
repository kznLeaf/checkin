package main

// 本文件是"用小程序 code 换打卡 ticket"的实现，对应晚安成电小程序里的
// utils/checkinTicket.js：
//
//	wx.login() → code
//	POST https://jzsz.uestc.edu.cn/platform-app/api/auth/checkin-ticket
//	     Content-Type: application/json
//	     platform: mp-weixin
//	     Cookie: SESSION=<sessionId>
//	     {"code":"<code>"}
//	→ {"ok":true,"data":{"ticket":"<43 位 base64url>"}}
//
// 说明：这个接口要求已登录，缺 SESSION 时会返回 {"code":4006,"msg":"请先登录小程序"}。
// 登录态不在这里获取：调用方通过 -session 传入已知的 SESSION cookie 值即可。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// 晚安成电（成电智慧校园）小程序后端。
	checkinBaseURL    = "https://jzsz.uestc.edu.cn/platform-app"
	checkinTicketPath = "/api/auth/checkin-ticket"

	// 小程序 wx.request 固定带的平台标识。
	platformHeader = "mp-weixin"

	// ticket 长度：43 位 base64url（32 字节），小程序侧会做同样校验。
	checkinTicketLen = 43
)

// apiEnvelope 是后端统一响应包：{ok, code, msg, data}。
// ok 用指针是为了区分"字段缺失"与"值为 false"。
type apiEnvelope struct {
	OK   *bool           `json:"ok"`
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// GetCheckinTicket 用一条 wx.login code 换取打卡票据。
//
// 参数：
//   - sessionID：命令行传入的 SESSION cookie 值（必填）
//   - code：GetCode 返回的小程序 code，一次性，调用前不要重复使用
//
// 返回的 ticket 有两种用法：
//
//	网页：  https://jzsz.uestc.edu.cn/daily-checkin-h5/index.html?sessionId=<SESSION>&checkinTicket=<ticket>
//	接口：  放在打卡请求头 X-Checkin-Ticket 里（见 checkin.go，抓包确认）
func GetCheckinTicket(ctx context.Context, sessionID, code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("code 为空")
	}

	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinBaseURL+checkinTicketPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("platform", platformHeader)
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		// 后端认的是 SESSION cookie，而不是响应里的 sessionId 这个名字。
		req.Header.Set("Cookie", "SESSION="+sessionID)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求打卡 ticket 接口失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("打卡 ticket 接口 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	var env apiEnvelope
	if err = json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("难以解析 ticket 响应: %w（原文: %s）", err, snippet(raw))
	}
	// 失败时后端会给出 ok=false 与非空 msg；ok 缺失时退回看 code。
	if (env.OK != nil && !*env.OK) || (env.OK == nil && env.Code != 0) {
		return "", fmt.Errorf("打卡 ticket 接口拒绝: code=%d msg=%s", env.Code, env.Msg)
	}

	var data struct {
		Ticket string `json:"ticket"`
	}
	if len(env.Data) > 0 {
		if err = json.Unmarshal(env.Data, &data); err != nil {
			return "", fmt.Errorf("解析 ticket 字段失败: %w（原文: %s）", err, snippet(raw))
		}
	}
	if data.Ticket == "" {
		return "", fmt.Errorf("响应里没有 ticket（原文: %s）", snippet(raw))
	}
	const base64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	if len(data.Ticket) != checkinTicketLen || strings.Trim(data.Ticket, base64URL) != "" {
		return "", fmt.Errorf("ticket 格式异常（期望 %d 位 base64url）: %s", checkinTicketLen, data.Ticket)
	}
	return data.Ticket, nil
}

// snippet 截断响应原文，避免把整页 HTML 打进错误信息。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
