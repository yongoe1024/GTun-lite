package tun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gtun-lite/internal/common"
)

// WorkerLink 是数据面访问已连接 Worker 的最小接口。Manager 注册 Worker 时提供此句柄，
// 数据面据此发送出站帧并接收入站帧；平台/数据面逻辑不触碰 Worker 的握手状态。
type WorkerLink interface {
	// AttemptToken 返回当前链路尝试的 token，数据面用它编码 GTUN 帧头的 6 字节。
	// 每次发帧都重新取：Worker 重新打洞后 token 会变，缓存会让帧带上旧 token
	// 而被对端丢弃。这是接口只暴露 token 而非整个身份的原因——数据面除了
	// 帧头这 6 字节，不需要知道链路的任何其他身份信息。
	AttemptToken() common.LinkToken
	// PeerLive 返回握手选定的对端地址，用于发送出站帧。
	PeerLive() (*netip.AddrPort, bool)
	// SendFrame 编码并经由 p2p_socket 发送一帧到 peer_live。
	SendFrame(ctx context.Context, frame []byte) error
	// SendBatch 批量发送一批出站帧；平台差异（Linux 批量化、其余逐包
	// 等价）由实现负责。失败语义与 SendFrame 一致：失败即丢，由上层
	// 协议（内层 TCP 重传）承担。
	SendBatch(ctx context.Context, frames [][]byte) error
}

// DataPlaneConfig 是数据面的不可变配置。
type DataPlaneConfig struct {
	// TUNMTU 是内层 IPv4 包的大小上限，帧编解码与缓冲分配都以此为准。
	TUNMTU int
	// OutboundQueuePackets 是每条链路出站队列（TUN → 对端）的包数上限。
	OutboundQueuePackets int
	// InboundQueuePackets 是全局入站队列（对端 → TUN）的包数上限。
	InboundQueuePackets int
	// Logger 接收数据面的调试日志（循环启停、丢包、退出原因）。
	// 丢弃与退出在 Info 级可见，逐包细节在 Debug 级；nil 表示全部丢弃。
	Logger *slog.Logger
}

// DataPlane 驱动 TUN 读写与 Worker 出入站队列。TUN 读循环按 dst 查找配对 Worker；
// 入站由 Worker 接收循环投递帧，数据面校验后写入全局入站队列。
//
// 四条 goroutine：TUN 读循环（平台相关，见 Start）、每链路一个出站发送者、
// 一个全局 TUN 写循环。队列满一律丢新包——不阻塞投递方，也不挤掉已排队
// 的旧包；丢弃与退出都写日志，静默死循环是数据面最难排查的故障形态。
type DataPlane struct {
	// device 是平台 TUN 设备（darwin utun / linux tun / windows wintun）。
	device Device
	// config 是启动时固化的不可变配置。
	config DataPlaneConfig
	// localIP 是本机虚拟 IP，出站包的源地址校验基准。
	localIP netip.Addr

	// mu 保护 links 表（注册/注销与 TUN 读循环的查找并发）。
	mu     sync.Mutex
	links  map[netip.Addr]*workerQueue // 按 peer 虚拟 IP 索引的出/入站队列
	closed bool

	// log 接收丢包与循环退出日志。数据面曾经完全无日志：写循环首次出错
	// 静默退出后隧道单向失灵、表面毫无异常——真机排障时定位代价极高，
	// 因此所有退出路径与丢弃决策都必须在此留痕。
	log *slog.Logger
	// 逐包事件的节流时间戳（unix 秒）：队列满/发送失败这类事件在拥塞期
	// 每个丢包触发一次，不限流日志自身就成了新的故障放大器。
	lastInboundFullLog  int64
	lastOutboundFullLog int64
	lastSendFailLog     int64
	writeCh             chan []byte // 全局共享的 TUN 写入队列（入站帧经校验后投递）
	ctx                 context.Context
	cancel              context.CancelFunc
	wg                  sync.WaitGroup
}

