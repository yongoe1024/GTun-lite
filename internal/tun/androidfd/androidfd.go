// Package androidfd 把 Android VpnService.establish() 返回的 TUN 文件描述符
// 适配成 tun.Device 与 tun.Opener。
//
// 与 mac/linux/win 三个平台实现的本质差异：这个平台的 TUN 不是本包自己打开
// 的。establish() 必须由宿主壳层在拿到虚拟 IP 之后调用——VPN 授权框也只能由
// 宿主弹出——所以 Open 的时序变成「内核要 fd → 回调宿主 → 宿主授权并
// establish → ProvideTunFd 送回」。fd 是一次性资源：os.File 关闭即 fd 失效，
// 每次栈重建都必须重新走一遍交付流程，不做任何缓存。
//
// 路由与地址由 VpnService.Builder 声明式下发（随 VPN 接口生灭，无系统残留），
// 本包不做任何系统路由操作；RouteCleanup 因此只负责关闭设备。
package androidfd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// TunRequester 是宿主壳层实现的 TUN 交付回调（gomobile 导出接口，Android 端
// 由 VpnService 实现）。
type TunRequester interface {
	// RequestTun 请求宿主建立 VPN 接口：mtu 与 localIP 原样交给
	// VpnService.Builder，peers 是逗号分隔的对端虚拟 IP 列表（逐个 /32 路由）。
	// 宿主完成授权与 establish 后必须调用 ProvideTunFd 送回 fd；拒绝授权则
	// 不送回（内核侧等待会超时失败，链路按 TUN_CREATE_FAILED 上报）。
	RequestTun(mtu int64, localIP string, peers string)
}

const (
	// requestTimeout 是单次 fd 交付的等待上限：首 授权框需要人工确认，
	// 取宽松值；授权被拒不会送 fd，只能靠超时把失败如实上报。
	requestTimeout = 10 * time.Minute
)

var (
	mu        sync.Mutex
	requester TunRequester
	// pending 是宿主先于 Open 送到的 fd 暂存位（Open 消费后即清）。
	pending = -1
	// delivered 是「pending 有新值」的信号；缓冲 1，重复送达不阻塞。
	delivered = make(chan struct{}, 1)
	// abandoned 是当前会话的放弃信号。每个会话开始时在 SetTunRequester
	// 里换新——若只 close 一次，Stop 之后的下一个会话里这个已关闭的
	// channel 会让所有 fd 等待立即误判为「已放弃」，断开再连必挂。
	abandoned = make(chan struct{})
)

// SetTunRequester 注册宿主回调并开启新一轮交付会话（gtunlib 每次会话
// 启动时调用）。重置放弃信号，使上一轮 Stop 的 Abandon 不再影响新会话；
// 顺手排干上一会话遗留的未消费 fd（它随旧 VPN 接口一起失效，留着只泄漏）。
func SetTunRequester(r TunRequester) {
	mu.Lock()
	defer mu.Unlock()
	requester = r
	if pending >= 0 {
		_ = os.NewFile(uintptr(pending), "gtun-stale-tun").Close()
		pending = -1
	}
	abandoned = make(chan struct{})
}

// ProvideTunFd 由宿主在 establish() 成功后送回 fd。fd 所有权随之移交本包
// （宿主侧应对 ParcelFileDescriptor 用 detachFd，避免双重关闭）。
// fd 暂存到 pending 位等 Open 消费；暂存位已占用（上一个 fd 未被消费，
// 通常是宿主重复 establish）时拒绝并让宿主自行关闭新 fd。
func ProvideTunFd(fd int64) error {
	if fd <= 0 || fd > 1<<31-1 {
		return fmt.Errorf("invalid tun fd %d", fd)
	}
	mu.Lock()
	hasRequester := requester != nil
	taken := false
	if hasRequester && pending < 0 {
		pending = int(fd)
		taken = true
	}
	mu.Unlock()
	if !hasRequester {
		return errors.New("tun requester not set")
	}
	if !taken {
		return errors.New("previous tun fd not consumed; fd not taken")
	}
	select {
	case delivered <- struct{}{}:
	default:
	}
	return nil
}

// Abandon 让当前会话所有等待 fd 的 Open 立即失败退出（宿主停止整个内核
// 时调用）。信号随下一个会话的 SetTunRequester 重置。
func Abandon() {
	mu.Lock()
	defer mu.Unlock()
	select {
	case <-abandoned:
	default:
		close(abandoned)
	}
}

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

// Opener 实现 tun.Opener：消费已交付的 fd，或向宿主回调要一个新 fd。
type Opener struct{}

// Open 获取 fd 并包装为 Device。优先消费 ProvideTunFd 已送到的 fd（宿主在
// 授权框确认后 fd 先到、Open 后到的时序），否则现场向宿主要求交付并阻塞等待。
func (Opener) Open(ctx context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	mu.Lock()
	r := requester
	fd := pending
	if fd >= 0 {
		pending = -1
	}
	abandonedCh := abandoned // 快照：等待期间会话可能被重置，别读同一个变量
	mu.Unlock()
	if fd < 0 {
		if r == nil {
			return nil, tun.RouteCleanup{}, errors.New("android tun requester not set")
		}
		r.RequestTun(int64(mtu), string(localIP), joinIPs(peers))
	wait:
		select {
		case <-delivered:
			mu.Lock()
			fd = pending
			pending = -1
			mu.Unlock()
			if fd < 0 {
				goto wait // 信号被前一轮消费完，继续等
			}
		case <-abandonedCh:
			return nil, tun.RouteCleanup{}, errors.New("tun fd wait abandoned")
		case <-time.After(requestTimeout):
			return nil, tun.RouteCleanup{}, errors.New("tun fd wait timed out (host never provided fd)")
		case <-ctx.Done():
			return nil, tun.RouteCleanup{}, fmt.Errorf("tun fd wait canceled: %w", ctx.Err())
		}
	}
	device := NewDevice(fd, name)
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
