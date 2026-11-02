package main

// 本文件是 faq-daily-checkin/src/faq_daily_checkin/login.py 的等价移植：
// 用学号 + 密码登录学校 CAS（idas.uestc.edu.cn），换取 jzsz.uestc.edu.cn 的 SESSION cookie。
//
// 步骤、请求头、表单字段、正则、cookie 域与顺序都与 Python 版保持一致：
//
//	① GET  /authserver/login?service=https://jzsz.uestc.edu.cn/   取 execution / pwdEncryptSalt
//	② 设置 MULTIFACTOR_BROWSER_FINGERPRINT（只对 idas 域名），加密密码后提交表单（禁止跟随 302）
//	③ 跟随 302 的 Location 访问一次，从 cookie jar 里取 jzsz 域名下的 SESSION
//	④ GET  /api/isLoginFailure?_t=<毫秒>  激活 session
//
// 密码加密等价于 encrypt.js：64 位随机前缀 + 密码，AES-CBC/PKCS7（key=salt，iv=16 位随机），Base64。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	idasHost = "https://idas.uestc.edu.cn"
	jzszHost = "https://jzsz.uestc.edu.cn"

	// 登录使用的 UA 不能随便改，否则第一步就会失败（疑似存在校验机制）。
	loginUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0"

	// CAS 前端加密用的字符集（与 encrypt.js 一致）。
	passwordCharset = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"
)

var (
	executionPattern = regexp.MustCompile(`name="execution"\s+value="([^"]+)"`)
	saltPattern      = regexp.MustCompile(`id="pwdEncryptSalt"\s+value="([^"]+)"`)
)

// GetSessionCookie 用 config.yaml 里的学号/密码/浏览器指纹完成一次 CAS 登录，
// 返回 jzsz.uestc.edu.cn 域名下的 SESSION cookie 值。
func GetSessionCookie(ctx context.Context, cfg Config) (string, error) {
	if cfg.StudentID == "" || cfg.Password == "" || cfg.BrowserFingerprint == "" {
		return "", fmt.Errorf("config.yaml 缺少 student_id / password / browser_fingerprint")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	// 一个 client 正常跟随重定向（用于第③④步），复制一份禁止跟随（用于提交登录拿 302）。
	client := &http.Client{Timeout: 20 * time.Second, Jar: jar}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	loginURL := idasHost + "/authserver/login?service=" + jzszHost + "/"

	// ① 登录页：取 execution 和 pwdEncryptSalt。
	fmt.Println("[1/4] 获取登录页动态参数")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("打开登录页失败: %w", err)
	}
	page, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("打开登录页失败: HTTP %d", resp.StatusCode)
	}
	execution := firstGroup(executionPattern, page)
	salt := firstGroup(saltPattern, page)
	if execution == "" || salt == "" {
		return "", fmt.Errorf("无法从登录页提取 execution 或 pwdEncryptSalt")
	}
	fmt.Printf("成功获取 execution=%s, salt=%s\n", execution, salt)

	// 只有访问 idas 时才需要携带 MULTIFACTOR_BROWSER_FINGERPRINT。
	idasURL, _ := url.Parse(idasHost + "/")
	jar.SetCookies(idasURL, []*http.Cookie{{
		Name:  "MULTIFACTOR_BROWSER_FINGERPRINT",
		Value: cfg.BrowserFingerprint,
		Path:  "/",
	}})

	// ② 加密密码并提交登录表单（禁止跟随重定向）。
	fmt.Println("[2/4] 正在加密密码并提交登录...")
	encrypted, err := encryptPassword(cfg.Password, salt)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"username":  {cfg.StudentID},
		"password":  {encrypted},
		"captcha":   {""},
		"_eventId":  {"submit"},
		"cllt":      {"userNameLogin"},
		"dllt":      {"generalLogin"},
		"lt":        {""},
		"execution": {execution},
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", idasHost)
	req.Header.Set("Referer", loginURL)

	resp, err = noRedirect.Do(req)
	if err != nil {
		return "", fmt.Errorf("提交登录表单失败: %w", err)
	}
	submitBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("登录失败（HTTP %d），响应: %s", resp.StatusCode, snippet(submitBody))
	}
	location := resp.Header.Get("Location")
	fmt.Printf("登录成功，获得重定向URL: %s\n", location)

	// ③ 跟随重定向换取业务平台 SESSION。
	fmt.Println("[3/4] 正在跟随重定向获取SESSION...")
	redirectURL := resolveReference(loginURL, location)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, redirectURL, nil)
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	req.Header.Set("Referer", idasHost+"/")
	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("跟随重定向失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	session := cookieValue(jar, jzszHost+"/", "SESSION")
	if session == "" {
		return "", fmt.Errorf("未检测到 SESSION cookie（登录可能未生效）")
	}
	fmt.Printf("成功获取业务SESSION: %s\n", session)

	// ④ 访问 /api/isLoginFailure 激活 session（真实浏览器也会先发这个请求）。
	fmt.Println("[4/4] 激活 session cookie")
	warmupURL := fmt.Sprintf("%s/api/isLoginFailure?_t=%d", jzszHost, time.Now().UnixMilli())
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, warmupURL, nil)
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Origin", jzszHost)
	req.Header.Set("Referer", jzszHost+"/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")

	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("激活 session 失败: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("激活 session 失败: HTTP %d", resp.StatusCode)
	}

	// // 激活过程中服务端可能刷新 cookie，再取一次最新值。
	// if latest := cookieValue(jar, jzszHost+"/", "SESSION"); latest != "" {
	// 	session = latest
	// }

	fmt.Println("成功激活！")
	return session, nil
}

// setBrowserHeaders 设置 Python 版 session.headers 里的默认三项。
func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", loginUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,en-US;q=0.9,en;q=0.8")
}

// encryptPassword 等价于 encrypt.js / encrypt.py 的 encryptPassword：
// 64 位随机前缀 + 密码，AES-CBC/PKCS7（key=salt 16 字节，iv=16 位随机），Base64。
func encryptPassword(password, salt string) (string, error) {
	if len(salt) != aes.BlockSize {
		return "", fmt.Errorf("pwdEncryptSalt 必须是 16 个字符，实际 %d", len(salt))
	}
	plain := randomString(64) + password
	iv := randomString(aes.BlockSize)

	padded := pkcs7Pad([]byte(plain), aes.BlockSize)
	block, err := aes.NewCipher([]byte(salt))
	if err != nil {
		return "", err
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// pkcs7Pad 做 PKCS#7 填充（补到 blockSize 的整数倍）。
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	filler := make([]byte, pad)
	for i := range filler {
		filler[i] = byte(pad)
	}
	return append(data, filler...)
}

// randomString 从 CAS 的字符集里随机取 n 个字符。
func randomString(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = passwordCharset[rand.Intn(len(passwordCharset))]
	}
	return string(out)
}

// firstGroup 返回正则第一个捕获组，没有匹配时返回空串。
func firstGroup(re *regexp.Regexp, data []byte) string {
	if m := re.FindSubmatch(data); len(m) == 2 {
		return string(m[1])
	}
	return ""
}

// resolveReference 把 Location（可能是相对路径）解析成绝对地址。
func resolveReference(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// cookieValue 从 cookie jar 里取指定 URL 作用域下的 cookie 值。
func cookieValue(jar http.CookieJar, rawURL, name string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, ck := range jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}
