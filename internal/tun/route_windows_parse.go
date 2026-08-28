package tun

import (
	"net"
	"net/netip"
)

// 本文件是 Windows `route print -4` 输出判定的纯逻辑，不持 build tag：
// 便于在任意平台用真实输出样例做单元测试（route_system_windows.go 只留
// 命令执行）。

// windowsRowsHaveHostRoute 判断 route print 行集合中是否存在 ip 的 /32。
func windowsRowsHaveHostRoute(rows [][]string, ip netip.Addr) bool {
	for _, fields := range rows {
		if fields[0] == ip.String() && fields[1] == "255.255.255.255" {
			return true
		}
	}
	return false
}

// windowsRowsDangling 判断 ip 的 /32 是否悬空（绑定接口已不存在）。
//
// Interface 列是路由所属接口的本机地址；适配器被拆除后该列不再能解析为
// 地址——中文系统显示本地化的「默认」，英文系统显示对应的占位词——这是
// 崩溃残留的典型形态。判定：
//   - Interface 列不可解析为 IP → 绑定接口无法定位，按悬空处理
//     （存活接口的该列恒为其本机地址）；
//   - 可解析但没有任何现存接口持有该地址 → 悬空。
func windowsRowsDangling(rows [][]string, ip netip.Addr) bool {
	for _, fields := range rows {
		if fields[0] != ip.String() || fields[1] != "255.255.255.255" || len(fields) < 4 {
			continue
		}
		ifaceAddr, err := netip.ParseAddr(fields[3])
		if err != nil {
			return true
		}
		if !interfaceAddressExists(ifaceAddr) {
			return true
		}
	}
	return false
}

// interfaceAddressExists 检查是否仍有接口持有指定 IPv4 地址。
// 枚举失败时保守视为存在：不清理、交冲突检查。
func interfaceAddressExists(addr netip.Addr) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v := ipNet.IP.To4(); v != nil && netip.AddrFrom4([4]byte(v)) == addr {
				return true
			}
		}
	}
	return false
}
