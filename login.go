package main

// 本文件实现"扫码登录应用宝 → 换 login_buffer"，也就是整条链路里唯一需要人工的一步。
//
// 链路：
//
//	① GET  open.weixin.qq.com/connect/qrconnect
//	        （appid=应用宝 PC 端的 wxd44977328b36e647，scope 含 snsapi_runtime_pcsdk）
//	        → 从返回的 HTML 里取二维码 uuid
//	② 终端渲染二维码：https://open.weixin.qq.com/connect/qrcode/<uuid>
//	③ 长轮询 long.open.weixin.qq.com/connect/l/qrconnect
//	        wx_errcode: 408=未扫 404=已扫未确认 405=已确认 403=取消 402=过期
//	        405 时同时下发 wx_code
//	④ GET  yybadaccess.3g.qq.com/pc_yyb/pcyyb_oauth?code=<wx_code>
//	        → Set-Cookie 里给出 openid / accesstoken / refreshtoken
//	⑤ POST yybadaccess.3g.qq.com/pc_yyb_auth/pcyyb_get_wx_login_buffer_auth
//	        → login_buffer（base64，内含 ticket / device_id / host_appid）
//
// login_buffer 约 2 小时后随 accesstoken 失效；用 refresh_token 重新换取
// login_buffer 的逻辑在 YYB_GO（POST /accounts/refresh），这里只做首次获取。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/skip2/go-qrcode"
)

const (
	// 应用宝 PC 端的微信扫码授权页（与微信开放平台 qrconnect 同一套协议）。
	yybOAuthURL = "https://open.weixin.qq.com/connect/qrconnect" +
		"?appid=wxd44977328b36e647" +
		"&redirect_uri=https://yybadaccess.3g.qq.com/pc_yyb/pcyyb_oauth?login_type=WX" +
		"&response_type=code&scope=snsapi_login,snsapi_runtime_pcsdk" +
		"&state=web&fast_login=1&self_redirect=true"
	yybCallbackURL = "https://yybadaccess.3g.qq.com/pc_yyb/pcyyb_oauth"
	wxQRCodeBase   = "https://open.weixin.qq.com/connect/qrcode/"
	wxQRPollURL    = "https://long.open.weixin.qq.com/connect/l/qrconnect"

	yybLoginBufferURL = "https://yybadaccess.3g.qq.com/pc_yyb_auth/pcyyb_get_wx_login_buffer_auth"
	yybBusinessID     = "pc_yyb_auth"
	yybAccessKey      = "wgrdg373hy26ww2"

	// 二维码在 5 分钟内有效，超时后需要重新生成。
	qrLoginTimeout = 5 * time.Minute
)

var (
	qrUUIDPattern = regexp.MustCompile(`/connect/qrcode/([^"'>\s]+)`)
	qrErrPattern  = regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`)
	qrCodePattern = regexp.MustCompile(`wx_code\s*=\s*'([^']*)'`)
)

