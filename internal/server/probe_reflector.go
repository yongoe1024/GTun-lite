package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"gtun-lite/internal/common"
)

// probeSocketCount 是探测端点数量，与客户端的 5 端口画像一一对应。
const probeSocketCount = common.ProbePortCount

// 畸形报文日志限流参数：每源 IP 每分钟至多一条，限流表上限 4096 条。
// UDP 端口天生会被扫描器与背景噪声打到，逐包 Warn 是磁盘放大向量；
// 表满后丢弃新来源的日志——防御机制自身不得成为随时间无界增长的内存负担。
const (
	malformedLogWindow = time.Minute
	malformedLogCap    = 4096
)

// ProbeReflector 在连续 5 个 UDP 端口上执行无状态的 PROBE/PORT 交换：
// 收到合法 PROBE 就把「观察到的来源公网地址」回显给请求者。
//
// 无状态是刻意的：它不做聚合、不记是谁在探测——画像的聚合与分类
// 全在客户端（它把五次回显组装成 NATProfile 再经 TCP 上报）。
// 服务器只需要一面镜子，镜子不需要知道照镜子的是谁。
//
// 失败语义是 fail-fast：绑定失败或任一端口写出错都让 ListenAndServe
// 返回错误、由调用方终止进程。半套探测端点比没有更糟——客户端画像
// 永远缺样本，失败却被静默藏住。
type ProbeReflector struct {
	bind     string
	basePort int
	log      *slog.Logger

	mu      sync.Mutex
	sockets []*net.UDPConn
	stopped bool // Close 已到来：后绑完的端口自行销毁，见 closeSockets
	done    chan struct{}
	once    sync.Once

	malformedMu sync.Mutex
	malformed   map[string]time.Time // 源 IP → 最近一次记录时刻
}

// NewProbeReflector 创建反射器（未监听）。basePort 为 0 时由内核分配
// 连续端口的首端口（仅测试使用）。
func NewProbeReflector(bind string, basePort int, log *slog.Logger) (*ProbeReflector, error) {
	address := net.ParseIP(bind)
	if address == nil || address.To4() == nil {
		return nil, fmt.Errorf("probe bind address must be an IPv4 literal, got %q", bind)
	}
	if basePort < 0 || basePort > 65531 {
		return nil, fmt.Errorf("probe base port must be between 0 and 65531, got %d", basePort)
	}
	return &ProbeReflector{
		bind: bind, basePort: basePort, log: log,
		done: make(chan struct{}), malformed: make(map[string]time.Time),
	}, nil
}

// ListenAndServe 绑定 5 个端口并进入读循环，直到 ctx 取消。
// 任一端口绑定失败即整体失败；任一端口的回写失败同样整体失败——
// 见类型的 fail-fast 注释。done 在所有退出路径上都会关闭，
// Close 等待它不会挂死。
func (reflector *ProbeReflector) ListenAndServe(ctx context.Context) error {
	defer close(reflector.done)

	sockets := make([]*net.UDPConn, 0, probeSocketCount)
	closeAll := func() {
		for _, socket := range sockets {
			_ = socket.Close()
		}
	}
	basePort := reflector.basePort
	for index := 0; index < probeSocketCount; index++ {
		address := &net.UDPAddr{IP: net.ParseIP(reflector.bind).To4(), Port: basePort + index}
		socket, err := net.ListenUDP("udp4", address)
		if err != nil {
			closeAll()
			return fmt.Errorf("listen probe UDP port %d: %w", basePort+index, err)
		}
		if index == 0 && basePort == 0 {
			basePort = socket.LocalAddr().(*net.UDPAddr).Port
			if basePort > 65531 {
				closeAll()
				return fmt.Errorf("ephemeral probe port %d cannot fit the five-port range", basePort)
			}
		}
		sockets = append(sockets, socket)
	}
	// Close 若抢在绑定完成之前到来（stopped 已置位），这里自行销毁并退出：
	// Close 的 once 只会关一次 socket，赶不上后到的这批。
	reflector.mu.Lock()
	if reflector.stopped {
		reflector.mu.Unlock()
		closeAll()
		return nil
	}
	reflector.sockets = sockets
	reflector.mu.Unlock()
	// 停机关闭走 ctx；本函数返回（含 fail-fast）经 watchDone 结束监视，
	// 两条路径都收敛，监视 goroutine 不依赖进程退出来终结。fail-fast 的
	// 资源清理由上方 closeSockets 完成，不再 defer Close——Close 会等待
	// 退出信号，而退出以本函数返回为前提，defer 它构成循环等待。
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			reflector.Close()
		case <-watchDone:
		}
	}()

	serveErr := make(chan error, probeSocketCount)
	var wg sync.WaitGroup
	for _, socket := range sockets {
		wg.Add(1)
		go func(socket *net.UDPConn) {
			defer wg.Done()
			if err := reflector.serve(socket); err != nil {
				serveErr <- err
				// fail-fast：关掉其余端口的 socket 解除它们的读阻塞，
				// 让整个反射器一起退出，而不是带着残缺探测面继续跑。
				reflector.closeSockets()
			}
		}(socket)
	}
	wg.Wait()
	select {
	case err := <-serveErr:
		return fmt.Errorf("probe reflector port failed: %w", err)
	default:
		return nil
	}
}

