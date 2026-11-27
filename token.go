package main

// 本文件管理应用宝 OAuth 的登录态（token）以及用它换 login_buffer 字符串
//
// 生命周期：
//   - accesstoken 约 2 小时，过期后用 refreshtoken 续期
//   - refreshtoken 是长期凭证
//   - login_buffer 由 accesstoken 换出，同样约 2 小时。
//
// 两个接口都要求 UAL 签名：md5(请求体 + 时间戳毫秒 + accessKey + nonce)。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	yybLoginBufferURL = "https://yybadaccess.3g.qq.com/pc_yyb_auth/pcyyb_get_wx_login_buffer_auth"
	yybRefreshURL     = "https://yybadaccess.3g.qq.com/pc_yyb_auth/pcyyb_refresh_token_auth"
	yybBusinessID     = "pc_yyb_auth"
	yybAccessKey      = "wgrdg373hy26ww2"

	// accesstoken 默认 2 小时；续期提前 5 分钟触发，避免临界失败。
	// 加一分钟是为了跟标准过期时间区分
	defaultTokenTTL  = 7201
	refreshAheadSecs = 300
)

// OAuthTokens 是扫码登录后拿到的登录态，也是 -refresh 依赖的 token 文件格式。
// 字段名与 YYB_GO 的账号凭证保持一致，方便互相导入导出。
type OAuthTokens struct {
	OpenID       string `json:"openid"`
	AccessToken  string `json:"accesstoken"`
	RefreshToken string `json:"refreshtoken"`
	LoginType    string `json:"logintype"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
}

// SaveTokens 把 token 写回文件
func SaveTokens(path string, t OAuthTokens) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// 0600: (U)ser / owner can read, can write and can't execute.
	// (G)roup can't read, can't write and can't execute.
	// (O)thers can't read, can't write and can't execute.
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// LoadTokens 读取 -token-file 指定的 token 文件。
func LoadTokens(path string) (OAuthTokens, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("读取 token 文件 %s 失败: %w", path, err)
	}
	var t OAuthTokens
	if err = json.Unmarshal(raw, &t); err != nil {
		return OAuthTokens{}, fmt.Errorf("token 文件 %s 不是合法 JSON: %w", path, err)
	}
	if t.LoginType == "" {
		t.LoginType = "WX"
	}
	return t, nil
}

// RefreshLoginBuffer 用 token 文件续期并重新换一个 login_buffer。
//
// accesstoken 还有效（剩余 > 5 分钟）时直接用；否则先换取新token,
// 把新 token 写回文件后再换 login_buffer。
func RefreshLoginBuffer(ctx context.Context, tokenFile string) (string, error) {
	t, err := LoadTokens(tokenFile)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" || t.ExpiresAt <= time.Now().Add(refreshAheadSecs*time.Second).Unix() {
		if err = refreshAccessToken(ctx, &t); err != nil {
			return "", err
		}
		// 写回tokens到tokenFile
		if err = SaveTokens(tokenFile, t); err != nil {
			return "", fmt.Errorf("保存续期后的 token 失败: %w", err)
		}
	}
	return fetchLoginBuffer(ctx, t)
}

// refreshAccessToken 调 pcyyb_refresh_token_auth 换新的 accesstoken
func refreshAccessToken(ctx context.Context, t *OAuthTokens) error {
	if t.RefreshToken == "" {
		return fmt.Errorf("token 文件缺少 refreshtoken，无法续期；请重新扫码")
	}
	loginType := t.LoginType
	if loginType == "" {
		loginType = "WX"
	}
	payload, err := json.Marshal(map[string]any{
		"userInfo": map[string]string{
			"openId":       t.OpenID,
			"refreshToken": t.RefreshToken,
			"accessToken":  t.AccessToken,
			"loginType":    loginType,
		},
	})
	if err != nil {
		return err
	}

	req, err := newYYBRequest(ctx, yybRefreshURL, payload)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("请求 token 续期接口失败: %w", err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		User struct {
			AccessToken  string    `json:"access_token"`
			RefreshToken string    `json:"refresh_token"`
			ExpiresIn    flexInt64 `json:"expires_in"`
		} `json:"user_info"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("token 续期响应解析失败: %w（原文: %s）", err, snippet(raw))
	}
	if result.Code != 0 {
		return fmt.Errorf("token 续期被拒绝: code=%d msg=%s（可能 refreshtoken 已失效，请重新扫码）", result.Code, result.Msg)
	}
	if result.User.AccessToken == "" {
		return fmt.Errorf("token 续期响应没有 access_token（原文: %s）", snippet(raw))
	}

	t.AccessToken = result.User.AccessToken
	if result.User.RefreshToken != "" {
		t.RefreshToken = result.User.RefreshToken
	}
	if result.User.ExpiresIn <= 0 {
		result.User.ExpiresIn = defaultTokenTTL
	}
	t.ExpiresIn = int64(result.User.ExpiresIn)
	t.ExpiresAt = time.Now().Unix() + int64(result.User.ExpiresIn)
	return nil
}

// flexInt64 服务端会把 expires_in 写为string类型，导致无法解析为int64
// （续期接口实际返回的是 "expires_in":"7200"，不是数字）。
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = flexInt64(v)
		return nil
	} else {
		return fmt.Errorf("无法解析整数 %q: %w", s, err)
	}
}

// fetchLoginBuffer 用 openid + accesstoken 换 login_buffer（扫码流程与续期流程共用）。
func fetchLoginBuffer(ctx context.Context, t OAuthTokens) (string, error) {
	if t.OpenID == "" || t.AccessToken == "" {
		return "", fmt.Errorf("token 缺少 openid 或 accesstoken")
	}
	payload, err := json.Marshal(map[string]any{
		"extInfo": map[string]any{
			"listS": map[string]any{
				"unionid":      map[string]any{"value": []string{t.OpenID}},
				"user_id":      map[string]any{"value": []string{t.OpenID}},
				"access_token": map[string]any{"value": []string{t.AccessToken}},
			},
			"listI": map[string]any{"user_type": map[string]any{"value": []int{0}}},
		},
	})
	if err != nil {
		return "", err
	}

	req, err := newYYBRequest(ctx, yybLoginBufferURL, payload)
	if err != nil {
		return "", err
	}
	// 这个接口除了 UAL 头，还要把三个 token 放在 Cookie 里。
	req.Header.Set("Cookie", fmt.Sprintf("openid=%s; accesstoken=%s; refreshtoken=%s",
		t.OpenID, t.AccessToken, t.RefreshToken))

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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

// newYYBRequest 构造带 UAL 签名的应用宝 POST 请求。
func newYYBRequest(ctx context.Context, endpoint string, payload []byte) (*http.Request, error) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonce := strconv.FormatInt(time.Now().UnixNano()%10000, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Ual-Access-Businessid", yybBusinessID)
	req.Header.Set("Ual-Access-Timestamp", timestamp)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", md5Hex(string(payload)+timestamp+yybAccessKey+nonce))
	return req, nil
}

// md5Hex 是应用宝 UAL 接口需要的签名：md5(请求体 + 时间戳 + accessKey + nonce)。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
