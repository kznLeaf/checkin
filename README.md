## 用法

```bash
# 自动获取登录code ticket并完成打卡
# 如果已经扫码登录过应用宝，会尽可能复用已有的token，不需要重复登录
go run . 
```
## Caution

- 如果 `-refresh` 报 `token 续期被拒绝 ... 请重新扫码`，说明 refresh token 已失效，重新扫码一次即可恢复。
