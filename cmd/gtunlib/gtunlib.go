// Package gtunlib 是 GTun-Lite 客户端内核的 gomobile 导出层：把打洞、
// 控制面与数据面整套内核以库形态交给移动端壳（Android VpnService / 未来
// iOS NetworkExtension）驱动。
//
// 与桌面端 cmd/client 的分工差异只有生命周期一处：桌面端是「进程即会话」，
// 退出靠信号；这里是「壳进程长存，会话由宿主启停」，因此 Start/Stop 显式
// 管理一个内部 context，不用信号。装配序列与桌面 main.run 严格同构：
// 配置 → 日志 → fd 预算 → 身份 → manager → 控制面。
//
// TUN 的获取走 androidfd 交付协议：内核在网络配置到达（拿到虚拟 IP）时经
// EventListener.TunRequest 同步回调宿主，宿主完成 establish 后直接返回 fd
// （失败抛异常）。控制面本身走互联网，不依赖 TUN，因此授权框只会出现在
// 「服务器已经认了这台设备」之后，不会白弹。
package gtunlib

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"

	"gtun-lite/internal/client"
	"gtun-lite/internal/logging"
	"gtun-lite/internal/notice"
	"gtun-lite/internal/tun/androidfd"
)

// EventListener 由宿主壳层实现，接收内核的关键事件。方法都在内核 goroutine
// 被调用，宿主自行切回 UI 线程后再碰 UI。
type EventListener interface {
	// Notice 转发窗口中文提示，一行一条（与桌面端 stderr 内容一致，已带时刻）。
	Notice(line string)
	// TunRequest 内核要建 TUN：宿主完成 establish 后返回 fd（对
	// ParcelFileDescriptor 先 detachFd 再返回裸 fd，所有权随返回移交内核）；
	// 失败返回非 nil 错误（Java 实现里抛异常，原因如实进入状态上报）。
	// peers 是逗号分隔的对端虚拟 IP。
	TunRequest(mtu int64, localIP string, peers string) (int64, error)
}

var (
	listener atomic.Value // EventListener
	mu       sync.Mutex
	cancel   context.CancelFunc
	// session 是会话代次，Start 递增。收尾清理以它做会话身份：
	// CancelFunc 不可比较，无法直接判「自己是否现任」。
	session uint64
	running bool
)

// SetListener 注册宿主事件回调。Start 前调用；传 nil 清除。
func SetListener(l EventListener) { listener.Store(l) }

// Start 异步启动一次客户端会话。配置错误等装配问题同步返回；运行中的状态
// 变化经 EventListener.Notice 推送。重复启动拒绝——先 Stop 再 Start。
func Start(configPath string) error {
	mu.Lock()
	if running {
		mu.Unlock()
		return fmt.Errorf("gtun session already running")
	}
	// 配置同步加载：路径错误、键缺失这类问题当场报给宿主，不藏进异步。
	if _, err := client.LoadClientConfig(configPath); err != nil {
		mu.Unlock()
		return err
	}
	running = true
	session++
	mySession := session
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	mu.Unlock()
	go run(ctx, configPath, mySession)
	return nil
}

// Stop 停止当前会话：取消 context，控制面断连、Worker 拆除、TUN 关闭。
// 幂等。
func Stop() error {
	mu.Lock()
	c := cancel
	running = false
	cancel = nil
	mu.Unlock()
	if c != nil {
		c()
	}
	return nil
}

// IsRunning 报告当前是否已有会话在跑。
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

