package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"

	"gtun-lite/internal/common"
	"gtun-lite/internal/notice"
)

// 打洞与保活的固定参数。不进配置文件：它们是协议行为的一部分，
// 两端取值不同不会不兼容，但会让打洞成功率分析失去共同基准。
const (
	// punchDirectInterval 是 stable-stable 直连 PUNCH 的发送间隔。
	punchDirectInterval = 100 * time.Millisecond
	// punchPollInterval 是三级扫描与 helper 轮询的全局最小发送间隔。
	punchPollInterval = 3 * time.Millisecond
	// punchOKResendInterval / punchOKResendCount 是 PUNCH_OK 的可靠重发：
	// 首次立即发送，随后按间隔再发 3 次（协议要求，link1 侧靠它完成握手）。
	punchOKResendInterval = 100 * time.Millisecond
	punchOKResendCount    = 3
	// helperInitWindow 是 stable 侧等 variable 侧 helper 建立映射的窗口，
	// 之后才开始 Range 扫描——扫得太早只会打在还没建立的映射上。
	helperInitWindow = 2 * time.Second
	// keepaliveInterval / keepaliveTimeout 是建成隧道的 PING 保活与失活阈值。
	// 失活即上报 TUNNEL_LOST，任一侧上报即拆链（服务器不变量 3）。
	keepaliveInterval = 20 * time.Second
	keepaliveTimeout  = 60 * time.Second
	// 端口空间与三级扫描的窗口常量在 scanplan.go（候选流纯逻辑所在处）。
)

// WorkerEvent 是 Worker 向控制会话汇报的里程碑。控制循环把它翻译成
// TCP 上报（画像消息或转场 state_report）并维护管理器视图；
// 事件丢失只影响「上报的及时性」，不影响 Worker 自身状态——
// 控制面断开期间 Worker 照常运行，重连后的全量上报自会补齐。
type WorkerEvent struct {
	PeeringID common.PeeringID
	Token     common.LinkToken
	Kind      WorkerEventKind
	Reason    common.Reason      // 仅失败/失活事件携带
	Profile   *common.NATProfile // 仅画像事件携带
}

// WorkerEventKind 是 Worker 里程碑的种类。
type WorkerEventKind int

const (
	// WorkerProfile：画像完成，待上报（服务器据此配对下发对端画像）。
	WorkerProfile WorkerEventKind = iota
	// WorkerConnected：三步握手完成，隧道建成。
	WorkerConnected
	// WorkerFailed：尝试失败（探测/打洞/NAT 组合），尝试终止。
	WorkerFailed
	// WorkerLost：已建成的隧道保活失活。
	WorkerLost
)

// linkWorker 承载一次建链尝试的全部阶段：探测 → 画像上报 → 等对端画像
// → 打洞 → 三步握手 → 保活。一条链路一个 Worker，随 CONNECT 创建，
// 随 DISCONNECT、配置收缩、失败或失活终止。
//
// 并发模型：run 是唯一的状态裁决者（owner），接收 goroutine 只投递事件，
// 发送 goroutine 只发包；p2p 路径字段由 dataMu 保护（保活与后续数据面并发读）。
type linkWorker struct {
	token    common.LinkToken
	peering  common.PeeringID
	peer     common.ConnectPeer
	device   common.DeviceID
	config   ClientConfig
	log      *slog.Logger
	window   *notice.Notice
	events   chan<- WorkerEvent
	incoming chan common.NATProfile // 对端画像投递（容量 1）
	ctx      context.Context
	cancel   context.CancelFunc
	// localVirtualIP 是本机虚拟 IP（服务器下发），peerVirtualIP 是对端的；
	// 二者用于入站 GTUN 帧的内层地址校验——隧道只承载这两地址间的流量，
	// 其余一律丢弃（防串流与地址欺骗）。
	localVirtualIP common.IPv4
	peerVirtualIP  common.IPv4
	// deliverInbound 把校验通过的入站 IPv4 包投递给数据面的全局入站队列。
	// 由 manager 在创建 Worker 时注入；数据面未就绪时为 nil，帧被丢弃。
	deliverInbound func(packet []byte)

	connected atomic.Bool
	finished  atomic.Bool

	mainSocket   *net.UDPConn
	mainSocketID common.SocketID
	// localAddr 是主 socket 绑定的本机源地址（selectLocalIPv4 选出），
	// helper 与主 socket 绑定同一地址：画像与打洞若走不同出口，
	// NAT 映射就对不上，打洞必败且难排查。仅 owner 读写。
	localAddr net.IP

	helperMu sync.Mutex
	helpers  []helperSocket

	eventsIn chan wireEvent
	// 握手状态（仅 owner 读写）。
	punchSentMu sync.Mutex
	punchSent   map[punchSentKey]punchSentRecord
	reverseSent map[reverseKey]struct{}
	ackAssoc    map[ackKey]ackAssociation
	connectedCh chan struct{}

	// p2p 路径与保活（dataMu 保护，保活/数据面并发读）。
	dataMu         sync.Mutex
	p2pSocket      *net.UDPConn
	p2pSocketID    common.SocketID
	peerSocketID   common.SocketID
	peerLive       *net.UDPAddr
	lastActivity   time.Time
	lastSupplement int64 // 选定路径 OK 补发的限流时刻（unix nano，atomic）

	wg sync.WaitGroup
}

// punchSentKey 标识「本地 socket 曾向某地址发过 PUNCH」，ACK 校验用。
type punchSentKey struct {
	localSocket common.SocketID
	ip          [4]byte
	port        int
}

// punchSentRecord 是 punchSent 的值。stage 非零表示该发是三级扫描的候选
// （命中溯源：入站报文校验通过时可回答「哪一级、第几次猜中的」）；
// 零值出现在信标、直连与反向 PUNCH——它们不是预测，命中也不算。
// global 是跨阶段的全局发送序号，仅观察日志用。
type punchSentRecord struct {
	stage   scanStage
	ordinal int
	global  int
	sentAt  time.Time
}

// reverseKey 标识一个 (来源, 发送者socket, 接收socket) 三元组，link0 侧
// 反向 PUNCH 的去重键：同三元组重复 PUNCH 只重发 ACK，不重发反向 PUNCH。
type reverseKey struct {
	sourcePort int
	sender     common.SocketID
	receiving  common.SocketID
	ip         [4]byte
}

// ackKey/ackAssociation 是 link1 侧收到 ACK 的记录，OK 到达时校验一致性。
type ackKey struct {
	localSocket common.SocketID
	ip          [4]byte
	port        int
}

type ackAssociation struct {
	peerSocketID common.SocketID
}

// helperSocket 把一个 helper socket 与它的 SocketID 绑定在同一份插入序里。
//
// PUNCH 的 SenderSocketID 必须属于真正发包的那个 socket：对端按它回
// ACK/PUNCH_OK，本侧接收循环再用「报文实际到达的 socket 的 ID」校验
// TargetSocketID。两者一旦错位，本侧自己的校验就会拒掉回程握手——曾经
// ids 取自 map 迭代序、sockets 取自切片序，zip 后几乎全部错位，一轮
// 轮询里只剩随机对齐的个别 helper 能建成，helper 档位的端口覆盖率设计
// 形同虚设。单一有序切片从结构上排除这种错位。
type helperSocket struct {
	socket *net.UDPConn
	id     common.SocketID
}

