## 用法

运行之前需要填写配置文件，重命名本工程目录下`config.yaml.example`为`config.yaml`即可。这些信息用于自动登录获取`session cookie`和打卡上报信息，具体来说：

- `student_id`，`password`，`browser_fingerprint`: 自动完成登录，并绕过手机发码验证。
- `position`：上报给打卡接口
- `time`: 预留，用于自动定时打卡，这个功能还没有实现，留个占位符即可。
- `mmwebid`: 三位或四位数字，用于 UA 头部，随便填写一个固定的数字即可。

第一次启动程序会在终端打印一个二维码用于登录应用宝，微信扫码之后即可完成整个流程。获取的 token 将保存到项目目录的`token.json`中，此后运行程序只要 token 还没有过期就不需要再扫码登录。如果出现 `token 续期被拒绝 ... 请重新扫码`，说明 refresh token 已失效，重新扫码登录一次即可恢复。

运行程序:

```bash
go run . 
```
