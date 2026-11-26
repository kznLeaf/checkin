package main

// 本文件是 faq-daily-checkin/src/faq_daily_checkin/checkin.py 的等价实现：
//
//	POST https://jzsz.uestc.edu.cn/daily-checkin/api/student-checkin-record/checkin
//	     X-Checkin-Ticket: <ticket>
//	     platform: h5
//	     Cookie: SESSION=<sessionId>
//	     Origin/Referer/X-Requested-With/sec-ch-ua 等来自抓包
//
// 与 Python 版保持一致：微信 UA（含 config.yaml 的 mmwebid）、经纬度随机扰动、
// clientTime 用 Asia/Shanghai、reverseGeocode 固定、locationDurationMs 随机 300~1000ms、
// networkTypeCode=wifi，以及code==0 且 ok==true 才算成功

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"net/http"
	"strings"
	"time"
)

const (
	checkinSiteURL = "https://jzsz.uestc.edu.cn"
	checkinAPIPath = "/daily-checkin/api/student-checkin-record/checkin"
	checkinH5Path  = "/daily-checkin-h5/index.html"

	checkinAddress = "四川省成都市郫都区蓉昌高速"

	wechatVersion = "8.0.79.3200(0x28004F32)"
	chromeVersion = "150.0.7871.189"
	xwebVersion   = "1500145"
	sdkDate       = "20260902"
)

// errAlreadyCheckedIn 表示已经完成打卡
var errAlreadyCheckedIn = errors.New("今日已完成打卡")

// Checkin 用 ticket + SESSION 提交一次打卡，返回接口原始响应。
//
// 参数：
//   - sessionID：自动登录或 -session 传入的 SESSION cookie 值
//   - ticket：GetCheckinTicket 得到的 43 位票据
//   - appID：固定为晚安成电的ID
//   - cfg：位置与 mmwebid 来自 config.yaml
func Checkin(ctx context.Context, sessionID, ticket, appID string, cfg Config) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	ticket = strings.TrimSpace(ticket)
	if sessionID == "" {
		return "", fmt.Errorf("SESSION 为空")
	}
	if ticket == "" {
		return "", fmt.Errorf("ticket 为空")
	}
	if cfg.Position.Latitude == 0 || cfg.Position.Longitude == 0 {
		return "", fmt.Errorf("config.yaml 的 position.latitude / position.longitude 无效")
	}

	userAgent := generateMiniProgramUA(appID, cfg.MMWebID)
	// 打卡页面的 Referer 里是一个随机的会话标识，不是真实的 SESSION。
	// 因为服务端不会检验这个HTTP头，凑数用的
	referer := checkinSiteURL + checkinH5Path + "?sessionId=" + randomUUID()

	body, err := json.Marshal(map[string]any{
		"latitude":             addRealisticDrift(cfg.Position.Latitude),
		"longitude":            addRealisticDrift(cfg.Position.Longitude),
		"reverseGeocode":       checkinAddress,
		"clientTime":           currentTime(),
		"userAgent":            userAgent,
		"coordinateSystemCode": "gcj02",
		"accuracy":             30,
		"locationDurationMs":   mrand.Intn(701) + 300,
		"networkTypeCode":      "wifi",
		"locationEnabled":      false,
		"locationAuthorized":   "",
		"locationAccuracy":     "",
		"errorMessage":         "",
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinSiteURL+checkinAPIPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("platform", "h5")
	req.Header.Set("Origin", checkinSiteURL)
	req.Header.Set("X-Requested-With", "com.tencent.mm")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("sec-ch-ua-platform", `"Android"`)
	req.Header.Set("sec-ch-ua", `"Not;A=Brand";v="8", "Chromium";v="150", "Android WebView";v="150"`)
	req.Header.Set("sec-ch-ua-mobile", "?1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("X-Checkin-Ticket", ticket)
	req.Header.Set("Cookie", "SESSION="+sessionID)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求打卡接口失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(raw))
	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("打卡接口 HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	var result struct {
		Code int    `json:"code"`
		OK   bool   `json:"ok"`
		Msg  string `json:"msg"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return text, fmt.Errorf("打卡响应不是合法 JSON: %s", snippet(raw))
	}
	// 如果在一次打卡之后重复运行，返回errAlreadyCheckedIn
	if result.Code == 4006 && strings.Contains(result.Msg, "已完成打卡") {
		return text, errAlreadyCheckedIn
	}
	if result.Code != 0 || !result.OK {
		return text, fmt.Errorf("打卡未成功: code=%d msg=%s", result.Code, result.Msg)
	}
	return text, nil
}

// generateMiniProgramUA 生成微信小程序 WebView 的 User-Agent
func generateMiniProgramUA(appID string, mmwebid int) string {
	if mmwebid == 0 {
		// MMWEBID 是 WebView 实例号，随便编一个三位/四位数即可。
		mmwebid = mrand.Intn(9900) + 100
	}
	return "Mozilla/5.0 (Linux; Android 14; ELI-AN00 Build/HONORELI-AN00; wv) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/" + chromeVersion + " " +
		"Mobile Safari/537.36 XWEB/" + xwebVersion + " MMWEBSDK/" + sdkDate + " " +
		fmt.Sprintf("MMWEBID/%d ", mmwebid) +
		"MicroMessenger/" + wechatVersion + " " +
		"WeChat/arm64 Weixin NetType/WIFI Language/zh_CN ABI/arm64 " +
		"miniProgram/" + appID
}

// addRealisticDrift 给经纬度叠加 ±0.000003° 内的正态扰动，并保留 6 位小数（同 Python 版）。
func addRealisticDrift(value float64) float64 {
	const maxDrift = 0.000003
	drift := mrand.NormFloat64() * (maxDrift / 3.0)
	drift = math.Max(-maxDrift, math.Min(drift, maxDrift))
	return math.Round((value+drift)*1e6) / 1e6
}

// currentTime 字符串形式返回当前时间
func currentTime() string {
	east8Zone := time.FixedZone("CST", 8*3600)
	now := time.Now().In(east8Zone)
	return now.Format("2006-01-02 15:04:05")
}

// randomUUID 生成 v4 UUID，用于打卡页 Referer 里的 sessionId。
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