// wireEvent 是接收 goroutine 投递给 owner 的一条握手报文。
type wireEvent struct {
	kind     string // common.P2PType* 常量
	control  common.P2PControl
	source   *net.UDPAddr
	socket   *net.UDPConn
	socketID common.SocketID
}

// startLinkWorker 创建并启动一次尝试的 Worker。localIP 是本机虚拟 IP
// （入站校验用），deliverInbound 是数据面入站投递回调（可为 nil）。
func startLinkWorker(config ClientConfig, device common.DeviceID, token common.LinkToken, peering common.PeeringID, peer common.ConnectPeer, localIP common.IPv4, deliverInbound func([]byte), events chan<- WorkerEvent, log *slog.Logger, window *notice.Notice) *linkWorker {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &linkWorker{
		token: token, peering: peering, peer: peer, device: device,
		config: config, log: log.With("peering_id", string(peering), "token", string(token)),
		window: window, events: events, incoming: make(chan common.NATProfile, 1),
		ctx: ctx, cancel: cancel,
		localVirtualIP: localIP, peerVirtualIP: peer.IP, deliverInbound: deliverInbound,
		punchSent: make(map[punchSentKey]punchSentRecord), reverseSent: make(map[reverseKey]struct{}),
		ackAssoc: make(map[ackKey]ackAssociation), connectedCh: make(chan struct{}),
		eventsIn: make(chan wireEvent, 64),
	}
	go worker.run()
	return worker
}

// Stop 取消 Worker。幂等；尝试的清理与终结汇报由 run 自己完成。
func (worker *linkWorker) Stop() { worker.cancel() }

// deliverPeerProfile 投递服务器下发的对端画像（容量 1，非阻塞覆盖）。
func (worker *linkWorker) deliverPeerProfile(profile common.NATProfile) {
	select {
	case worker.incoming <- profile:
	default:
		select {
		case <-worker.incoming:
		default:
		}
		worker.incoming <- profile
	}
}

// currentState 返回当前事实状态（全量上报用）：finished/failed 即 IDLE。
func (worker *linkWorker) currentState() (string, common.LinkToken) {
	if worker.connected.Load() && !worker.finished.Load() {
		return common.StateConnected, worker.token
	}
	if !worker.finished.Load() {
		return common.StateConnecting, worker.token
	}
	return common.StateIdle, ""
}

// run 是 Worker owner：串联探测、画像交换与打洞，保活直到终态。
func (worker *linkWorker) run() {
	defer worker.finish()
	// 地址解析与源地址选择先于 socket 创建：主 socket 必须绑定在
	// 「通往服务器的本机出口」上，后续打洞复用同一 socket 才不会换出口。
	// 解析失败归 PROBE_TIMEOUT：CONFIG_INVALID 按定义只在启动校验阶段
	// 报出，运行期失败多是 DNS 抖动之类的环境问题。
	serverIP, err := serverIPv4(worker.config.Server.Addr)
	if err != nil {
		worker.fail(common.ReasonProbeTimeout)
		return
	}
	localIP, err := selectLocalIPv4(&net.UDPAddr{IP: serverIP, Port: worker.config.Server.ProbeBasePort})
	if err != nil {
		worker.fail(common.ReasonInternalError)
		return
	}
	worker.localAddr = localIP
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
	if err != nil {
		// 普通 UDP socket 创建失败不是 TUN 问题也不是配置问题，如实归入
		// 内部错误；错误原文在日志里。
		worker.fail(common.ReasonInternalError)
		return
	}
	// mainSocket 的赋值与关闭（finish）跨 goroutine，统一走 dataMu；
	// owner 后续使用各自持有的局部引用，不再触碰字段。
	worker.dataMu.Lock()
	worker.mainSocket = socket
	worker.dataMu.Unlock()
	worker.mainSocketID = common.GenerateSocketID()

	// 阶段一：五端口画像。
	profile, reason := worker.probe(serverIP)
	if reason != "" {
		worker.fail(reason)
		return
	}
	worker.emit(WorkerEvent{PeeringID: worker.peering, Token: worker.token, Kind: WorkerProfile, Profile: &profile})
	worker.window.Printf("探测完成：公网 IP %s，端口 %v，本机 NAT %s",
		string(profile.PublicIP), profile.Ports, notice.NAT(profile.NAT))

	// 阶段二：等同次尝试的对端画像（服务器在双方画像齐后下发）。
	//
	// 等待有上限：探测预算 + 较大一档打洞预算。对端画像迟到的唯一合法
	// 原因是它的探测还没做完（受它自己的探测预算约束，两端配置通常
	// 相同）加上配对下发的网络延迟；超过这个窗口还没到，对端必然已经
	// 探测失败——它的失败上报只改服务器侧链路状态，不会通知本端。
	// 没有上限的等待会让 Worker 永卡 CONNECTING：socket 与 goroutine
	// 不释放，快照还会把服务器已被对端纠正的 IDLE 覆盖回 CONNECTING。
	// 归入 PUNCH_TIMEOUT：探测、画像交换与打洞是同一次尝试的整体预算。
	waitBudget := worker.config.Punch.VariableTimeout
	if worker.config.Punch.StableTimeout > waitBudget {
		waitBudget = worker.config.Punch.StableTimeout
	}
	waitTimer := time.NewTimer(worker.config.Probe.Timeout + waitBudget)
	var peerProfile common.NATProfile
	select {
	case peerProfile = <-worker.incoming:
		waitTimer.Stop()
	case <-waitTimer.C:
		worker.fail(common.ReasonPunchTimeout)
		return
	case <-worker.ctx.Done():
		return
	}

	// 阶段三：打洞。两级路径之外的组合直接判定。
	if profile.NAT == common.NATVariable && peerProfile.NAT == common.NATVariable {
		worker.fail(common.ReasonNATUnsupported)
		return
	}
	worker.punch(profile, peerProfile)
}

