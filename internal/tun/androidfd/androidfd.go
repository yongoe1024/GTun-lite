// Package androidfd 把 Android VpnService.establish() 返回的 TUN 文件描述符
// 适配成 tun.Device 与 tun.Opener。
//
// 与 mac/linux/win 三个平台实现的本质差异：这个平台的 TUN 不是本包自己打开
// 的。establish() 必须由宿主壳层在拿到虚拟 IP 之后调用——VPN 授权框也只能由
// 宿主弹出——所以 Open 的时序是「内核要 fd → 同步回调宿主 → 宿主 establish
// → 返回 fd」。前提：授权在会话启动前由宿主解决，establish 不做 UI 操作，
// 同步等待无阻塞风险。fd 是一次性资源：os.File 关闭即失效，每次栈重建都
// 重新走一遍交付。
//
// 路由与地址由 VpnService.Builder 声明式下发（随 VPN 接口生灭），本包不做
// 任何系统路由操作；RouteCleanup 只负责关闭设备。
package androidfd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// TunRequester 是宿主壳层实现的 TUN 交付回调（gomobile 导出接口，Android 端
// 由 VpnService 实现）。
type TunRequester interface {
	// RequestTun 请求宿主建立 VPN 接口并返回其文件描述符：mtu 与 localIP
	// 原样交给 VpnService.Builder，peers 是逗号分隔的对端虚拟 IP 列表
	// （逐个 /32 路由）。宿主完成 establish 后对 ParcelFileDescriptor 调用
	// detachFd 并返回裸 fd（所有权随之移交本包）；失败返回非 nil error
	// （Java 侧抛异常），原因随错误如实上报。
	RequestTun(mtu int64, localIP string, peers string) (int64, error)
}

// requester 是当前注册的宿主回调。写在会话 goroutine（gtunlib.run 装配时），
// 读在其派生的控制面读循环 goroutine（Open 经 ApplyConfig/HandleConnect 进入）；
// go 语句建立 happens-before（Go 内存模型），无需加锁。若调用结构改变
// （如注册与开栈落在无派生关系的 goroutine），须重新评估同步方式。
var requester TunRequester

// SetTunRequester 注册宿主回调。会话启动时调用；覆盖式。
func SetTunRequester(r TunRequester) { requester = r }

// Device 是基于既有 fd 的 TUN 设备。Android 的 VpnService fd 读写裸 IPv4 包，
// 无任何平台前缀，是全平台最薄的 Device 实现。
type Device struct {
	f    *os.File
	name string
}

// NewDevice 包装一个 VpnService fd。fd 所有权归 Device，Close 即关闭。
func NewDevice(fd int, name string) *Device {
	return &Device{f: os.NewFile(uintptr(fd), "gtun-tun"), name: name}
}

// Read 读取一个裸 IPv4 包。
func (device *Device) Read(buffer []byte) (int, error) { return device.f.Read(buffer) }

// Write 写入一个裸 IPv4 包。
func (device *Device) Write(packet []byte) (int, error) { return device.f.Write(packet) }

// Name 返回请求的接口名（仅供日志；Android 无真实接口名可查）。
func (device *Device) Name() string { return device.name }

// Close 关闭 fd。幂等。
func (device *Device) Close() error { return device.f.Close() }

// Opener 实现 tun.Opener：向宿主同步请求 fd 并包装为 Device。
type Opener struct{}

// Open 同步获取 fd 并包装为 Device。
func (Opener) Open(ctx context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	_ = ctx
	r := requester
	if r == nil {
		return nil, tun.RouteCleanup{}, errors.New("android tun requester not set")
	}
	fdValue, err := requester.RequestTun(int64(mtu), string(localIP), joinIPs(peers))
	if err != nil {
		return nil, tun.RouteCleanup{}, fmt.Errorf("host establish: %w", err)
	}
	if fdValue <= 0 || fdValue > 1<<31-1 {
		return nil, tun.RouteCleanup{}, fmt.Errorf("host returned invalid tun fd %d", fdValue)
	}
	device := NewDevice(int(fdValue), name)
	// 路由由 VpnService 声明式管理，无系统资源可回滚；清理即关设备。
	cleanup := tun.NewRouteCleanup(name, hostRouteEntries(name, peers), func() error {
		return device.Close()
	})
	return device, cleanup, nil
}

// joinIPs 把对端虚拟 IP 拼成逗号分隔串（跨 gomobile 边界用单字符串）。
func joinIPs(ips []common.IPv4) string {
	parts := make([]string, len(ips))
	for i, ip := range ips {
		parts[i] = string(ip)
	}
	return strings.Join(parts, ",")
}

// hostRouteEntries 描述本栈名义上的 /32 路由清单。Android 上没有真实系统路由
// 可操作，清单仅供 RouteCleanup 的字段完整（manager 日志可读）。
func hostRouteEntries(name string, peers []common.IPv4) []tun.RouteEntry {
	entries := make([]tun.RouteEntry, 0, len(peers))
	for _, peer := range peers {
		entries = append(entries, tun.RouteEntry{Destination: peer, Interface: name})
	}
	return entries
}
