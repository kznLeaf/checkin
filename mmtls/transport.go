package mmtls

// 本文件只做两件事：把微信域名解析成可连接的 IP:Port（HTTPDNS），以及提供最简的 TCP 拨号。
// 原实现里的 SOCKS5 / HTTP CONNECT 代理、DNS 缓存、并发控制在这里全部去掉。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// target 是一个可直连的服务器地址。
type target struct {
	IP   string
	Port int
}

const (
	// 微信 HTTPDNS 入口：返回 longcloud / shortcloud 等域名的 IP 与协议端口。
	newdnsHost     = "aedns.weixin.qq.com"
	newdnsBackupIP = "180.153.202.85"
	newdnsPath     = "/cgi-bin/default/getdns"

	longlinkDomain  = "longcloud.weixin.com"  // MMTLS 长连接（握手 + manualauth）
	shortlinkDomain = "shortcloud.weixin.com" // 0-RTT 短连接（发 CGI）
	protoMMTLS      = "mmtlsovertcp"
	protoHTTP       = "http"

	shortlinkFallbackIP = "120.241.131.173" // HTTPDNS 失败时使用的已知短连接地址
)

// dialTCP 是最简拨号：忽略代理参数，只做直连。
// 保留与协议层相同的签名，是为了让拷贝进来的 wire_*.go 不用改动。
func dialTCP(ctx context.Context, host string, port int, timeout time.Duration, _ string, _ bool) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// dnsResponse 只声明用得到的字段，其余忽略。
type dnsResponse struct {
	DNS struct {
		Retcode    int `json:"retcode"`
		DomainList []struct {
			Name string `json:"name"`
			IPs  []struct {
				IP string `json:"ip"`
			} `json:"iplist"`
			Protocols []struct {
				Name  string `json:"name"`
				Ports []int  `json:"portlist"`
			} `json:"protocollist"`
		} `json:"domainlist"`
	} `json:"dns"`
}

// resolveTargets 通过微信 HTTPDNS 取得长连接与短连接的目标列表。
//
// 返回的顺序就是后续尝试连接的顺序：长连接按 8080 > 80 > 443 > 5000 的端口优先级排，
// 短连接只要 80 端口。HTTPDNS 走不通时短连接回落到已知 IP，长连接只能报错。
func resolveTargets(ctx context.Context, timeout time.Duration) (longTargets, shortTargets []target, err error) {
	query := url.Values{
		"clientversion": {"0"},
		"devicetype":    {"Windows"},
		"uin":           {"0"},
		"format":        {"json"},
	}.Encode()

	body, err := fetchNewDNS(ctx, newdnsPath+"?"+query, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("HTTPDNS 请求失败: %w", err)
	}

	var resp dnsResponse
	if err = json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("HTTPDNS 响应解析失败: %w", err)
	}
	if resp.DNS.Retcode != 0 {
		return nil, nil, fmt.Errorf("HTTPDNS retcode=%d", resp.DNS.Retcode)
	}

	for _, domain := range resp.DNS.DomainList {
		var ports []int
		for _, p := range domain.Protocols {
			if (domain.Name == longlinkDomain && p.Name == protoMMTLS) ||
				(domain.Name == shortlinkDomain && p.Name == protoHTTP) {
				ports = append(ports, p.Ports...)
			}
		}
		for _, ip := range domain.IPs {
			for _, port := range ports {
				if domain.Name == shortlinkDomain && port != 80 {
					continue
				}
				t := target{IP: ip.IP, Port: port}
				if domain.Name == longlinkDomain {
					longTargets = append(longTargets, t)
				} else {
					shortTargets = append(shortTargets, t)
				}
			}
		}
	}

	// 长连接：优先常见端口，最多留 6 个候选。
	preference := []int{8080, 80, 443, 5000}
	sort.SliceStable(longTargets, func(i, j int) bool {
		return portRank(longTargets[i].Port, preference) < portRank(longTargets[j].Port, preference)
	})
	if len(longTargets) > 6 {
		longTargets = longTargets[:6]
	}
	if len(longTargets) == 0 {
		return nil, nil, fmt.Errorf("HTTPDNS 未返回 %s 的 %s 地址", longlinkDomain, protoMMTLS)
	}

	if len(shortTargets) == 0 {
		shortTargets = []target{{IP: shortlinkFallbackIP, Port: 80}}
	}
	return longTargets, shortTargets, nil
}

// portRank 用于按偏好排序端口，未列出的端口排在最后。
func portRank(port int, preference []int) int {
	for i, p := range preference {
		if p == port {
			return i
		}
	}
	return len(preference)
}

// fetchNewDNS 先按域名请求，失败再用已知 IP 兜底（请求头里仍然带域名）。
func fetchNewDNS(ctx context.Context, pathAndQuery string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+newdnsHost+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MicroMessenger Client")
	req.Header.Set("Accept", "*/*")
	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		if body, err := io.ReadAll(resp.Body); err == nil && resp.StatusCode == http.StatusOK {
			return body, nil
		}
	}

	conn, err := dialTCP(ctx, newdnsBackupIP, 80, timeout, "", true)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	raw := fmt.Sprintf("GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: MicroMessenger Client\r\nAccept: */*\r\nConnection: close\r\n\r\n", pathAndQuery, newdnsHost)
	if _, err = conn.Write([]byte(raw)); err != nil {
		return nil, err
	}
	all, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	_, body := splitHTTP(all)
	if len(body) == 0 {
		return nil, fmt.Errorf("HTTPDNS 响应缺少正文")
	}
	return body, nil
}

// splitHTTP 把裸 HTTP 响应切成头部与正文；短连接的 0-RTT 响应就是这种形式。
func splitHTTP(raw []byte) (string, []byte) {
	before, after, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		return "", raw
	}
	return string(before), after
}
