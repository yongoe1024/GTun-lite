package tun

import (
	"errors"
	"fmt"
	"net/netip"

	"gtun-lite/internal/common"
)

var (
	// ErrRouteConflict 是路由预检全部拒绝原因的哨兵；包装错误携带具体地址与检查项，
	// 供客户端日志直接展示（真实测试中"route conflict"三连前缀无地址曾无法定位）。
	ErrRouteConflict = errors.New("route conflict")
)

// Preflight 在修改路由前检查目标 /32 不与固定服务器、探测端点、默认网关、
// 本机地址、回环/链路本地/多播/广播或已存在的 /32 冲突。静默覆盖任何一个
// 都会把别人的流量引进 TUN 或让控制面失联，因此冲突一律如实报错，
// 调用方不得发布配置或修改数据面。
//
// 不存在「GTun 自身路由」豁免：重应用幂等由 manager 的「拓扑未变不重建
// + 重建前先拆干净」保证，preflight 执行时系统里不应有任何 GTun 路由——
// 此刻存在的 /32 无论来源（其他 VPN、上次异常退出的残留）一律如实报
// 冲突，清理责任交给运维，不静默接管。
func Preflight(input PreflightInput, table RouteTable) error {
	if err := input.LocalIP.Validate(); err != nil {
		return ErrRouteConflict
	}
	// 默认网关与本机地址和具体目标无关，整个 preflight 查一次；
	// /32 存在性随目标逐个查（darwin 上每次是一次全表解析）。
	gateway, gatewayPresent, err := table.DefaultGateway()
	if err != nil {
		return err
	}
	locals, err := table.LocalAddresses()
	if err != nil {
		return err
	}
	targets := append([]common.IPv4{input.LocalIP}, input.Peers...)
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return ErrRouteConflict
		}
		addr, err := netip.ParseAddr(string(target))
		if err != nil || !addr.Is4() {
			return ErrRouteConflict
		}
		if isReserved(addr) {
			return fmt.Errorf("%w: %s is a reserved address", ErrRouteConflict, addr)
		}
		if isServerOrProbeRoute(addr, input.ServerIP) {
			return fmt.Errorf("%w: %s equals the server IP", ErrRouteConflict, addr)
		}
		if err := checkExisting(table, addr, gateway, gatewayPresent, locals); err != nil {
			return err
		}
	}
	return nil
}

// isReserved 拒绝回环、链路本地、多播、广播和未指定地址。
func isReserved(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified()
}

// isServerOrProbeRoute 阻止目标 /32 覆盖服务器公网 IP（探测端点共享同一服务器 IP，
// 五端口仅是端口差异，IP 维度与服务器相同，此处按 IP 判定）。
func isServerOrProbeRoute(addr netip.Addr, serverIP common.IPv4) bool {
	server, err := netip.ParseAddr(string(serverIP))
	if err != nil {
		return false
	}
	return addr == server
}

// checkExisting 拒绝覆盖已存在的 /32、默认网关与本机地址。网关与本机地址
// 由调用方查好传入（与目标无关，见 Preflight）。table 为 nil 属装配错误：
// 不做防御检查，nil 方法调用直接 panic 暴露问题。
func checkExisting(table RouteTable, addr netip.Addr, gateway netip.Addr, gatewayPresent bool, locals []netip.Addr) error {
	exists, err := table.HasHostRoute(addr)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s already has a host route", ErrRouteConflict, addr)
	}
	if gatewayPresent && gateway == addr {
		return fmt.Errorf("%w: %s is the default gateway", ErrRouteConflict, addr)
	}
	for _, local := range locals {
		if local == addr {
			return fmt.Errorf("%w: %s is already a local address", ErrRouteConflict, addr)
		}
	}
	return nil
}
