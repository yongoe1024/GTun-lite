//go:build linux

package tun

import (
	"net"
	"net/netip"
	"os/exec"
	"strings"
)

// DefaultGateway 通过 `ip route show default` 查询默认网关。
func (systemRouteTable) DefaultGateway() (netip.Addr, bool, error) {
	out, err := exec.Command("ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return netip.Addr{}, false, err
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			if addr, err := netip.ParseAddr(fields[i+1]); err == nil {
				return addr, true, nil
			}
		}
	}
	return netip.Addr{}, false, nil
}

// HasHostRoute 用 `ip route show <ip>/32` 查询精确主机路由；仅存在该 /32 时输出非空。
func (systemRouteTable) HasHostRoute(ip netip.Addr) (bool, error) {
	out, err := exec.Command("ip", "route", "show", ip.String()+"/32").CombinedOutput()
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// HostRouteDangling 判定 /32 是否悬空：路由行的 dev 接口已不存在。
// 输出里解析不到 dev（如 via 网关路由）时保守返回 false。
func (systemRouteTable) HostRouteDangling(ip netip.Addr) (bool, error) {
	out, err := exec.Command("ip", "route", "show", ip.String()+"/32").CombinedOutput()
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			_, err := net.InterfaceByName(fields[i+1])
			return err != nil, nil
		}
	}
	return false, nil
}

// DeleteHostRoute 删除 /32 主机路由（与回滚同款命令）。
func (systemRouteTable) DeleteHostRoute(ip netip.Addr) error {
	return exec.Command("ip", "route", "del", ip.String()+"/32").Run()
}
