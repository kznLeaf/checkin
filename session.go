package main

// 用配置文件中的学号和密码登录学校统一身份认证平台（idas.uestc.edu.cn），
// 换取 SESSION cookie，即用户个人的身份标识。
// 服务端通过该 cookie 识别用户身份，因此在打卡请求中需要携带该cookie。
//
//	① 请求统一身份认证页面 GET  /authserver/login?service=https://jzsz.uestc.edu.cn/
//    提取 execution / pwdEncryptSalt，这两个字符串用于提交登录表单;
//	② 在请求中携带cookie: MULTIFACTOR_BROWSER_FINGERPRINT（针对 idas 域名），以便绕过二次验证，
//    根据固定的算法加密密码后提交表单;
//	③ 请求上一步 302 的目标地址，从 cookie 里取 session cookie
//	④ GET  /api/isLoginFailure?_t=<毫秒>  激活 session

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

	// CAS 前端加密用的字符集，来自 encrypt.js
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

	// 携带 MULTIFACTOR_BROWSER_FINGERPRINT 访问 idas，绕过二次验证
	idasURL, _ := url.Parse(idasHost + "/")
	jar.SetCookies(idasURL, []*http.Cookie{{
		Name:  "MULTIFACTOR_BROWSER_FINGERPRINT",
		Value: cfg.BrowserFingerprint,
		Path:  "/",
	}})

	// ② 加密密码并提交登录表单
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

	// location 形如 https://jzsz.uestc.edu.cn/?ticket=xxxxxxx
	location := resp.Header.Get("Location")
	fmt.Printf("登录成功，获得重定向URL: %s\n", location)

	// ③ 跟随重定向换取业务平台 SESSION
	// 必须向重定向指向的目标 URL 发送一次请求， 否则下一步可能拿不到SESSION
	fmt.Println("[3/4] 正在跟随重定向获取SESSION...")
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
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

	// ④真实用户通过浏览器访问 jzsz 时会先向 /api/isLoginFailure 发送请求
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

func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", loginUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,en-US;q=0.9,en;q=0.8")
}

// encryptPassword 对用户密码进行加密。
//
// 加密算法来自于访问身份认证页面时服务端发过来的 encrypt.js:
//  1. 如果加密密钥(即salt)为空，直接返回原始密码 n
//  2. 如果密钥不为空，生成一个长度为 64 的随机字符串，拼接在密码前面
//  3. IV 是随机生成的长度 16 的字符串，因为真实密码从第五个 block 才开始， 所以IV根本不重要，占位即可
//  4. 使用 AES-CBC 模式和 PKCS#7 填充加密
//  5. 输出 Base64 编码的密文
//  6. 如果加密过程中发生异常，encryptPassword 会捕获异常并返回原始密码
//
// 由于 AES CBC 的特性, 第 N 块密文的原始文本 = 第 N 块解密之后的数据块与 N-1 密文块异或
// 所以无需知道 IV 和密码前的随机字符串也可以实现解密.
//
// salt 来自于登录页面的隐藏html元素，使用正则匹配就能获取
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

// firstGroup 返回正则第一个匹配到的字符串组，没有匹配时返回空串。
func firstGroup(re *regexp.Regexp, data []byte) string {
	if m := re.FindSubmatch(data); len(m) == 2 {
		return string(m[1])
	}
	return ""
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
