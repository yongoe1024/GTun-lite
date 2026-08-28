//go:build windows

package tun

import (
	"net"
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
	for _, fields := range rows {
		if fields[0] == ip.String() && fields[1] == "255.255.255.255" {
			return true, nil
		}
	}
	return false, nil
}

// HostRouteDangling 判定 /32 是否悬空。route print 的 Interface 列是路由
// 所属接口的本机地址：没有任何现存接口持有该地址（适配器已被拆除）即为
// 悬空。列不全是 IP（解析不了）时保守返回 false，交由冲突检查如实报错。
func (systemRouteTable) HostRouteDangling(ip netip.Addr) (bool, error) {
	rows, err := parseRoutePrint()
	if err != nil {
		return false, err
	}
	for _, fields := range rows {
		if fields[0] != ip.String() || fields[1] != "255.255.255.255" || len(fields) < 4 {
			continue
		}
		ifaceAddr, err := netip.ParseAddr(fields[3])
		if err != nil {
			continue
		}
		if !interfaceAddressExists(ifaceAddr) {
			return true, nil
		}
	}
	return false, nil
}

// interfaceAddressExists 检查是否仍有接口持有指定 IPv4 地址。
func interfaceAddressExists(addr netip.Addr) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true // 查询失败时保守视为存在：不清理、交冲突检查
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			prefix, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v := prefix.IP.To4(); v != nil && netip.AddrFrom4([4]byte(v)) == addr {
				return true
			}
		}
	}
	return false
}

// DeleteHostRoute 删除 /32 主机路由（route delete）。
func (systemRouteTable) DeleteHostRoute(ip netip.Addr) error {
	return exec.Command("route", "delete", ip.String()).Run()
}
