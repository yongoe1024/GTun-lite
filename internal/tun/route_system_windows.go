//go:build windows

package tun

import (
	"net/netip"
	"os/exec"
	"strings"
)

// parseRoutePrint 执行 `route print -4` 并返回有效路由行（网络目的、掩码、网关三列）。
// 识别方式与语言无关：仅接受前两列均能解析为 IPv4 的行，表头与分隔线自然被跳过；
// 网关列可能是 "On-link"，由调用方自行决定是否解析。
func parseRoutePrint() ([][]string, error) {
	out, err := exec.Command("route", "print", "-4").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if _, err := netip.ParseAddr(fields[0]); err != nil {
			continue
		}
		if _, err := netip.ParseAddr(fields[1]); err != nil {
			continue
		}
		rows = append(rows, fields)
	}
	return rows, nil
}

// DefaultGateway 从 `route print -4` 解析 0.0.0.0/0 路由的网关列。
func (systemRouteTable) DefaultGateway() (netip.Addr, bool, error) {
	rows, err := parseRoutePrint()
	if err != nil {
		return netip.Addr{}, false, err
	}
	for _, fields := range rows {
		if fields[0] != "0.0.0.0" || fields[1] != "0.0.0.0" {
			continue
		}
		gateway, err := netip.ParseAddr(fields[2])
		if err != nil {
			continue
		}
		return gateway, true, nil
	}
	return netip.Addr{}, false, nil
}

// HasHostRoute 从 `route print -4` 查找目的等于 ip、掩码 255.255.255.255 的 /32 主机路由。
func (systemRouteTable) HasHostRoute(ip netip.Addr) (bool, error) {
	rows, err := parseRoutePrint()
	if err != nil {
		return false, err
	}
	return windowsRowsHaveHostRoute(rows, ip), nil
}

// HostRouteDangling 判定 /32 是否悬空：适配器已拆除的崩溃残留。
// 判定逻辑见 windowsRowsDangling（纯函数，含真实输出样例测试）。
func (systemRouteTable) HostRouteDangling(ip netip.Addr) (bool, error) {
	rows, err := parseRoutePrint()
	if err != nil {
		return false, err
	}
	return windowsRowsDangling(rows, ip), nil
}

// DeleteHostRoute 删除 /32 主机路由（route delete）。
func (systemRouteTable) DeleteHostRoute(ip netip.Addr) error {
	return exec.Command("route", "delete", ip.String()).Run()
}
