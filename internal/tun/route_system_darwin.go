//go:build darwin

package tun

import (
	"net"
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
	_, found := parseDarwinHostRoute(string(out), ip)
	return found, nil
}

// parseDarwinHostRoute 判断 netstat -rn -f inet 输出中是否存在指向 ip 的主机路由，
// 存在时一并返回该路由绑定的接口名（Netif 列）。
// 目的字段有两种形态：`route add -host` 安装的静态路由（GTun 自己与其他 VPN 的 /32 均属此类）
// 显示为 <ip>/32，内核学习条目（ARP/接口主机路由）显示为裸 <ip>——两者都算存在主机路由。
// 网络路由的目的字段带非 /32 前缀或为短形态（如 192.168.31）、default，不匹配。
// Netif 列位置按表头动态定位：现代输出为 Destination Gateway Flags Netif Expire，
// 含 Refs/Use 列的旧格式会右移。
func parseDarwinHostRoute(out string, ip netip.Addr) (string, bool) {
	bare := ip.String()
	withPrefix := bare + "/32"
	netifIndex := 3
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == bare || fields[0] == withPrefix) {
			iface := ""
			if netifIndex < len(fields) {
				iface = fields[netifIndex]
			}
			return iface, true
		}
		if len(fields) >= 4 && fields[0] == "Destination" {
			for i, f := range fields {
				if f == "Netif" {
					netifIndex = i
				}
			}
		}
	}
	return "", false
}

// HostRouteDangling 判定 /32 是否悬空：netstat 行的 Netif 接口已不存在。
// 接口名缺失（列不全、解析不了）时保守返回 false，交由冲突检查如实报错。
func (systemRouteTable) HostRouteDangling(ip netip.Addr) (bool, error) {
	out, err := exec.Command("netstat", "-rn", "-f", "inet").CombinedOutput()
	if err != nil {
		return false, err
	}
	iface, found := parseDarwinHostRoute(string(out), ip)
	if !found || iface == "" {
		return false, nil
	}
	_, err = net.InterfaceByName(iface)
	return err != nil, nil
}

// DeleteHostRoute 删除 /32 主机路由（route -q delete -host，与回滚同款命令）。
func (systemRouteTable) DeleteHostRoute(ip netip.Addr) error {
	return exec.Command("route", "-q", "delete", "-host", ip.String()).Run()
}
