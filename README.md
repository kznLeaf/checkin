<p  align="center">
    <img src="./docs/logo/logo.png" alt="Quilkin logo" height="220">
</p>

用于自动完成晚安成电打卡的命令行工具。

## 用法

运行之前需要填写配置文件，重命名本工程目录下`config.yaml.example`为`config.yaml`即可。这些信息用于自动登录获取`session cookie`和打卡上报信息，具体来说：

- `student_id`，`password`，`browser_fingerprint`: 自动完成登录，并绕过手机发码验证。

> browser_fingerprint 的获取方式：在 PC 端打开 F12 切换到 network 标签，访问 https://jzsz.uestc.edu.cn 找到发往 `https://idas.uestc.edu.cn/authserver/login` 的请求，在请求携带的 Cookies 中找到`MULTIFACTOR_BROWSER_FINGERPRINT`，复制其值。

- `position`：上报给打卡接口
- `time`: 预留，用于自动定时打卡，这个功能还没有实现，留个占位符即可。
- `mmwebid`: 三位或四位数字，用于 UA 头部，随便填写一个固定的数字即可。
- `telegram_bot`(可选): 输入 Telegram bot 的`token`和`chatID`，获取`chatID`的方法见[这里](https://stackoverflow.com/questions/32423837/telegram-bot-how-to-get-a-group-chat-id)，打卡成功会推送一条 Telegram 通知。

第一次启动程序会在终端打印一个二维码用于登录应用宝，微信扫码之后即可完成整个流程。获取的 token 将保存到项目目录的`token.json`中，此后运行程序只要 token 还没有过期就不需要再扫码登录。如果出现 `token 续期被拒绝 ... 请重新扫码`，说明 refresh token 已失效，重新扫码登录一次即可恢复。

运行程序:

```bash
go run . 
```
