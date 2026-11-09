// Package mmtls 是微信 MMTLS 协议的最小实现，只暴露 GetCode 方法
//
// 文件划分：
//
//	wire_mmtls_protocol.go  记录层 / 握手报文编解码
//	wire_mmtls_crypto.go    HKDF、AES-GCM 记录加解密、曲线原语
//	wire_bytes.go           protobuf 变长字段与 LZ4 编解码
//	wire_ilink.go           login_buffer、manualauth、transfer 信封、js-login 明文
//	wire_shortlink.go       0-RTT PSK 短连接
//	transport.go       HTTPDNS 寻址与最简 TCP 拨号
//	code.go            对外入口 GetCode
package mmtls
