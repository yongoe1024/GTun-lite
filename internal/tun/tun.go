// Package tun 定义与平台无关的 TUN 设备、路由适配与数据面边界。
//
// 平台子目录 linux/mac/win 各自实现 Device 与 Opener；internal/tun 只持有接口、
// route preflight 与跨平台的数据面逻辑。internal/tun 不持有业务状态、不创建业务
// goroutine；TUN、路由与网关/NAT 规则变更全部由客户端 manager owner 串行调用。
package tun

import (
	"context"
	"net/netip"

	"gtun-lite/internal/common"
)

// Device 是平台无关的 TUN 设备，读写原始 IPv4 包（无地址族头等平台前缀，
// 平台实现内部处理 utun 4 字节地址族头等差异）。
type Device interface {
	// Read 从 TUN 读取一个原始 IPv4 包到 buffer，返回包长度。
	Read(buffer []byte) (int, error)
	// Write 向 TUN 写入一个原始 IPv4 包，返回写入长度。
	Write(packet []byte) (int, error)
	// Name 返回实际接口名（Linux 是请求名或内核分配名；macOS 是 utunN；
	// Windows 是 Wintun adapter 名）。供 manager 精确回滚与日志。
	Name() string
	// Close 关闭设备并释放平台资源。幂等。
	Close() error
}

// RouteEntry 描述一次需要安装或回滚的 /32 路由。
type RouteEntry struct {
	// Destination 是对端虚拟 IP，安装为 /32 主机路由。
	Destination common.IPv4
	// Interface 是安装该路由的实际接口名。
	Interface string
}

// RouteCleanup 是 Apply 成功后返回的资源清单与回滚句柄。manager 在平台操作失败或
// 后续配置替换时按精确清单回滚已创建资源。Close 必须幂等。
type RouteCleanup struct {
	// Interface 是已创建的实际接口名。
	Interface string
	// Routes 是已安装的 /32 路由，回滚时逐条删除。
	Routes []RouteEntry
	// closeImpl 由平台实现填充，执行实际关闭与路由删除。
	closeImpl func() error
}

// NewRouteCleanup 构造带平台回滚函数的资源清单。平台实现通过此函数注入 closeImpl，
// 避免直接访问未导出字段。
func NewRouteCleanup(iface string, routes []RouteEntry, closeImpl func() error) RouteCleanup {
	return RouteCleanup{Interface: iface, Routes: routes, closeImpl: closeImpl}
}

// Close 执行平台资源回滚。幂等。
func (cleanup RouteCleanup) Close() error {
	if cleanup.closeImpl != nil {
		return cleanup.closeImpl()
	}
	return nil
}

// Opener 在指定平台打开并配置一个 TUN 设备。
// name 是请求的接口名（macOS 仅作日志前缀，实际为 utunN；Windows 是 Wintun 名；
// Linux 是 /dev/net/tun 请求名）。localIP 是本机虚拟 IP，按 /32 或点对点配置。
// peers 是需要安装 /32 路由的对端虚拟 IP 列表。返回实际设备、资源清单与回滚句柄。
type Opener interface {
	Open(ctx context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (Device, RouteCleanup, error)
}

// PreflightInput 是 route preflight 的不可变输入。
type PreflightInput struct {
	// LocalIP 是本机虚拟 IP，将作为 /32 安装。
	LocalIP common.IPv4
	// Peers 是需要安装 /32 路由的对端虚拟 IP。
	Peers []common.IPv4
	// ServerIP 是固定服务器 IPv4。
	ServerIP common.IPv4
}

// RouteTable 抽象系统路由表的读取与残留清理，供 preflight 使用。
// 查询结果按 Go netip.Addr 返回，平台实现用命令填充。
type RouteTable interface {
	// DefaultGateway 返回默认网关地址（无默认路由返回 false）。
	DefaultGateway() (netip.Addr, bool, error)
	// LocalAddresses 返回本机非回环单播 IPv4 地址。
	LocalAddresses() ([]netip.Addr, error)
	// HasHostRoute 返回指定 /32 主机路由是否已存在。
	HasHostRoute(ip netip.Addr) (exists bool, err error)
	// HostRouteDangling 报告 ip 的 /32 是否指向已不存在的接口——异常
	// 退出（崩溃/强杀/断电）残留的零歧义特征。无此路由或无法判定
	// （解析失败、信息不全）时一律返回 false，交由冲突检查如实报错。
	HostRouteDangling(ip netip.Addr) (bool, error)
	// DeleteHostRoute 删除 ip 的 /32 主机路由。仅用于清理悬空残留；
	// 指向活跃接口的 /32 必须保留给 Preflight 如实报冲突。
	DeleteHostRoute(ip netip.Addr) error
}