// Addr 返回首个探测端点的监听地址（测试用 0 端口时与配置不同）。
func (reflector *ProbeReflector) Addr() *net.UDPAddr {
	reflector.mu.Lock()
	defer reflector.mu.Unlock()
	if len(reflector.sockets) != probeSocketCount {
		return nil
	}
	return reflector.sockets[0].LocalAddr().(*net.UDPAddr)
}

// Close 关闭全部端口并等待读循环退出。幂等。
func (reflector *ProbeReflector) Close() {
	reflector.once.Do(func() {
		reflector.closeSockets()
		<-reflector.done
	})
}

// closeSockets 关闭当前持有的全部端口（不等待 done）。
// 标记 stopped：与 ListenAndServe 的绑定段互斥，Close 抢先时后绑的
// 端口能察觉并自行销毁。
func (reflector *ProbeReflector) closeSockets() {
	reflector.mu.Lock()
	reflector.stopped = true
	sockets := reflector.sockets
	reflector.sockets = nil
	reflector.mu.Unlock()
	for _, socket := range sockets {
		_ = socket.Close()
	}
}

// serve 是单端点的读循环：解析、回显；畸形报文限流记日志后丢弃。
// 读错误（端口被关闭）是正常停机路径返回 nil；回写失败返回错误，
// 由 ListenAndServe 汇总为整体失败。
func (reflector *ProbeReflector) serve(socket *net.UDPConn) error {
	reflector.log.Debug("probe port serving", "local", socket.LocalAddr().String())
	buffer := make([]byte, common.MaxProbeDatagram+1)
	for {
		length, remote, err := socket.ReadFromUDP(buffer)
		if err != nil {
			return nil // 端口关闭（停机）
		}
		request, err := common.ParseProbeRequest(buffer[:length])
		if err != nil {
			if reflector.shouldLogMalformed(remote.IP.String()) {
				reflector.log.Warn("malformed probe datagram", "source", remote.String(), "error", err.Error())
			}
			continue
		}
		publicIP := remote.IP.To4()
		if publicIP == nil || remote.Port <= 0 {
			continue
		}
		response, err := common.EncodeProbeResponse(common.ProbeResponse{
			Nonce: request.Nonce, ProbeID: request.ProbeID,
			PublicIP: common.IPv4(publicIP.String()), MappedPort: common.Port(remote.Port),
		})
		if err != nil {
			continue
		}
		if _, err := socket.WriteToUDP(response, remote); err != nil {
			return fmt.Errorf("echo to %s: %w", remote, err)
		}
	}
}

// shouldLogMalformed 按源 IP 限流：窗口内已记录过的来源不再记，
// 表满后新来源直接放弃日志。返回 true 表示这条值得记。
func (reflector *ProbeReflector) shouldLogMalformed(source string) bool {
	now := time.Now()
	reflector.malformedMu.Lock()
	defer reflector.malformedMu.Unlock()
	if last, ok := reflector.malformed[source]; ok && now.Sub(last) < malformedLogWindow {
		return false
	}
	if len(reflector.malformed) >= malformedLogCap {
		if _, known := reflector.malformed[source]; !known {
			return false
		}
	}
	reflector.malformed[source] = now
	return true
}