// probe 向服务器 5 个连续探测端口各发一个 PROBE 并组装画像。
// 任何端口在预算内无响应 → PROBE_TIMEOUT。回显公网 IP 不一致（家宽按流
// 轮换出口）不再拒绝：一律取首个回显当固定值继续打洞，轮换仅日志留痕。
func (worker *linkWorker) probe(serverIP net.IP) (common.NATProfile, common.Reason) {
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return common.NATProfile{}, common.ReasonInternalError
	}
	nonce := hex.EncodeToString(nonceBytes)
	socket := worker.mainSocketRef()

	responses := make([]common.ProbeResponse, common.ProbePortCount)
	observed := make([]string, common.ProbePortCount)
	totalCtx, cancel := context.WithTimeout(worker.ctx, worker.config.Probe.Timeout)
	defer cancel()
	for index := 0; index < common.ProbePortCount; index++ {
		target := &net.UDPAddr{IP: serverIP, Port: worker.config.Server.ProbeBasePort + index}
		response, err := worker.probeOne(totalCtx, socket, target, nonce, uint8(index+1))
		if err != nil {
			// 画像失败必须带观察样本：公网 IP 轮换类问题没有样本时
			// 只能靠抓包反推（真实三机测试的教训）。
			worker.log.Warn("probe failed", "error", err.Error(), "observed", joinObserved(observed[:index]))
			if worker.ctx.Err() != nil {
				return common.NATProfile{}, "" // 被 DISCONNECT 取消，不是失败
			}
			return common.NATProfile{}, common.ReasonProbeTimeout
		}
		responses[index] = response
		observed[index] = string(response.PublicIP) + ":" + fmt.Sprint(int(response.MappedPort))
		worker.log.Debug("probe port answered", "port", target.Port, "public_ip", string(response.PublicIP), "mapped_port", int(response.MappedPort))
	}
	profile, reason := classifyProfile(responses, observed)
	if ipChanged := profileIPChanged(responses); ipChanged {
		// 不再因 IP 轮换失败：与 p2p 项目同口径，取首个回显 IP 当固定值
		// 继续打洞。轮换仍要留痕（带观察样本，公网排障只能靠它），
		// 但判定权交给真机测试而不是探测层。
		worker.log.Warn("probe observed rotating public IPs, using first", "public_ip", string(profile.PublicIP), "observed", joinObserved(observed))
	}
	// 画像是打洞策略的全部依据，公网排障第一件事就是看这两行。
	worker.log.Debug("nat profile observed", "nat", profile.NAT, "public_ip", string(profile.PublicIP), "ports", fmt.Sprintf("%v", profile.Ports))
	return profile, reason
}

// profileIPChanged 报告五份回显是否存在公网 IP 不一致（仅用于日志留痕）。
func profileIPChanged(responses []common.ProbeResponse) bool {
	publicIP := responses[0].PublicIP
	for _, response := range responses[1:] {
		if response.PublicIP != publicIP {
			return true
		}
	}
	return false
}

// classifyProfile 把五份回显组装成画像：公网 IP 一律取首个回显（不校验
// 一致性，出口轮换由调用方留痕放行）+ 端口分类。
func classifyProfile(responses []common.ProbeResponse, observed []string) (common.NATProfile, common.Reason) {
	publicIP := responses[0].PublicIP
	ports := make([]common.Port, len(responses))
	stable := true
	for index, response := range responses {
		ports[index] = response.MappedPort
		if ports[index] != ports[0] {
			stable = false
		}
	}
	nat := common.NATVariable
	if stable {
		nat = common.NATStable
	}
	profile := common.NATProfile{NAT: nat, PublicIP: publicIP, Ports: ports}
	return profile, ""
}

// joinObserved 把观察样本拼成日志字符串。
func joinObserved(observed []string) string {
	out := ""
	for index, sample := range observed {
		if index > 0 {
			out += " "
		}
		out += sample
	}
	return out
}

// probeOne 对单个端口做「发送-等待-匹配」循环，含配置的重试次数。
func (worker *linkWorker) probeOne(totalCtx context.Context, socket *net.UDPConn, target *net.UDPAddr, nonce string, probeID uint8) (common.ProbeResponse, error) {
	request, err := common.EncodeProbeRequest(common.ProbeRequest{Nonce: nonce, ProbeID: probeID})
	if err != nil {
		return common.ProbeResponse{}, err
	}
	for attempt := 0; attempt <= worker.config.Probe.Retries; attempt++ {
		if totalCtx.Err() != nil {
			return common.ProbeResponse{}, totalCtx.Err()
		}
		if _, err := socket.WriteToUDP(request, target); err != nil {
			return common.ProbeResponse{}, err
		}
		deadline := time.Now().Add(worker.config.Probe.PerPortTimeout)
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 || totalCtx.Err() != nil {
				break
			}
			_ = socket.SetReadDeadline(time.Now().Add(remaining))
			buffer := make([]byte, common.MaxProbeDatagram+1)
			length, source, err := socket.ReadFromUDP(buffer)
			if err != nil {
				break // 本端口等待超时，进入下一次重试
			}
			response, err := common.ParseProbeResponse(buffer[:length])
			if err == nil && sameUDPAddr(source, target) && response.Nonce == nonce && response.ProbeID == probeID {
				_ = socket.SetReadDeadline(time.Time{})
				return response, nil
			}
		}
	}
	return common.ProbeResponse{}, fmt.Errorf("no response from probe port %d after %d retries", target.Port, worker.config.Probe.Retries)
}

// punch 按两侧画像选择路径并驱动三步握手直到 CONNECTED 或预算耗尽。
func (worker *linkWorker) punch(local, peer common.NATProfile) {
	budget := worker.config.Punch.StableTimeout
	if local.NAT == common.NATVariable || peer.NAT == common.NATVariable {
		budget = worker.config.Punch.VariableTimeout
	}
	deadline := time.Now().Add(budget)
	worker.log.Info("punch started", "local_nat", local.NAT, "peer_nat", peer.NAT, "budget", budget.String())
	worker.window.Printf("开始打洞（本机 NAT %s，对方 NAT %s）", notice.NAT(local.NAT), notice.NAT(peer.NAT))

	// 接收循环：主 socket + （variable 侧）全部 helper 各一个。
	worker.startReceiver(worker.mainSocketRef(), worker.mainSocketID, peer.PublicIP)

	switch {
	case local.NAT == common.NATVariable:
		// variable 侧：helper 池向对端 stable 端点轮询。创建失败不降档，
		// 整次尝试如实失败。helper 与主 socket 绑定同一本机地址，
		// 保证映射行为与画像阶段一致。
		sockets, err := CreateHelpers(worker.config.Punch.HelperCount, worker.localAddr)
		if err != nil {
			worker.log.Error("create helpers", "error", err)
			worker.fail(common.ReasonInternalError)
			return
		}
		worker.helperMu.Lock()
		worker.helpers = make([]helperSocket, len(sockets))
		for index, socket := range sockets {
			id := common.GenerateSocketID()
			worker.helpers[index] = helperSocket{socket: socket, id: id}
			worker.startReceiverLocked(socket, id, peer.PublicIP)
		}
		worker.helperMu.Unlock()
		worker.log.Debug("helper pool created", "count", len(sockets), "local_addr", worker.localAddr.String())
		target := peerEndpoint(peer)
		go worker.pollHelpers(target, deadline)
	default:
		if peer.NAT == common.NATVariable {
			// stable 侧：先等 helper 初始化窗口，再从对端最后观测端口向上
			// 做 Range 扫描（helper_count+48 候选 × 3ms，循环至预算耗尽）。
			go worker.rangeScan(peer, deadline)
		} else {
			// stable-stable：直连对端稳定映射端点。
			go worker.directPunch(peer, deadline)
		}
	}

	worker.own(deadline)
}

