//go:build darwin

package tun

import (
	"net/netip"
	"os/exec"
	"strings"
)

// DefaultGateway 通过 `route -n get default` 查询默认网关。
func (systemRouteTable) DefaultGateway() (netip.Addr, bool, error) {
	out, err := exec.Command("route", "-n", "get", "default").CombinedOutput()
	if err != nil {
		return netip.Addr{}, false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "gateway:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if addr, err := netip.ParseAddr(fields[1]); err == nil {
				return addr, true, nil
			}
		}
	}
	return netip.Addr{}, false, nil
}

// HasHostRoute 枚举 `netstat -rn -f inet` 路由表，按目的字段精确匹配 /32 主机路由。
// 不能用 `route get <ip>`：它是最长前缀查找，任何目的地都会命中默认路由，
// 无法区分「存在精确 /32」与「经默认网关可达」。
func (systemRouteTable) HasHostRoute(ip netip.Addr) (bool, error) {
	out, err := exec.Command("netstat", "-rn", "-f", "inet").CombinedOutput()
	if err != nil {
		return false, err
	}
	return parseDarwinHostRoute(string(out), ip), nil
}

// parseDarwinHostRoute 判断 netstat -rn -f inet 输出中是否存在指向 ip 的主机路由。
// 目的字段有两种形态：`route add -host` 安装的静态路由（GTun 自己与其他 VPN 的 /32 均属此类）
// 显示为 <ip>/32，内核学习条目（ARP/接口主机路由）显示为裸 <ip>——两者都算存在主机路由。
// 网络路由的目的字段带非 /32 前缀或为短形态（如 192.168.31）、default，不匹配。
func parseDarwinHostRoute(out string, ip netip.Addr) bool {
	bare := ip.String()
	withPrefix := bare + "/32"
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == bare || fields[0] == withPrefix) {
			return true
		}
	}
	return false
}
