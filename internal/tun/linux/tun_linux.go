//go:build linux

package linux

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// opener 用 /dev/net/tun 创建 TUN 设备（IFF_TUN|IFF_NO_PI），读写原始 IPv4。
type opener struct{}

// New 返回 Linux 平台的 tun.Opener。
func New() tun.Opener { return opener{} }

const (
	// IFF_TUN|IFF_NO_PI：L3 TUN 设备，不带包信息头（协议字段），数据面直接读原始 IPv4。
	tunFlags = unix.IFF_TUN | unix.IFF_NO_PI
)

// Open 创建并配置 TUN 设备，安装本机 /32 地址与对端 /32 路由。
func (opener) Open(_ context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	if err := localIP.Validate(); err != nil {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid local IP: %w", err)
	}
	if mtu < 20 || mtu > common.MaxTUNMTU {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid MTU %d", mtu)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, tun.RouteCleanup{}, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	// 请求接口名：留空则内核分配。失败时关闭 fd 不泄漏。
	request := ifreq{flags: tunFlags}
	copy(request.name[:], name)
	if err := unix.IoctlSetInt(fd, unix.TUNSETIFF, int(uintptr(unsafe.Pointer(&request)))); err != nil {
		unix.Close(fd)
		return nil, tun.RouteCleanup{}, fmt.Errorf("TUNSETIFF: %w", err)
	}
	actualName := trimName(request.name[:])
	// 必须先置非阻塞再 os.NewFile：os.NewFile 只为非阻塞 fd 注册 runtime poller，
	// Read 走 poller 后 Close 才能唤醒阻塞的 tunReadLoop——真实生命周期验证发现，
	// 阻塞模式的 /dev/net/tun fd 不进 poller，空闲时优雅退出永久卡死在 wg.Wait
	// （与 darwin utun 同源，goroutine 栈证实 read 直通 syscall）。
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, tun.RouteCleanup{}, fmt.Errorf("set tun nonblocking: %w", err)
	}
	file := os.NewFile(uintptr(fd), "/dev/net/tun")
	device := &device{file: file, name: actualName}

	// 配置 /32 地址与 MTU；任一失败回滚已创建资源。
	if err := configureInterface(actualName, mtu, localIP); err != nil {
		device.Close()
		return nil, tun.RouteCleanup{}, fmt.Errorf("configure interface: %w", err)
	}
	routes := []tun.RouteEntry{}
	for _, peer := range peers {
		if err := addHostRoute(actualName, peer); err != nil {
			for _, r := range routes {
				_ = deleteHostRoute(r.Interface, r.Destination)
			}
			device.Close()
			return nil, tun.RouteCleanup{}, fmt.Errorf("add host route %s: %w", peer, err)
		}
		routes = append(routes, tun.RouteEntry{Destination: peer, Interface: actualName})
	}
	cleanup := tun.NewRouteCleanup(actualName, routes, func() error {
		var first error
		for _, route := range routes {
			if err := deleteHostRoute(route.Interface, route.Destination); err != nil && first == nil {
				first = err
			}
		}
		return first
	})
	return device, cleanup, nil
}

type device struct {
	file *os.File
	name string
}

// Read 从 TUN 读一个原始 IPv4 包（IFF_NO_PI：无协议前缀）。fd 已置非阻塞并
// 注册进 runtime poller，无包时阻塞在 poller 上、Close 可唤醒。
func (d *device) Read(buffer []byte) (int, error) { return d.file.Read(buffer) }

// Write 向 TUN 写一个原始 IPv4 包，由本机内核协议栈接收。
func (d *device) Write(packet []byte) (int, error) { return d.file.Write(packet) }

// Name 返回内核确认的实际接口名（请求名被占用时内核会另行分配）。
func (d *device) Name() string { return d.name }

// Close 关闭 TUN fd；地址与路由的拆除由 RouteCleanup 负责。
func (d *device) Close() error {
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

// ifreq 对齐 TUNSETIFF ioctl 的接口请求结构。
type ifreq struct {
	name  [16]byte
	flags uint16
	_     [22]byte // 填充至结构体总长
}

// trimName 把 C 字符串截断到首个 NUL。
func trimName(raw []byte) string {
	for i, b := range raw {
		if b == 0 {
			return string(raw[:i])
		}
	}
	return string(raw)
}