// own 是握手裁决循环：处理事件、预算到期、保活，直到终态。
func (worker *linkWorker) own(deadline time.Time) {
	deadlineTimer := time.After(time.Until(deadline))
	var pingTicker *time.Ticker
	var pingC, timeoutC <-chan time.Time
	var peerTimer *time.Timer
	var pingSequence uint32
	// link 身份：字典序小的一侧是 link[0]，握手不对称性以此为锚。
	isLink0 := worker.device < worker.peer.DeviceID

	for {
		select {
		case event := <-worker.eventsIn:
			if event.control.Token != worker.token {
				continue // 别的尝试（或噪声）的报文
			}
			// 建成后仍接受 PUNCH：回 ACK，link0 已建成时再补发 OK（见
			// handlePunch——对端可能还没建成，尤其它的扫描从
			// helperInitWindow 之后才开始）；只丢弃重复的 ACK/OK。
			// PING 是保活，同样继续处理。
			if worker.connected.Load() && event.kind != common.P2PTypePunch && event.kind != common.P2PTypePing {
				continue
			}
			worker.handle(event, isLink0)
			if worker.connected.Load() && pingC == nil {
				// 首次进入 CONNECTED：启动保活。
				pingTicker = time.NewTicker(keepaliveInterval)
				pingC = pingTicker.C
				peerTimer = time.NewTimer(keepaliveTimeout)
				timeoutC = peerTimer.C
				defer pingTicker.Stop()
			}
		case <-deadlineTimer:
			if !worker.connected.Load() {
				worker.fail(common.ReasonPunchTimeout)
				return
			}
		case <-pingC:
			pingSequence++
			worker.sendPing(pingSequence)
		case <-timeoutC:
			// 滑动失活：静默不足阈值就按剩余预算重武装。
			worker.dataMu.Lock()
			idle := time.Since(worker.lastActivity)
			worker.dataMu.Unlock()
			if idle >= keepaliveTimeout {
				worker.log.Warn("peer keepalive timed out")
				worker.window.Printf("对方心跳超时，隧道断开")
				worker.emit(WorkerEvent{PeeringID: worker.peering, Token: worker.token, Kind: WorkerLost, Reason: common.ReasonTunnelLost})
				return
			}
			peerTimer.Reset(keepaliveTimeout - idle)
		case <-worker.ctx.Done():
			return
		}
	}
}

// handle 处理一条握手报文。协议：
// PUNCH → ACK（link0 另发反向 PUNCH）；link0 收到合法 ACK 即建成并发
// PUNCH_OK（立即 + 100ms×3 重发）；link1 凭 ACK 记录 + PUNCH_OK 建成。
// 终态判定不在报文处理里：Worker 的退出统一由 own 的预算/失活/取消驱动。
func (worker *linkWorker) handle(event wireEvent, isLink0 bool) {
	switch event.kind {
	case common.P2PTypePunch:
		worker.handlePunch(event, isLink0)
	case common.P2PTypePunchACK:
		worker.log.Debug("punch ack received", "source", addrLabel(event.source), "socket", string(event.socketID))
		if isLink0 {
			worker.handleACKLink0(event)
		} else {
			worker.handleACKLink1(event)
		}
	case common.P2PTypePunchOK:
		worker.log.Debug("punch ok received", "source", addrLabel(event.source), "socket", string(event.socketID))
		if !isLink0 {
			worker.handleOKLink1(event)
		}
	case common.P2PTypePing:
		// 建成后仅接受选定路径上的 PING，helper 残留报文不算活动证据。
		worker.dataMu.Lock()
		socket, peer := worker.p2pSocket, worker.peerLive
		worker.dataMu.Unlock()
		if socket == event.socket && peer != nil && sameUDPAddr(event.source, peer) {
			worker.touchActivity()
		}
	}
}

// handlePunch 回 ACK；未建成时对新三元组补发一次反向 PUNCH（两种角色都发：
// 换回的 ACK 让 link0 直接建成、让 link1 记下关联，配对端已建成时的 OK
// 补发即可完成）；link0 已建成时改为补发 OK（对端此刻才记下 ACK 关联，
// commit 时的 OK 突发早已发完）。
func (worker *linkWorker) handlePunch(event wireEvent, isLink0 bool) {
	receiving := event.socketID
	// 观察设施（临时，随 scanlog.go 删除）：源端口若出自此前的扫描候选，
	// 这一发就是预测命中。必须先查后记——下面的登记会让本次查询恒真。
	guessed := worker.punchSentFrom(receiving, event.source)
	worker.notePredictionHit(receiving, event.source, "punch")
	scanInboundOnce(worker.token, event.source.Port, guessed)
	worker.recordPunchSent(receiving, event.source, punchSentRecord{sentAt: time.Now()})
	ack := common.P2PControl{Type: common.P2PTypePunchACK, Token: worker.token, TargetSocketID: event.control.SenderSocketID, SenderSocketID: receiving}
	if encoded, err := common.MarshalP2PControl(ack); err == nil {
		_, _ = event.socket.WriteToUDP(encoded, event.source)
	}
	if worker.connected.Load() {
		if !isLink0 {
			return
		}
		// 已建成的 link0 收到仍在打洞的对端的 PUNCH：ACK 照回（给对端记
		// 关联的材料），但 OK 不从本 socket 发——补发 OK 必须走本侧选定的
		// 路径。若照 PUNCH 来源回，link1 会在「收到 OK 的那个 socket」上
		// 建成，而它与 link0 选定的 socket 可能是两个不同 helper：对称
		// NAT 下两条映射互不相通，隧道从建成起就是双向黑洞（公网真机
		// 抓出：本机选 helper X、对端记 helper Y，双方 60s 保活双双超时）。
		worker.sendSelectedPathOK()
		return
	}
	// 未建成：对新三元组发一次反向 PUNCH。link1 侧同样需要——它的握手
	// 来源只有对端 PUNCH 的来源地址，扫描打不中随机分配的映射时
	// （运营商 NAT 常态，公网真机实证），这发反向 PUNCH 是对端能拿到
	// ACK 的唯一途径；同三元组去重，只发一次。
	key := reverseKey{sourcePort: event.source.Port, sender: event.control.SenderSocketID, receiving: receiving}
	copy(key.ip[:], event.source.IP.To4())
	if _, sent := worker.reverseSent[key]; sent {
		return
	}
	worker.reverseSent[key] = struct{}{}
	punch := common.P2PControl{Type: common.P2PTypePunch, Token: worker.token, SenderSocketID: receiving}
	if encoded, err := common.MarshalP2PControl(punch); err == nil {
		_, _ = event.socket.WriteToUDP(encoded, event.source)
	}
}

// handleACKLink0 校验 ACK 后建成连接、上报并发 PUNCH_OK（可靠重发）。
func (worker *linkWorker) handleACKLink0(event wireEvent) {
	if worker.connected.Load() || event.control.TargetSocketID != event.socketID || !worker.punchSentFrom(event.socketID, event.source) {
		return
	}
	worker.notePredictionHit(event.socketID, event.source, "ack")
	worker.commit(event.socket, event.socketID, event.control.SenderSocketID, event.source)
	ok := common.P2PControl{Type: common.P2PTypePunchOK, Token: worker.token, TargetSocketID: event.control.SenderSocketID, SenderSocketID: event.socketID}
	go worker.resendPunchOK(event.socket, ok, event.source)
}