// run 是会话主体，序列与 cmd/client 的 run 一致（差别仅：无信号、ctx 由
// Stop 控制、提示走 EventListener 而非 stderr）。mySession 是本会话代次，
// 用作会话身份：收尾清理仅在「自己仍是现任会话」时执行——Stop 后立即
// Start 的新会话不受旧 goroutine 迟到退出的影响。
func run(ctx context.Context, configPath string, mySession uint64) {
	defer func() {
		mu.Lock()
		if session == mySession {
			running = false
			cancel = nil
			mu.Unlock()
			notifyNotice("GTun 会话已结束")
			return
		}
		mu.Unlock()
		// 被快速重启顶替的旧会话安静退出：新会话正在运行，
		// 「会话已结束」只属于最后一任。
	}()

	config, err := client.LoadClientConfig(configPath)
	if err != nil {
		notifyNotice("启动失败（配置加载失败）：" + err.Error())
		return
	}
	log, closeLogs, err := logging.New(logging.Options{
		Level:     config.Logging.Level,
		File:      config.Logging.File,
		ErrorFile: config.Logging.ErrorFile,
	})
	if err != nil {
		notifyNotice("启动失败（日志初始化失败）：" + err.Error())
		return
	}
	defer closeLogs()

	window := notice.New(newNoticePipe())

	if err := client.EnsureHelperFDHeadroom(config.Punch.HelperCount); err != nil {
		log.Error("helper fd headroom", "error", err)
		window.Printf("启动失败（helper 文件描述符预算不足）：%v", err)
		return
	}
	identity, err := client.LoadIdentity(config.Identity.Path)
	if err != nil {
		log.Error("load identity", "error", err)
		window.Printf("启动失败（设备身份加载失败）：%v", err)
		return
	}

	manager := client.NewManager(config, identity, client.PlatformOpener(), stubRouteTable{}, log, window)
	defer manager.Close()
	// fd 交付回调注册：Open 经 androidfd.TunRequester 向宿主要 fd。
	androidfd.SetTunRequester(tunRequestAdapter{currentListener()})
	control := client.NewControlClient(config, manager, identity, log, window)

	log.Info("gtun-lite session started", "device_id", string(identity), "server", config.Server.Addr)
	window.Printf("客户端已启动（设备 %s）", string(identity))
	if err := control.Run(ctx); err != nil {
		select {
		case <-ctx.Done():
			window.Printf("客户端已停止")
		default:
			window.Printf("控制面异常退出：%v", err)
		}
	}
}

// stubRouteTable 满足 preflight 的只读查询。安卓路由由 VpnService 声明式
// 管理：随 VPN 接口生灭、无系统 /32 残留，保守返回「无网关、无本机地址、
// 无既有路由」，preflight 保留地址与服务器地址两项检查照常生效，其余
// 冲突类检查在安卓上不存在检查对象。
type stubRouteTable struct{}

func (stubRouteTable) DefaultGateway() (netip.Addr, bool, error)   { return netip.Addr{}, false, nil }
func (stubRouteTable) LocalAddresses() ([]netip.Addr, error)       { return nil, nil }
func (stubRouteTable) HasHostRoute(netip.Addr) (bool, error)       { return false, nil }
func (stubRouteTable) HostRouteDangling(netip.Addr) (bool, error) { return false, nil }
func (stubRouteTable) DeleteHostRoute(netip.Addr) error            { return nil }

// newNoticePipe 建一条管道把窗口提示转发给 EventListener.Notice。写侧交给
// notice.Notice，读侧逐行推给宿主；会话结束管道随写侧关闭而终结。
func newNoticePipe() io.Writer {
	reader, writer := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			notifyNotice(scanner.Text())
		}
	}()
	return writer
}

// notifyNotice 把一行提示推给宿主回调；无监听者时安静丢弃。
func notifyNotice(line string) {
	if l, ok := listener.Load().(EventListener); ok && l != nil {
		l.Notice(line)
	}
}

// currentListener 取当前宿主回调；未注册返回 nil。
func currentListener() EventListener {
	l, _ := listener.Load().(EventListener)
	return l
}

// tunRequestAdapter 把宿主 EventListener 适配成 androidfd.TunRequester。
type tunRequestAdapter struct{ l EventListener }

// RequestTun 同步转发内核的建 TUN 请求，返回宿主 establish 出的 fd。
func (adapter tunRequestAdapter) RequestTun(mtu int64, localIP string, peers string) (int64, error) {
	if adapter.l == nil {
		return 0, fmt.Errorf("host event listener not set")
	}
	return adapter.l.TunRequest(mtu, localIP, peers)
}
