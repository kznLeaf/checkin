## 获取 login buffer

1. 取 uuid：GET 应用宝的 qrconnect 授权页（appid=wxd44977328b36e647，scope 含 snsapi_runtime_pcsdk），从 HTML 里正则抓 `/connect/qrcode/<uuid>`。 
2. 打印二维码：内容就是 `https://open.weixin.qq.com/connect/qrcode/<uuid>` ，用你给的 ToSmallString(false) 渲染到终端，同时把链接也打出来。
3. 长轮询：`long.open.weixin.qq.com/connect/l/qrconnect?uuid=…`，按 `wx_errcode` 分支：408 未扫、404 已扫待确认、405 确认并下发 wx_code、403 取消、402 过期；5 分钟超时。
4. 换 cookie：拿 `wx_code` 回 pcyyb_oauth，同一个 cookie jar 里捞出 openid / accesstoken / refreshtoken。
5. 换 login_buffer：`POST pcyyb_get_wx_login_buffer_auth`，带 Ual-Access-* 头（签名 = md5(body + 时间戳 + key + nonce)），从 `ext_info.list_s.login_buffer.value[0]` 取出 buffer。 之后 main 会自动接着 GetCode（HTTPDNS → 握手 → manualauth → 0-RTT js-login）和 GetCheckinTicket。

## 自动登录微信 → 取 code 的逻辑

整条链路只有 6 段，核心是"login_buffer 是登录态，MMTLS 是通道，js-login 是取码接口"：

| 阶段 | 做什么 | 关键数据 |
| --- | --- | --- |
| 0 一次性 | 应用宝扫码 → OAuth 回调拿到 `openid/accesstoken/refreshtoken` → 换 `login_buffer` | 人工只在这一步 |
| 1 解析 | `login_buffer` 是 protobuf：`field1=ticket`、`field2=device_id`、`field3=host_appid` | `loginMeta` |
| 2 寻址 | 微信 HTTPDNS（`aedns.weixin.qq.com`）拿 `longcloud`（mmtls）与 `shortcloud`（http）的 IP:Port | 全直连 IP，不走系统 DNS |
| 3 握手 | 长连接 MMTLS 1-RTT ECDHE：ClientHello → ServerHello → CertVerify（固定公钥验签）→ NewSessionTicket | 得到 `PSK_ACCESS`（type=1） |
| 4 登录 | 同一条连接上发 `manualauth`（cmd `0x0d7d`），请求体是 HybridEcdh（AES-GCM + LZ4）加密的 protobuf | 换回 `UIN`、`send_key`、`recv_key`、`F9`、`ticket` |
| 5 取码 | 短连接 0-RTT：PSK ClientHello + early data，外层是 transfer 信封（`/ilink/ilinkapp/mp/wxaruntime_transfer`, cmd `0x0b41`），内层是 `js-login`（cmd `1029`, `/cgi-bin/mmbiz-bin/js-login`） | `code = 响应 field2.field3` |
| 6 换票 | `code` → `POST /platform-app/api/auth/checkin-ticket` | `ticket`（43 位 base64url） |

约束没有变：code 与 **appid + 该账号 openid** 绑定、5 分钟有效、一次性；所以一次打卡链路要取两次 code（login + ticket）。

### 代码结构

| 文件 | 职责 |
| --- | --- |
| [code.go](mini/code.go#L34) | `GetCode` 一条直线：①解析 → ②HTTPDNS → ③握手 → ④manualauth → ⑤0-RTT js-login；`manualAuth`（[code.go:110](mini/code.go#L110)）紧跟其后 |
| [ticket.go](mini/ticket.go#L58) | `GetCheckinTicket`：POST JSON、带 `platform: mp-weixin` 与可选 `Cookie: SESSION=…`，按 `{ok,code,msg,data.ticket}` 解析并校验 43 位 base64url |
| [main.go](mini/main.go#L35) | CLI：`-buffer`（文件或 base64）、`-appid`、`-session`、`-code-only`；先打印 code 再取 ticket |
| [transport.go](mini/transport.go#L68) | `Target`、最简 `dialTCP`（直连，代理参数忽略）、HTTPDNS 解析（含已知 IP 兜底） |
| `wire_*.go` | 6 个文件，协议原语（记录/握手/PSK/HKDF/AES-GCM/protobuf/LZ4/shortlink/transfer），从 `YYB_GO` **原样搬入**、只删了手机号与云函数相关的构建函数 |

## refresh功能新增/改动

| 文件 | 内容 |
| --- | --- |
| [token.go](mini/token.go)（新增） | `OAuthTokens` 类型 + 读写（0600）、`RefreshLoginBuffer`（[token.go:77](mini/token.go#L77)）、`refreshAccessToken`（[token.go:95](mini/token.go#L95)）、`fetchLoginBuffer`（[token.go:161](mini/token.go#L161)） |
| [login.go](mini/login.go#L76) | 扫码成功后不再丢弃 cookie：返回 `OAuthTokens` + `login_buffer`，换 buffer 的请求改由 `fetchLoginBuffer` 统一发 |
| [main.go](mini/main.go#L48) | 新增 `-refresh`、`-token-file`（默认 `token.json`）；扫码后自动保存 token；`-refresh` 时走续期路径 |

### 逻辑

```
login_buffer 来源优先级：-buffer  >  -refresh(token 文件)  >  扫码
```

`-refresh` 内部（[token.go:77](mini/token.go#L77)）：

1. 读 `token.json`；
2. 若 `accesstoken` 缺失、或 `expires_at` 距现在不足 5 分钟 → 调 `pcyyb_refresh_token_auth`（`userInfo{openId, refreshToken, accessToken, loginType}`），拿新 `access_token`，**同时把可能被轮换的 `refresh_token` 和新 `expires_at` 写回文件**；
3. 用有效 token 调 `pcyyb_get_wx_login_buffer_auth` 重取 `login_buffer`；
4. access token 还没到期就跳过第 2 步，直接重取 buffer。

两个请求共用 `newYYBRequest`，带 `Ual-Access-Businessid/Timestamp/Nonce/Signature`（`md5(body + ts + key + nonce)`）。

## 用法

```bash
# 第一次：扫码（同时把 token 写进 token.json）
go run . -session <SESSION>

# 之后：免扫码，只续期 + 重取 buffer
go run . -refresh -session <SESSION>

# 日常打卡
go run . -refresh -session <SESSION> -checkin -lat 30.7512 -lng 103.9331
```

## 验证情况

- `go build` / `go vet` / `gofmt` 通过；
- 错误路径实测：缺 token 文件、缺 `refreshtoken` 都有明确提示，不会去发请求；
- **续期链路实测到了真实接口**：用假 token 触发 `fetchLoginBuffer`，服务端返回 `code=-101 ... [40001] [invalid credential, access_token is invalid or not latest]`——说明 UAL 签名、Cookie、JSON 体、响应解析这条链路是被服务端正常受理的（只是凭证是假的）。用真实 token 时应能直接拿到 buffer；
- 没验证的只有真 token 的续期成功路径（需要你扫码后跑一次 `-refresh`）。

## Caution

- `token.json` 等价于账号密码（含 refresh token），已按 0600 写入；别提交到仓库，也不要贴出来。
- 如果 `-refresh` 报 `token 续期被拒绝 ... 请重新扫码`，说明 refresh token 已失效（长期不用/被轮换后丢失），重新扫码一次即可恢复。