// handleACKLink1 只记录关联，建成靠随后的 PUNCH_OK。
func (worker *linkWorker) handleACKLink1(event wireEvent) {
	if event.control.TargetSocketID != event.socketID || !worker.punchSentFrom(event.socketID, event.source) {
		return
	}
	worker.notePredictionHit(event.socketID, event.source, "ack")
	key := ackKey{localSocket: event.socketID, port: event.source.Port}
	copy(key.ip[:], event.source.IP.To4())
	worker.ackAssoc[key] = ackAssociation{peerSocketID: event.control.SenderSocketID}
}

// handleOKLink1 校验 OK 与先前 ACK 关联一致后建成连接。
func (worker *linkWorker) handleOKLink1(event wireEvent) {
	if worker.connected.Load() || event.control.TargetSocketID != event.socketID {
		return
	}
	key := ackKey{localSocket: event.socketID, port: event.source.Port}
	copy(key.ip[:], event.source.IP.To4())
	assoc, ok := worker.ackAssoc[key]
	if !ok || assoc.peerSocketID != event.control.SenderSocketID {
		return
	}
	worker.commit(event.socket, event.socketID, event.control.SenderSocketID, event.source)
}

// commit 选定 p2p 路径并宣布 CONNECTED。两侧必须收敛到同一条路径：
// 路径由（本侧 socket, 对侧地址+socket）三元组决定，对称 NAT 下不同
// helper 的映射互不相通——若两侧各自选中不同的 helper，隧道从建成起
// 就是双向黑洞（公网真机抓出，见 sendSelectedPathOK）。
func (worker *linkWorker) commit(socket *net.UDPConn, localID, peerID common.SocketID, source *net.UDPAddr) {
	worker.connected.Store(true)
	worker.dataMu.Lock()
	worker.p2pSocket = socket
	worker.p2pSocketID = localID
	worker.peerSocketID = peerID
	worker.peerLive = cloneUDPAddr(source)
	worker.lastActivity = time.Now()
	worker.dataMu.Unlock()
	worker.log.Info("punch connected", "peer", source.String(), "local_socket", string(localID), "peer_socket", string(peerID))
	worker.window.Printf("打洞成功，隧道已建立")
	worker.emit(WorkerEvent{PeeringID: worker.peering, Token: worker.token, Kind: WorkerConnected})
	close(worker.connectedCh) // 发送 goroutine 由此退出（helper 信标除外，见 pollHelpers）
	worker.pruneHelpers(socket)
}

// pruneHelpers 在建成时收缩 helper 池：只保留 commit 选中的那个 helper，
// 其余关闭（锁内摘除、锁外 Close，finish 同范式；被关 helper 的接收循环
// 随 Close 退出）。pollHelpers 逐发取当前切片，立即仅对选中 helper 继续
// 信标至
// 预算耗尽——它同时承担映射保活与 sendSelectedPathOK 的触发，收缩后
// 对端晚建成时的握手补发路径不受影响。commit 落在主 socket 时池内无
// 匹配项，全清即可（variable 侧正常经 helper 建成，此处只是防御）。
func (worker *linkWorker) pruneHelpers(keep *net.UDPConn) {
	worker.helperMu.Lock()
	kept := make([]helperSocket, 0, 1)
	var dropped []*net.UDPConn
	for _, helper := range worker.helpers {
		if helper.socket == keep {
			kept = append(kept, helper)
		} else {
			dropped = append(dropped, helper.socket)
		}
	}
	worker.helpers = kept
	worker.helperMu.Unlock()
	for _, socket := range dropped {
		_ = socket.Close()
	}
	if len(dropped) > 0 {
		worker.log.Info("helpers pruned to selected path", "closed", len(dropped))
		scanNote(worker.token, "helpers pruned", "closed", len(dropped))
	}
}

// sendSelectedPathOK 在本侧选定的路径上补发一次 PUNCH_OK：从 p2pSocket
// 发往 peerLive，Target 用对端选定 socket 的 ID。
//
// 两个使命：其一，对端（link1）可能在本侧 commit 后才开始打洞
// （variable×stable 中对端扫描自 helperInitWindow 后起步），commit 时的
// OK 突发（1+3 发）结束远早于它记下关联，需要持续补发；其二，补发必须
// 固定走选定路径，link1 只会在「OK 到达的 socket」上建成——路径因此
// 强制收敛为 link0 选定的那一条。对端已建成则按 connected 检查忽略。
// 每秒至多一次：helper 信标以 3ms 间隔送达 PUNCH，逐个补发只是刷屏。
func (worker *linkWorker) sendSelectedPathOK() {
	now := time.Now().UnixNano()
	for {
		last := atomic.LoadInt64(&worker.lastSupplement)
		if now-last < int64(time.Second) {
			return
		}
		if atomic.CompareAndSwapInt64(&worker.lastSupplement, last, now) {
			break
		}
	}
	worker.dataMu.Lock()
	socket, peer, peerID, localID := worker.p2pSocket, worker.peerLive, worker.peerSocketID, worker.p2pSocketID
	worker.dataMu.Unlock()
	if socket == nil || peer == nil || peerID == "" {
		return
	}
	ok := common.P2PControl{Type: common.P2PTypePunchOK, Token: worker.token, TargetSocketID: peerID, SenderSocketID: localID}
	if encoded, err := common.MarshalP2PControl(ok); err == nil {
		_, _ = socket.WriteToUDP(encoded, peer)
		worker.log.Debug("punch ok supplement sent on selected path", "target", string(peerID), "peer", addrLabel(peer))
	}
}

// resendPunchOK 负责协议要求的可靠重发：首发立即（函数入口这一次），
// 随后按 100ms 间隔补发 3 次——PUNCH_OK 是 link1 侧建成的唯一依据，
// 单发丢包会让已打通的路径卡死到超时。
//
// 注意不能像发送循环那样以 connectedCh 作为退出条件：本函数在 commit
// （connectedCh 已关闭）之后才启动，select 到已关闭的通道会立即命中，
// 重发一次都不会发生（公网真机测试抓包定位的缺陷）。
func (worker *linkWorker) resendPunchOK(socket *net.UDPConn, ok common.P2PControl, target *net.UDPAddr) {
	if encoded, err := common.MarshalP2PControl(ok); err == nil {
		_, _ = socket.WriteToUDP(encoded, target)
		worker.log.Debug("punch ok sent", "send", "immediate", "target", addrLabel(target))
	}
	for i := 0; i < punchOKResendCount; i++ {
		select {
		case <-worker.ctx.Done():
			return
		case <-time.After(punchOKResendInterval):
			if encoded, err := common.MarshalP2PControl(ok); err == nil {
				_, _ = socket.WriteToUDP(encoded, target)
				worker.log.Debug("punch ok sent", "send", fmt.Sprintf("resend %d/%d", i+1, punchOKResendCount), "target", addrLabel(target))
			}
		}
	}
}

