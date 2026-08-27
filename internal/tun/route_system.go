package tun

import (
	"net"
	"net/netip"
)

// systemRouteTable 用平台命令查询系统路由表，供 route preflight。
// 平台差异（DefaultGateway/HasHostRoute）在 route_system_{darwin,linux,windows}.go
// 按 build tag 提供，与 TUN 设备实现的目录隔离方式同构。
type systemRouteTable struct{}

// NewSystemRouteTable 返回当前平台的真实路由表查询器。
func NewSystemRouteTable() RouteTable { return systemRouteTable{} }

// LocalAddresses 返回本机非回环 IPv4 单播地址。
func (systemRouteTable) LocalAddresses() ([]netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ifaceAddrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip.To4())
			if !ok || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
				continue
			}
			addrs = append(addrs, parsed.Unmap())
		}
	}
	return addrs, nil
}