// LoginForLoginBuffer 引导用户扫码，返回应用宝下发的 login_buffer。
//
// 调用时会在终端打印二维码并阻塞等待：扫码 → 手机确认 → 自动换取 login_buffer。
// 超过 5 分钟未完成会返回超时错误。
func LoginForLoginBuffer(ctx context.Context) (string, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Second, Jar: jar}

	// ① 打开授权页，从 HTML 里取二维码 uuid。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, yybOAuthURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("打开微信授权页失败: %w", err)
	}
	page, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	match := qrUUIDPattern.FindStringSubmatch(string(page))
	if len(match) < 2 {
		return "", fmt.Errorf("未能从授权页解析出二维码 uuid（微信可能改了页面）")
	}
	uuid := match[1]

	// ② 终端打印二维码；同时给出链接，渲染失败时也能手动打开。
	qrContent := wxQRCodeBase + uuid
	printQRCode(qrContent)

	// ③ 长轮询等待扫码。长轮询本身会挂 20~30 秒，所以单独用一个超时更长的 client。
	pollClient := &http.Client{Timeout: 40 * time.Second}
	deadline := time.Now().Add(qrLoginTimeout)
	var wxCode string
	for wxCode == "" {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("二维码超过 %s 未确认，已超时", qrLoginTimeout)
		}
		pollURL := wxQRPollURL + "?" + url.Values{
			"uuid": {uuid},
			"_":    {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		}.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := pollClient.Do(req)
		if err != nil {
			// 单次长轮询失败（网络抖动/超时）不致命，下一轮继续。
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			time.Sleep(time.Second)
			continue
		}
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var errCode int
		if m := qrErrPattern.FindStringSubmatch(string(text)); len(m) == 2 {
			errCode, _ = strconv.Atoi(m[1])
		}
		switch errCode {
		case 404: // 已扫码，等待手机上点确认
			fmt.Println("已扫码，请在手机上确认登录…")
		case 403:
			return "", fmt.Errorf("已取消登录")
		case 402:
			return "", fmt.Errorf("二维码已过期，请重新运行")
		case 405:
			if m := qrCodePattern.FindStringSubmatch(string(text)); len(m) == 2 {
				wxCode = m[1]
				fmt.Println("已确认，正在换取 login_buffer…")
			}
		default: // 408（未扫）或异常响应，稍等再轮询
			time.Sleep(500 * time.Millisecond)
		}
	}

	// ④ 用 wx_code 换应用宝的登录 cookie（openid / accesstoken / refreshtoken）。
	callback := yybCallbackURL + "?" + url.Values{
		"login_type": {"WX"},
		"code":       {wxCode},
		"state":      {"web"},
	}.Encode()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OAuth 回调失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cookies := map[string]string{}
	for _, raw := range []string{callback, "https://yybadaccess.3g.qq.com/"} {
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, ck := range jar.Cookies(parsed) {
			cookies[ck.Name] = ck.Value
		}
	}
	openid, accessToken := cookies["openid"], cookies["accesstoken"]
	if openid == "" || accessToken == "" {
		return "", fmt.Errorf("OAuth 回调没有下发 openid/accesstoken")
	}

	// ⑤ 用 openid + accesstoken 换 login_buffer（UAL 签名：md5(body + 时间戳 + key + nonce)）。
	payload, err := json.Marshal(map[string]any{
		"extInfo": map[string]any{
			"listS": map[string]any{
				"unionid":      map[string]any{"value": []string{openid}},
				"user_id":      map[string]any{"value": []string{openid}},
				"access_token": map[string]any{"value": []string{accessToken}},
			},
			"listI": map[string]any{"user_type": map[string]any{"value": []int{0}}},
		},
	})
	if err != nil {
		return "", err
	}
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := strconv.Itoa(rand.Intn(10000))
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, yybLoginBufferURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Ual-Access-Businessid", yybBusinessID)
	req.Header.Set("Ual-Access-Timestamp", timestamp)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", md5Hex(string(payload)+timestamp+yybAccessKey+nonce))
	req.Header.Set("Cookie", fmt.Sprintf("openid=%s; accesstoken=%s; refreshtoken=%s",
		openid, accessToken, cookies["refreshtoken"]))

	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 login_buffer 失败: %w", err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Ext  struct {
			ListS struct {
				LoginBuffer struct {
					Value []string `json:"value"`
				} `json:"login_buffer"`
			} `json:"list_s"`
		} `json:"ext_info"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("login_buffer 响应解析失败: %w（原文: %s）", err, snippet(raw))
	}
	if result.Code != 0 {
		return "", fmt.Errorf("login_buffer 接口返回失败: code=%d msg=%s", result.Code, result.Msg)
	}
	if len(result.Ext.ListS.LoginBuffer.Value) == 0 || result.Ext.ListS.LoginBuffer.Value[0] == "" {
		return "", fmt.Errorf("响应里没有 login_buffer（原文: %s）", snippet(raw))
	}
	return result.Ext.ListS.LoginBuffer.Value[0], nil
}

// printQRCode 在终端渲染二维码。ToSmallString(false) 用半角方块，
// 深色终端下会显示为"亮码"，微信、手机相机都能识别；同时打印链接便于手动打开。
func printQRCode(content string) {
	fmt.Printf("请用微信扫码登录应用宝（或直接打开）：\n%s\n\n", content)
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		fmt.Printf("二维码渲染失败（用上面的链接代替）：%v\n", err)
		return
	}
	fmt.Println(q.ToSmallString(false))
	fmt.Println("等待扫码…")
}

// md5Hex 是应用宝 UAL 接口需要的签名：md5(请求体 + 时间戳 + accessKey + nonce)。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