// directPunch 用主 socket 周期性向对端稳定端点发 PUNCH（stable-stable）。
func (worker *linkWorker) directPunch(peer common.NATProfile, deadline time.Time) {
	target := peerEndpoint(peer)
	mainSocket := worker.mainSocketRef()
	for !worker.senderExited(deadline) {
		worker.sendPunch(mainSocket, worker.mainSocketID, target)
		if worker.waitSendInterval(punchDirectInterval) {
			return
		}
	}
}

// rangeScan 等待 helper 初始化窗口后，按三级候选流扫描对端 variable 侧
// 的 helper 映射端口：邻域（最后观测端口 ±10）→ 均匀扫描（步长
// helper_count/4 覆盖全端口空间）→ 无重复随机填满剩余预算。上一级发完
// 立即进入下一级；建成（senderExited 监听 connectedCh）、取消或预算耗尽
// 即止。候选生成见 scanplan.go；每发候选记观察日志（临时设施，scanlog.go）。
func (worker *linkWorker) rangeScan(peer common.NATProfile, deadline time.Time) {
	if worker.waitSendInterval(helperInitWindow) {
		return
	}
	ip := net.ParseIP(string(peer.PublicIP)).To4()
	lastPort := peer.Ports[len(peer.Ports)-1]
	helperCount := worker.config.Punch.HelperCount
	rng := newScanRng()
	global := 0
	defer worker.scanEnd(&global)
	scanNote(worker.token, "scan start", "peer_ip", string(peer.PublicIP), "last_port", int(lastPort),
		"helper_count", helperCount, "stride", helperCount/4,
		"budget", time.Until(deadline).Round(time.Millisecond).String())
	for !worker.senderExited(deadline) {
		stream := newScanStream(lastPort, helperCount, rng)
		lastStage := scanStage(0)
		for {
			candidate, ok := stream.next()
			if !ok {
				// 整个端口空间已发完（15s 预算 @3ms 发不到 64512 个候选，
				// 实际到不了这里）：重建流重扫，等价旧行为的循环重发。
				scanNote(worker.token, "scan stream exhausted, restarting", "sent", global)
				break
			}
			if worker.senderExited(deadline) {
				return
			}
			if candidate.Stage != lastStage {
				scanNote(worker.token, "stage begin", "stage", candidate.Stage.String(),
					"candidates", stream.stageCount(candidate.Stage))
				lastStage = candidate.Stage
			}
			global++
			target := &net.UDPAddr{IP: ip, Port: candidate.Port}
			worker.sendScanPunch(worker.mainSocketRef(), worker.mainSocketID, target, candidate, global)
			scanNote(worker.token, "candidate sent", "stage", candidate.Stage.String(),
				"ordinal", candidate.Ordinal, "global", global, "port", candidate.Port)
			if worker.waitSendInterval(punchPollInterval) {
				return
			}
		}
	}
}

// scanEnd 记扫描收尾（原因 + 总发包数）。临时观察设施，随 scanlog.go 删除。
func (worker *linkWorker) scanEnd(sent *int) {
	reason := "budget exhausted"
	if worker.connected.Load() {
		reason = "connected"
	} else if worker.ctx.Err() != nil {
		reason = "cancelled"
	}
	scanNote(worker.token, "scan end", "reason", reason, "sent", *sent)
}

// pollHelpers 让 helper 池按全局 3ms 间隔轮流向对端 stable 端点发 PUNCH。
// socket 与其 SocketID 同出 helpers 切片，SenderSocketID 必然属于发包的 socket。
//
// 每发都从当前 helpers 切片取（锁内逐发取，不再按轮快照）：commit 收缩
// 池后轮询立即收敛到选中的 helper，不会对着已关闭的 socket 空转一整轮——
// 按轮快照在收缩后会留下 helper_count×3ms 的无信标间隙。
//
// 信标语义：刻意不在本侧建成时停发（与 directPunch/rangeScan 的退出条件
// 不同）。variable 侧（尤其作为 link0）可能远早于对端建成——对端的握手
// 来源只有我们持续发出的 PUNCH（运营商 NAT 常按流随机分配端口，对端的
// 扫描打不中我们的映射，公网真机实证）。本侧建成后继续播信标直到预算
// 耗尽或取消，同时维持映射不过期；代价是预算内约 333 包/秒的发送，可忽略。
func (worker *linkWorker) pollHelpers(target *net.UDPAddr, deadline time.Time) {
	cursor := 0
	for !worker.beaconExited(deadline) {
		worker.helperMu.Lock()
		count := len(worker.helpers)
		var helper helperSocket
		if count > 0 {
			helper = worker.helpers[cursor%count]
			cursor++
		}
		worker.helperMu.Unlock()
		if count > 0 {
			worker.sendPunch(helper.socket, helper.id, target)
		}
		if worker.waitBeaconInterval(punchPollInterval) {
			return
		}
	}
}

// beaconExited 是 helper 信标的退出条件：只看取消与预算，不看建成。
func (worker *linkWorker) beaconExited(deadline time.Time) bool {
	select {
	case <-worker.ctx.Done():
		return true
	default:
		return time.Now().After(deadline)
	}
}

// waitBeaconInterval 在信标发送间隔内响应取消（不响应建成）。
func (worker *linkWorker) waitBeaconInterval(interval time.Duration) bool {
	select {
	case <-worker.ctx.Done():
		return true
	case <-time.After(interval):
		return false
	}
}

// startReceiver 启动单个 socket 的接收循环（锁外包装）。
func (worker *linkWorker) startReceiver(socket *net.UDPConn, id common.SocketID, peerIP common.IPv4) {
	worker.helperMu.Lock()
	defer worker.helperMu.Unlock()
	worker.startReceiverLocked(socket, id, peerIP)
}

// startReceiverLocked 需持有 helperMu（保证与 Stop 的关闭互斥）。
func (worker *linkWorker) startReceiverLocked(socket *net.UDPConn, id common.SocketID, peerIP common.IPv4) {
	expectedIP := net.ParseIP(string(peerIP)).To4()
	// 接收缓冲只须容纳最大的合法报文：GTUN 帧（16 头 + TUN MTU）或
	// P2P 控制报文（64）。按 64KB 固定分配在 1024 helper 档位下要吃掉
	// 64MB 纯缓冲，超出部分永远读不到。
	recvSize := common.GTUNHeaderBytes + worker.config.TUN.MTU
	if recvSize < common.MaxP2PControlDatagram+1 {
		recvSize = common.MaxP2PControlDatagram + 1
	}
	worker.wg.Add(1)
	go func() {
		defer worker.wg.Done()
		buffer := make([]byte, recvSize)
		for {
			_ = socket.SetReadDeadline(time.Now().Add(time.Second))
			length, source, err := socket.ReadFromUDP(buffer)
			if err != nil {
				if worker.ctx.Err() != nil {
					return
				}
				if asTimeout(err) {
					continue // 周期性超时让 ctx 取消可被感知
				}
				return // socket 已关闭
			}
			if length >= 4 && string(buffer[:4]) == "GTUN" {
				worker.deliverFrame(buffer[:length], source, socket)
				continue
			}
			control, err := common.ParseP2PControl(buffer[:length])
			if err != nil {
				continue // 非 P2P 控制报文的噪声
			}
			// 来源 IP 必须等于对端画像的公网 IP；端口不固定。
			if source == nil || !source.IP.Equal(expectedIP) {
				continue
			}
			select {
			case worker.eventsIn <- wireEvent{kind: control.Type, control: control, source: cloneUDPAddr(source), socket: socket, socketID: id}:
			case <-worker.ctx.Done():
				return
			}
		}
	}()
}

