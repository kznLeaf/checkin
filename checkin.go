package main

// 本文件是打卡动作本身，接口定义来自一次真实抓包：
//
//	POST https://jzsz.uestc.edu.cn/daily-checkin/api/student-checkin-record/checkin
//	     X-Checkin-Ticket: <ticket>        ← ticket 放在请求头，不是 query 参数
//	     platform: h5
//	     Cookie: SESSION=<sessionId>
//	     Origin/Referer: https://jzsz.uestc.edu.cn/daily-checkin-h5/index.html?sessionId=<sessionId>
//	     {"latitude":…, "longitude":…, "reverseGeocode":…, "clientTime":…, "userAgent":…, …}
//
// 请求体里的定位相关字段照抄抓包：coordinateSystemCode=gcj02、accuracy=30、
// locationDurationMs=563、networkTypeCode=4g，locationEnabled 等字段为 WebView 定位失败的固定值。
// 响应结构未知，这里原样返回 JSON 文本，交给调用方打印。

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
	// 打卡的 H5 与 API 都在站点根路径下，不在 /platform-app 下。
	checkinSiteURL = "https://jzsz.uestc.edu.cn"
	checkinAPIPath = "/daily-checkin/api/student-checkin-record/checkin"
	checkinH5Path  = "/daily-checkin-h5/index.html"

	// 抓包里的微信 Android UA：请求头与 body.userAgent 保持一致。
	defaultCheckinUA = "Mozilla/5.0 (Linux; Android 14; TNA-AN00 Build/HONORTNA-AN00; wv) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/150.0.7871.189 Mobile Safari/537.36 " +
		"XWEB/1500145 MMWEBSDK/20260902 MMWEBID/445 MicroMessenger/8.0.79.3200(0x28004F35) " +
		"WeChat/arm64 Weixin NetType/5G Language/zh_CN ABI/arm64 miniProgram/wxaa78bf2905f01cee"
)

// CheckinRequest 是打卡需要的定位信息。坐标系固定 gcj02（与抓包一致）。
type CheckinRequest struct {
	Latitude  float64 // 纬度
	Longitude float64 // 经度
	Address   string  // reverseGeocode 文本，可为空
}

// Checkin 用 ticket + SESSION 提交一次打卡，返回接口原始响应。
//
// 参数：
//   - sessionID：命令行传入的 SESSION cookie 值
//   - ticket：GetCheckinTicket 得到的 43 位票据
//   - in：打卡定位，纬度/经度必须非 0
func Checkin(ctx context.Context, sessionID, ticket string, in CheckinRequest) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	ticket = strings.TrimSpace(ticket)
	if sessionID == "" {
		return "", fmt.Errorf("SESSION 为空")
	}
	if ticket == "" {
		return "", fmt.Errorf("ticket 为空")
	}
	if in.Latitude == 0 || in.Longitude == 0 {
		return "", fmt.Errorf("打卡需要有效的 -lat/-lng")
	}

	// 字段名与顺序无关，这里按抓包里的完整字段集提交，避免后端缺字段判错。
	body, err := json.Marshal(map[string]any{
		"latitude":             in.Latitude,
		"longitude":            in.Longitude,
		"reverseGeocode":       in.Address,
		"clientTime":           time.Now().Format("2006-01-02 15:04:05"),
		"userAgent":            defaultCheckinUA,
		"coordinateSystemCode": "gcj02",
		"accuracy":             30,
		"locationDurationMs":   563,
		"networkTypeCode":      "4g",
		"locationEnabled":      false,
		"locationAuthorized":   "",
		"locationAccuracy":     "",
		"errorMessage":         "",
	})
	if err != nil {
		return "", err
	}

	referer := checkinSiteURL + checkinH5Path + "?sessionId=" + sessionID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinSiteURL+checkinAPIPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", defaultCheckinUA)
	req.Header.Set("platform", "h5")
	req.Header.Set("X-Checkin-Ticket", ticket)
	req.Header.Set("X-Requested-With", "com.tencent.mm")
	req.Header.Set("Origin", checkinSiteURL)
	req.Header.Set("Referer", referer)
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
	return text, nil
}
