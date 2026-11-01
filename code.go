package main

// 本文件是"用一段 login_buffer 换微信小程序登录 code"的最小实现。
//
// 整条链路（GetCode 里按顺序展开，方便对照抓包）：
//
//	① login_buffer 解析     解出 ticket / device_id / host_appid
//	② HTTPDNS               拿到 longcloud（mmtls）与 shortcloud（http）的 IP:Port
//	③ MMTLS 1-RTT 握手      ECDHE 建连、验签、收 NewSessionTicket（得到 PSK）
//	④ manualauth(cmd 0x0d7d) 用 login_buffer 登录，换回 UIN / SendKey / RecvKey / access PSK
//	⑤ 0-RTT + js-login      短连接发 transfer 信封，调 /cgi-bin/mmbiz-bin/js-login 取 code
//
// 与 YYB_GO 里的实现相比，这里去掉了连接池、SQLite 会话缓存、代理、并发与重试，
// 只保留一条直线；协议编解码/crypto 原语在 wire_*.go（与 YYB_GO 同一份逻辑，未改）。
//
// 关于"登录"的边界：应用宝扫码换 login_buffer 那一步必须人工扫一次码，
// 所以不在这个程序里；拿到 login_buffer 之后（含用 refresh_token 续期）全流程无人工。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// GetCode 用 login_buffer 换取 appID 对应的 wx.login code。
//
// 参数：
//   - loginBuffer：应用宝 OAuth 登录后得到的 login_buffer（base64 文本）
//   - appID：目标小程序 appid，晚安成电是 wxaa78bf2905f01cee
//
// 返回的 code 由微信服务端签发，与 appid + 该微信账号的 openid 绑定，
// 5 分钟内有效且只能被后端消费一次。
func GetCode(ctx context.Context, loginBuffer, appID string) (string, error) {
	const timeout = 10 * time.Second

	loginBuffer = strings.TrimSpace(loginBuffer)
	appID = strings.TrimSpace(appID)
	if loginBuffer == "" {
		return "", fmt.Errorf("login_buffer 为空")
	}
	if appID == "" {
		return "", fmt.Errorf("appid 为空")
	}

	// ① 解析 login_buffer：protobuf 里至少要有 ticket(field1) 与 device_id(field2)。
	meta, err := parseLoginBuffer(loginBuffer)
	if err != nil {
		return "", fmt.Errorf("login_buffer 解析失败: %w", err)
	}

	// ② HTTPDNS：微信的 CGI 都直连 IP，不做系统 DNS。
	longTargets, shortTargets, err := resolveTargets(ctx, timeout)
	if err != nil {
		return "", err
	}

	// ③ + ④ 长连接：握手成功后同一连接上跑一次 manualauth。
	// 多个候选地址逐个尝试，通常第一个就成功。
	var (
		sess    AppSession
		psk     pskEntry
		lastErr error
	)
	for _, target := range longTargets {
		mc, err := connectMmtls(ctx, target, timeout, "", true)
		if err != nil {
			lastErr = fmt.Errorf("连接 %s:%d 失败: %w", target.IP, target.Port, err)
			continue
		}
		sess, psk, err = manualAuth(mc, loginBuffer, meta)
		mc.close()
		if err != nil {
			lastErr = fmt.Errorf("manualauth(%s:%d) 失败: %w", target.IP, target.Port, err)
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return "", lastErr
	}

	// ⑤ 短连接：0-RTT PSK + transfer 信封 + js-login。
	// host_appid 来自 login_buffer，缺失时用应用宝的默认值。
	hostAppID := sess.HostAppID
	if len(hostAppID) == 0 {
		hostAppID = hostAppIDDefault
	}
	plain := buildJSAPIPlaintext(sess.UIN, appID, jsLoginURL, jsLoginCmdID, nil, hostAppID, nil)
	envelope, err := buildTransferPacket(sess, plain)
	if err != nil {
		return "", fmt.Errorf("构造 js-login 请求失败: %w", err)
	}

	code, _, err := send0RTT(ctx, shortTargets, psk, sess.RecvKey, envelope, timeout, "", true)
	if err != nil {
		return "", fmt.Errorf("js-login 请求失败: %w", err)
	}
	if len(code) == 0 {
		// 服务端未下发 code：多为该 appid 未被应用宝放行，或账号/会话已失效。
		return "", fmt.Errorf("js-login 未返回 code（检查 appid 是否被应用宝放行、login_buffer 是否过期）")
	}
	return string(code), nil
}

// manualAuth 在一条已握手的 MMTLS 长连接上执行 manualauth：
// 用 login_buffer 里的 ticket 登录，服务端返回 UIN、收发消息密钥和一张 access PSK，
// 后者是随后 0-RTT 短连接复用会话的凭据。
func manualAuth(mc *mmtlsClient, loginBuffer string, meta loginMeta) (AppSession, pskEntry, error) {
	// app_device_id 每次登录随机生成，属于设备指纹的一部分。
	appDeviceID, err := randomAppDeviceID()
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}

	// 请求体是 HybridEcdh 加密的 protobuf，加密用的临时私钥/中间密钥留在 temp 里，
	// 解析响应时要拿它来解。
	temp := &manualAuthTemp{}
	body, err := buildLoginBody(loginBuffer, meta.DeviceID, appDeviceID, temp)
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}
	if err = mc.sendApp(cmdManualAuth, body); err != nil {
		return AppSession{}, pskEntry{}, err
	}
	resp, err := mc.recvApp()
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}
	if resp.Cmd != cmdManualAuth {
		return AppSession{}, pskEntry{}, fmt.Errorf("manualauth 响应 cmd 不匹配: %d", resp.Cmd)
	}

	mar, err := parseLoginResponse(resp.Body, temp)
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}
	sess, err := extractSession(mar)
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}
	// 后续发包要用 login_buffer 里的 device_id，host_appid 用于 js-login 的宿主标识。
	sess.DeviceID = meta.DeviceID
	sess.HostAppID = meta.HostAppID

	psks, err := mc.extractPSKs()
	if err != nil {
		return AppSession{}, pskEntry{}, err
	}
	psk, ok := pickAccessPSK(psks)
	if !ok {
		return AppSession{}, pskEntry{}, fmt.Errorf("登录完成但服务端没有下发 access PSK")
	}
	return sess, psk, nil
}