// sendPunch 从指定 socket 发一条 PUNCH 并登记 punchSent（ACK 校验用）。
func (worker *linkWorker) sendPunch(socket *net.UDPConn, id common.SocketID, target *net.UDPAddr) {
	worker.sendPunchRecorded(socket, id, target, punchSentRecord{sentAt: time.Now()})
}

// sendScanPunch 发一个三级扫描候选并登记阶段元数据（命中溯源用）。
func (worker *linkWorker) sendScanPunch(socket *net.UDPConn, id common.SocketID, target *net.UDPAddr, candidate scanCandidate, global int) {
	worker.sendPunchRecorded(socket, id, target, punchSentRecord{
		stage: candidate.Stage, ordinal: candidate.Ordinal, global: global, sentAt: time.Now(),
	})
}

// sendPunchRecorded 编码发送并登记。nil 判守：发送循环逐发取 socket 引用
// 与 finish 摘除 socket 之间存在竞态——own 的预算定时器和发送循环的
// senderExited 在同一 deadline 对齐，own 先返回触发 finish 把 mainSocket
// 置 nil，发送循环恰好刚通过检查正要发包时取到 nil，对 nil 调
// WriteToUDP 会 panic（尝试收尾路径上的崩溃，此前疑似三次客户端无声
// 退出的来源）。ctx 已被 finish 取消，弃发后发送循环下轮即退出。
func (worker *linkWorker) sendPunchRecorded(socket *net.UDPConn, id common.SocketID, target *net.UDPAddr, record punchSentRecord) {
	if socket == nil {
		return
	}
	punch := common.P2PControl{Type: common.P2PTypePunch, Token: worker.token, SenderSocketID: id}
	if encoded, err := common.MarshalP2PControl(punch); err == nil {
		_, _ = socket.WriteToUDP(encoded, target)
	}
	worker.recordPunchSent(id, target, record)
}

// deliverFrame 是入站 GTUN 帧的验证链，全部通过才投递数据面并刷新保活：
//  1. 已连接（CONNECTED 前不收数据）
//  2. 收包 socket == 选定的 p2p socket 且来源 == 选定的对端地址
//     （helper 残留路径与旧地址的帧不算数）
//  3. 帧可解码且 token == 当前尝试的 token
//  4. 内层 src == 对端虚拟 IP 且 dst == 本机虚拟 IP（防串流与地址欺骗）
//
// 通过校验的帧即对端活动证据，刷新保活失活窗口。
// 每个拒绝分支都写 Debug 日志：入站黑洞类故障（ping 通但业务不通）
// 的定位全靠这条链上「帧死在哪一步」。
func (worker *linkWorker) deliverFrame(datagram []byte, source *net.UDPAddr, socket *net.UDPConn) {
	if !worker.connected.Load() {
		return // 握手未完成：对端提前发数据属乱序，静默丢弃
	}
	if worker.deliverInbound == nil {
		return // 数据面未就绪（栈未开或正在重建）：无处投递，丢弃
	}
	worker.dataMu.Lock()
	p2pSocket, peerLive := worker.p2pSocket, worker.peerLive
	worker.dataMu.Unlock()
	if socket != p2pSocket || peerLive == nil || !sameUDPAddr(source, peerLive) {
		worker.log.Debug("inbound frame from non-selected path; dropped",
			"source", addrLabel(source), "socket_selected", socket == p2pSocket)
		return
	}
	frame, err := common.DecodeGTUNFrame(datagram, worker.config.TUN.MTU)
	if err != nil {
		worker.log.Debug("inbound frame failed GTUN decode; dropped", "size", len(datagram), "error", err.Error())
		return
	}
	if frame.Token != worker.token {
		worker.log.Debug("inbound frame carries stale token; dropped", "frame_token", string(frame.Token))
		return
	}
	src, dst, ok := ipv4InnerEndpoints(frame.Payload)
	if !ok || src != worker.peerVirtualIP || dst != worker.localVirtualIP {
		worker.log.Debug("inbound frame inner addresses mismatch; dropped",
			"src", string(src), "dst", string(dst),
			"want_src", string(worker.peerVirtualIP), "want_dst", string(worker.localVirtualIP))
		return
	}
	worker.touchActivity()
	worker.deliverInbound(frame.Payload)
}

// addrLabel 把 UDP 地址安全地格式化为日志字符串（nil 时给占位符）。
func addrLabel(addr *net.UDPAddr) string {
	if addr == nil {
		return "<nil>"
	}
	return addr.String()
}

// ipv4InnerEndpoints 提取 IPv4 包的内层源与目的地址。
func ipv4InnerEndpoints(packet []byte) (src, dst common.IPv4, ok bool) {
	if len(packet) < 20 {
		return "", "", false
	}
	return common.IPv4(net.IPv4(packet[12], packet[13], packet[14], packet[15]).String()),
		common.IPv4(net.IPv4(packet[16], packet[17], packet[18], packet[19]).String()), true
}

// AttemptToken 实现 tun.WorkerLink：数据面发帧时取当前尝试的 token。
// 每次发帧都重新取——重新打洞后 token 会变，缓存会让帧带旧 token 被对端丢弃。
func (worker *linkWorker) AttemptToken() common.LinkToken { return worker.token }

// PeerLive 实现 tun.WorkerLink：返回握手选定的对端地址。
func (worker *linkWorker) PeerLive() (*netip.AddrPort, bool) {
	worker.dataMu.Lock()
	defer worker.dataMu.Unlock()
	if worker.peerLive == nil {
		return nil, false
	}
	addr, ok := netip.AddrFromSlice(worker.peerLive.IP)
	if !ok {
		return nil, false
	}
	address := netip.AddrPortFrom(addr.Unmap(), uint16(worker.peerLive.Port))
	return &address, true
}

