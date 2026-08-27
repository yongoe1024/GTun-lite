package client

import (
	"fmt"
	"net"
)

// serverIPv4 把配置里的服务器地址（host:port，host 可为 IP 字面量或域名）
// 解析为 IPv4。manager 的路由预检（/32 不得覆盖服务器）与 Worker 的探测
// 目标共用这一份解析——两处语义本就相同（通往服务器的 IPv4），分写两遍
// 只会漂移。
func serverIPv4(addr string) (net.IP, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split server address: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return ip.To4(), nil
	}
	resolved, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return nil, fmt.Errorf("resolve server host: %w", err)
	}
	if ip := resolved.IP.To4(); ip != nil {
		return ip, nil
	}
	return nil, fmt.Errorf("server host %q has no IPv4 address", host)
}