// throttled 判定一个逐包日志位是否放行：同一位置每秒至多一条。
// CAS 保证并发投递路径（多链路接收循环）下不重复放行。
func throttled(last *int64) bool {
	now := time.Now().Unix()
	for {
		previous := atomic.LoadInt64(last)
		if now-previous < 1 {
			return false
		}
		if atomic.CompareAndSwapInt64(last, previous, now) {
			return true
		}
	}
}

// workerQueue 是一条已建成链路在数据面里的全部状态。
type workerQueue struct {
	// link 是 Worker 提供的收发句柄。
	link WorkerLink
	// peerIP 是对端虚拟 IP，日志与索引用。
	peerIP netip.Addr
	// outbound 是 TUN → 对端的包队列；满则丢新包，关闭即发送者退出信号。
	outbound chan []byte
}

var (
	errDataPlaneClosed = errors.New("data plane is closed")
)

// NewDataPlane 创建数据面。TUN 读写循环由 Start 启动；Apply 配置时注册 Worker 队列。
func NewDataPlane(device Device, config DataPlaneConfig, localIP common.IPv4) (*DataPlane, error) {
	addr, err := netip.ParseAddr(string(localIP))
	if err != nil || !addr.Is4() {
		return nil, errors.New("invalid local IP for data plane")
	}
	if device == nil || config.TUNMTU < 20 || config.OutboundQueuePackets <= 0 || config.InboundQueuePackets <= 0 {
		return nil, errors.New("invalid data plane configuration")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &DataPlane{device: device, config: config, localIP: addr, log: config.Logger,
		links: make(map[netip.Addr]*workerQueue), writeCh: make(chan []byte, config.InboundQueuePackets),
		ctx: ctx, cancel: cancel}, nil
}

// Start 启动 TUN 读循环与 TUN writer。返回后数据面开始处理包。
// 读循环纳入 wg：平台 fd 均为非阻塞 + poller 接管（见各平台 Open），
// Close 时被 device.Close 唤醒退出，可被等待。
func (dp *DataPlane) Start() {
	dp.wg.Add(1)
	go func() { defer dp.wg.Done(); dp.tunReadLoop() }()
	dp.wg.Add(1)
	go dp.tunWriteLoop()
}

// RegisterLink 注册一个已连接 Worker 的出站队列，按 peer 虚拟 IP 索引。
func (dp *DataPlane) RegisterLink(link WorkerLink, peerIP common.IPv4) error {
	addr, err := netip.ParseAddr(string(peerIP))
	if err != nil || !addr.Is4() {
		return errors.New("invalid peer IP")
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if dp.closed {
		return errDataPlaneClosed
	}
	if _, exists := dp.links[addr]; exists {
		return errors.New("peer link already registered")
	}
	queue := &workerQueue{link: link, peerIP: addr, outbound: make(chan []byte, dp.config.OutboundQueuePackets)}
	dp.links[addr] = queue
	// 启动该 Worker 的出站发送 goroutine。
	dp.wg.Add(1)
	go dp.outboundSender(queue)
	dp.log.Debug("outbound queue registered", "peer", addr.String(), "queue_packets", dp.config.OutboundQueuePackets)
	return nil
}

// UnregisterLink 移除一个 Worker 的队列并关闭其 outbound 通道：outboundSender 以
// 通道关闭为退出信号。关闭与 tunReadLoop 的非阻塞发送都在 dp.mu 内串行执行，
// 不会出现「向已关闭通道发送」的竞争。
func (dp *DataPlane) UnregisterLink(peerIP common.IPv4) {
	addr, err := netip.ParseAddr(string(peerIP))
	if err != nil {
		return
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if queue, exists := dp.links[addr]; exists {
		delete(dp.links, addr)
		close(queue.outbound)
	}
}

// DeliverInbound 把一个已校验的入站 GTUN payload（原始 IPv4 包）投递到全局 TUN 写入队列。
// 队列满（即 inbound_queue_packets 个全局 permit 耗尽）时丢弃新包，不阻塞投递方。
func (dp *DataPlane) DeliverInbound(packet []byte) {
	select {
	case dp.writeCh <- packet:
	default:
		// 全局入站 permit 满，丢弃。丢弃必须可见：持续丢弃意味着 TUN 写入
		// 已停摆（写循环退出）或对端突发超过消费能力，两者处置完全不同。
		// 与出站方向同样按秒节流：这是逐包路径，不限流日志自身就成了
		// 拥塞期新的故障放大器。
		if throttled(&dp.lastInboundFullLog) {
			dp.log.Warn("inbound queue full; packet dropped", "queue_packets", dp.config.InboundQueuePackets)
		}
	}
}

// tunReadLoop 从 TUN 读取原始 IPv4 包，校验源地址，按目的地址查找配对 Worker 入队。
// 查找与非阻塞发送在 dp.mu 内完成，保证与 UnregisterLink 的通道关闭互斥。
func (dp *DataPlane) tunReadLoop() {
	dp.log.Debug("tun read loop started", "interface", dp.device.Name(), "mtu", dp.config.TUNMTU)
	buffer := make([]byte, dp.config.TUNMTU+4)
	for {
		n, err := dp.device.Read(buffer)
		if err != nil {
			// 读循环退出即出站方向死亡（TUN 关闭是正常停机，其余为异常）。
			dp.log.Debug("tun read loop exited", "interface", dp.device.Name(), "error", err.Error())
			return
		}
		if n < 20 {
			// 空读取：避免 busy-spin。真实 TUN 设备的 Read 会阻塞直到有包。
			select {
			case <-dp.ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		packet := make([]byte, n)
		copy(packet, buffer[:n])
		if err := common.ValidateIPv4Packet(packet); err != nil {
			dp.log.Debug("outbound packet failed IPv4 validation; dropped", "error", err.Error(), "size", n)
			continue
		}
		src, dst, ok := ipv4Endpoints(packet)
		if !ok || src != dp.localIP {
			// 源必须等于本机虚拟 IP：TUN 上出现其他源说明有进程试图经
			// gtun 接口伪造源地址（或路由误把别人的流量引了进来）。
			dp.log.Debug("outbound packet with foreign source; dropped", "src", src.String(), "want", dp.localIP.String())
			continue
		}
		dp.mu.Lock()
		queue := dp.links[dst]
		if queue != nil {
			select {
			case queue.outbound <- packet:
			default:
				// 出站队列满，丢弃。偶发丢弃由 TCP 重传吸收；持续丢弃
				// 说明对端链路消费不及，属于容量问题而非错误。
				if throttled(&dp.lastOutboundFullLog) {
					dp.log.Warn("outbound queue full; packets being dropped", "peer", dst.String())
				}
			}
		} else {
			dp.log.Debug("outbound packet to unregistered peer; dropped", "dst", dst.String())
		}
		dp.mu.Unlock()
	}
}

// outboundBatchSize 是出站攒批上限：队列非空时一次取满这批帧再发送，
// Linux 上对应一次 sendmmsg 系统调用。只在包已在队列里时攒批，绝不
// 为凑批等待——不引入任何额外延迟。
const outboundBatchSize = 32

// outboundSender 从 Worker 出站队列取包，编码 GTUN 帧并批量发送到 peer_live。
// 攒批：收到一个包后非阻塞 drain 队列（至多 outboundBatchSize 帧），一次
// SendBatch——Linux 上即一次 sendmmsg，减少高 pps 下的系统调用与内核切换。
// 以 outbound 通道关闭或数据面 ctx 取消为退出信号。
func (dp *DataPlane) outboundSender(queue *workerQueue) {
	defer dp.wg.Done()
	var sequence uint32
	batch := make([][]byte, 0, outboundBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := queue.link.SendBatch(dp.ctx, batch); err != nil {
			// 发送失败会在 Worker 停止前对每个包重复，节流到每秒一条；
			// 持续失败由保活超时（≤60s）收敛，日志只需指明方向。
			if throttled(&dp.lastSendFailLog) {
				dp.log.Warn("send batch failed; packets being dropped", "peer", queue.peerIP.String(), "error", err.Error())
			}
		}
		batch = batch[:0]
	}
	encode := func(packet []byte) {
		frame, err := common.EncodeGTUNFrame(queue.link.AttemptToken(), sequence, packet, dp.config.TUNMTU)
		if err != nil {
			// 编码拒绝的典型原因是包大于 TUNMTU（TUN 读循环的缓冲决定
			// 了不该出现这种包）；静默跳过曾让"对端收不到大包"无从排查。
			dp.log.Warn("GTUN frame encoding rejected; packet dropped", "peer", queue.peerIP.String(), "size", len(packet), "mtu", dp.config.TUNMTU, "error", err.Error())
			return
		}
		sequence++
		batch = append(batch, frame)
	}
	for {
		select {
		case packet, ok := <-queue.outbound:
			if !ok {
				flush()
				dp.log.Debug("outbound sender exited; queue closed", "peer", queue.peerIP.String())
				return
			}
			encode(packet)
			// 队列非空时继续 drain，攒满一批再发；空队列立即 flush（不等待）。
		drain:
			for len(batch) < outboundBatchSize {
				select {
				case more, ok := <-queue.outbound:
					if !ok {
						flush()
						dp.log.Debug("outbound sender exited; queue closed", "peer", queue.peerIP.String())
						return
					}
					encode(more)
				default:
					break drain
				}
			}
			flush()
		case <-dp.ctx.Done():
			flush()
			dp.log.Debug("outbound sender exited; data plane closing", "peer", queue.peerIP.String())
			return
		}
	}
}

// tunWriteLoop 从全局入站队列取包写入 TUN，队列满时由 DeliverInbound 丢新包。
// 写失败即退出：TUN 写错误不可恢复（设备通常已被拆除），继续重试只会刷日志。
func (dp *DataPlane) tunWriteLoop() {
	defer dp.wg.Done()
	for {
		select {
		case packet, ok := <-dp.writeCh:
			if !ok {
				return
			}
			if _, err := dp.device.Write(packet); err != nil {
				// 写循环死亡 = 全部链路的入站方向停摆。这条日志是单向失灵
				// 故障的唯一线索，必须高于 Debug 级。
				dp.log.Error("tun write failed; inbound path stopping", "interface", dp.device.Name(), "error", err.Error())
				return
			}
		case <-dp.ctx.Done():
			return
		}
	}
}

// Close 停止数据面并关闭 TUN 设备。幂等。
func (dp *DataPlane) Close() error {
	dp.mu.Lock()
	if dp.closed {
		dp.mu.Unlock()
		return nil
	}
	dp.closed = true
	dp.links = make(map[netip.Addr]*workerQueue)
	dp.mu.Unlock()
	// 先 cancel 停 outboundSender；随后必须先关闭设备再 wg.Wait：tunReadLoop
	// 阻塞在 device.Read 时无法感知 ctx 取消，只有 device.Close（经 runtime
	// poller）能让 Read 立即返回错误退出，否则 TUN 空闲时 Close 会永久卡在
	// wg.Wait。
	dp.cancel()
	if dp.device != nil {
		_ = dp.device.Close()
	}
	dp.wg.Wait()
	return nil
}

// ipv4Endpoints 提取 IPv4 包的源和目的地址。
func ipv4Endpoints(packet []byte) (src, dst netip.Addr, ok bool) {
	if len(packet) < 20 {
		return netip.Addr{}, netip.Addr{}, false
	}
	src = netip.AddrFrom4([4]byte{packet[12], packet[13], packet[14], packet[15]})
	dst = netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	return src, dst, true
}