// SendBatch 实现 tun.WorkerLink：批量发送出站帧。
// UDP 写不设期限：无连接语义下写阻塞不是可恢复状态，失败即丢，
// 可靠性由上层协议（TCP over TUN 的重传）承担。
// Linux 上用 x/net 的 WriteBatch（底层 sendmmsg）一次系统调用发多帧；
// macOS/Windows 没有 sendmmsg，保持逐包 WriteToUDP——与改造前行为
// 完全一致。批量路径任何错误（含部分成功）都回落逐包发送，绝不丢帧。
func (worker *linkWorker) SendBatch(_ context.Context, frames [][]byte) error {
	worker.dataMu.Lock()
	socket, peer := worker.p2pSocket, worker.peerLive
	worker.dataMu.Unlock()
	if socket == nil || peer == nil {
		return errors.New("worker not connected")
	}
	if len(frames) == 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		for _, frame := range frames {
			if _, err := socket.WriteToUDP(frame, peer); err != nil {
				return err
			}
		}
		return nil
	}
	packet := ipv4.NewPacketConn(socket)
	msgs := make([]ipv4.Message, 0, len(frames))
	for _, frame := range frames {
		msgs = append(msgs, ipv4.Message{Addr: peer, Buffers: [][]byte{frame}})
	}
	sent, err := packet.WriteBatch(msgs, 0)
	if err == nil && sent == len(msgs) {
		return nil
	}
	// 部分成功（sendmmsg 出错时也返回已成功发送的数量）：无论 err 与否，
	// 前 sent 帧都已发出，只回落发送剩余——重发已成功的帧会白白放大流量。
	if sent > 0 {
		msgs = msgs[sent:]
	}
	for _, msg := range msgs {
		if _, err := socket.WriteToUDP(msg.Buffers[0], msg.Addr.(*net.UDPAddr)); err != nil {
			return err
		}
	}
	return nil
}

// sendPing 在选定路径上发保活 PING。sequence 仅抓包对账用，见 GTUNFrame.Sequence。
func (worker *linkWorker) sendPing(sequence uint32) {
	worker.dataMu.Lock()
	socket, peer := worker.p2pSocket, worker.peerLive
	worker.dataMu.Unlock()
	if socket == nil || peer == nil {
		return
	}
	ping := common.P2PControl{Type: common.P2PTypePing, Token: worker.token, Sequence: sequence}
	if encoded, err := common.MarshalP2PControl(ping); err == nil {
		_, _ = socket.WriteToUDP(encoded, peer)
	}
}

// recordPunchSent / lookupPunchSent / punchSentFrom 维护「本 socket 曾向谁
// 发过 PUNCH」。
func (worker *linkWorker) recordPunchSent(id common.SocketID, target *net.UDPAddr, record punchSentRecord) {
	key := punchSentKey{localSocket: id, port: target.Port}
	copy(key.ip[:], target.IP.To4())
	worker.punchSentMu.Lock()
	worker.punchSent[key] = record
	worker.punchSentMu.Unlock()
}

// lookupPunchSent 返回该来源的发送记录（命中溯源用）。
func (worker *linkWorker) lookupPunchSent(id common.SocketID, source *net.UDPAddr) (punchSentRecord, bool) {
	key := punchSentKey{localSocket: id, port: source.Port}
	copy(key.ip[:], source.IP.To4())
	worker.punchSentMu.Lock()
	defer worker.punchSentMu.Unlock()
	record, ok := worker.punchSent[key]
	return record, ok
}

// punchSentFrom 查询「本 socket 是否向该来源发过 PUNCH」（ACK 校验用）。
func (worker *linkWorker) punchSentFrom(id common.SocketID, source *net.UDPAddr) bool {
	_, ok := worker.lookupPunchSent(id, source)
	return ok
}

// notePredictionHit 在入站报文通过 punchSent 校验后调用：来源端口若出自
// 三级扫描候选，这次入站就是预测命中的直接证据，记观察日志并镜像一行到
// 主日志（低频高价值）。kind 标注入站类型（punch/ack），仅日志用。
// 临时观察设施，随 scanlog.go 一并删除。
func (worker *linkWorker) notePredictionHit(id common.SocketID, source *net.UDPAddr, kind string) {
	record, ok := worker.lookupPunchSent(id, source)
	if !ok || record.stage == 0 {
		return
	}
	scanHit(worker.token, record.stage, record.ordinal, source.Port, time.Since(record.sentAt))
	worker.log.Info("prediction hit", "kind", kind,
		"stage", record.stage.String(), "ordinal", record.ordinal, "global", record.global, "port", source.Port)
}

// mainSocketRef 在锁内取主 socket 引用；已关闭（finish 置 nil）返回 nil，
// 调用方对 nil 的写/读会立即报错退出，等价于「尝试已终结」。
func (worker *linkWorker) mainSocketRef() *net.UDPConn {
	worker.dataMu.Lock()
	defer worker.dataMu.Unlock()
	return worker.mainSocket
}

// touchActivity 刷新保活窗口。
func (worker *linkWorker) touchActivity() {
	worker.dataMu.Lock()
	worker.lastActivity = time.Now()
	worker.dataMu.Unlock()
}

// senderExited 判定发送 goroutine 是否应退出。
func (worker *linkWorker) senderExited(deadline time.Time) bool {
	select {
	case <-worker.connectedCh:
		return true
	case <-worker.ctx.Done():
		return true
	default:
		return time.Now().After(deadline)
	}
}

// waitSendInterval 在发送间隔内响应建成与取消；返回 true 表示应退出。
func (worker *linkWorker) waitSendInterval(interval time.Duration) bool {
	select {
	case <-worker.connectedCh:
		return true
	case <-worker.ctx.Done():
		return true
	case <-time.After(interval):
		return false
	}
}

// emit 向控制循环汇报里程碑。非阻塞：控制面可能已断开，
// 丢失的只是上报时机，Worker 状态不受影响（重连全量上报补齐）。
func (worker *linkWorker) emit(event WorkerEvent) {
	select {
	case worker.events <- event:
	default:
		worker.log.Warn("worker event dropped (control session busy or down)", "kind", event.Kind)
	}
}

// fail 上报失败并终结。
func (worker *linkWorker) fail(reason common.Reason) {
	worker.log.Warn("attempt failed", "reason", string(reason))
	worker.window.Printf("打洞失败（%s）", string(reason))
	worker.emit(WorkerEvent{PeeringID: worker.peering, Token: worker.token, Kind: WorkerFailed, Reason: reason})
}

// finish 是 Worker 的统一收尾：幂等、关 socket、等接收循环退出。
func (worker *linkWorker) finish() {
	if !worker.finished.CompareAndSwap(false, true) {
		return
	}
	worker.cancel()
	worker.dataMu.Lock()
	mainSocket := worker.mainSocket
	worker.mainSocket = nil
	worker.dataMu.Unlock()
	if mainSocket != nil {
		_ = mainSocket.Close()
	}
	worker.helperMu.Lock()
	helpers := worker.helpers
	worker.helpers = nil
	worker.helperMu.Unlock()
	for _, helper := range helpers {
		_ = helper.socket.Close()
	}
	worker.wg.Wait()
}

// peerEndpoint 取 stable 画像的公网端点（五个映射端口全等，取第一个）。
func peerEndpoint(profile common.NATProfile) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(string(profile.PublicIP)).To4(), Port: int(profile.Ports[0])}
}

// sameUDPAddr 比较两个 UDP 地址是否相同。
func sameUDPAddr(left, right *net.UDPAddr) bool {
	return left != nil && right != nil && left.Port == right.Port && left.IP.Equal(right.IP)
}

// cloneUDPAddr 复制一份 UDP 地址，避免接收缓冲复用后被篡改。
func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	duplicate := *addr
	return &duplicate
}

// asTimeout 判定错误是否为网络层超时。
func asTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
