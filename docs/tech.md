## 获取 login buffer

1. 取 uuid：GET 应用宝的 qrconnect 授权页（appid=wxd44977328b36e647，scope 含 snsapi_runtime_pcsdk），从 HTML 里正则抓 `/connect/qrcode/<uuid>`。 
2. 打印二维码：内容就是 `https://open.weixin.qq.com/connect/qrcode/<uuid>`
3. 长轮询：`long.open.weixin.qq.com/connect/l/qrconnect?uuid=…`，按 `wx_errcode` 分支：408 未扫、404 已扫待确认、405 确认并下发 wx_code、403 取消、402 过期；二维码有效时间为5min
4. 换 cookie：拿 `wx_code` 回 pcyyb_oauth，从同一个 cookies 里提取 openid / accesstoken / refreshtoken。
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
