package client

import (
	"log/slog"
	"sync"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// Manager 管理客户端的全部本地状态：TUN 数据面栈与每条链路的 Worker。
//
// 并发模型：控制会话有两个 goroutine——读循环（收下行消息，调用
// ApplyConfig/HandleConnect/...）与主循环（消费 Worker 事件，调用
// applyEvent）。二者都会触碰 workers 表，因此表由 mu 保护；
// 锁序恒为 manager.mu → plane.mu（Register/UnregisterLink），
// 反向不存在。Worker 在自己的 goroutine 里运行，经 events 通道汇报
// 里程碑，经原子标志暴露当前状态——全量上报读表时拿到的永远是实况，
// 控制面断连期间建成/失败的隧道在重连上报里依然是准确值。
//
// 配置应用始终全量替换，不做增量对比。全量应用是幂等的——拓扑未变时
// 跳过重建（sameTUNTopology），拓扑变化时先拆旧栈再过 preflight，预检
// 执行时系统里不存在任何 GTun 路由，重复应用同一份配置总能通过。
// 这也是配置不需要携带代次的前提（见 common.NetworkConfig 的不变量）：
// 若将来为省流量改成增量推送，必须先重新引入代次来标识基线，
// 否则会产生「基于错误基线应用了增量」这类极难定位的缺陷。
type Manager struct {
	mu         sync.Mutex
	log        *slog.Logger
	config     ClientConfig
	device     common.DeviceID
	opener     tun.Opener
	routeTable tun.RouteTable
	// network 是最近一次应用的网络配置，nil 表示从未收到过。
	network *common.NetworkConfig
	// stack 是当前 TUN 数据面栈；nil 表示尚未打开（无网络或打开失败）。
	stack *dataStack
	// stackFailureReason 记录最近一次栈打开失败的原因分类。栈未就绪期间
	// 收到 CONNECT 即以此回报转场失败——页面必须看到「失败+原因」，
	// 而不是打洞成功却装不了数据的假 CONNECTED。
	stackFailureReason common.Reason
	// workers 是配对 → 当前尝试。键存在且未终结即在尝试中。
	workers map[common.PeeringID]*linkWorker
	// events 是 Worker 里程碑的汇入通道，控制循环消费并翻译成 TCP 上报。
	events chan WorkerEvent
}

// NewManager 创建管理器。opener 与 routeTable 由调用方注入
// （生产用平台实现，测试用假实现），manager 不感知平台差异。
func NewManager(config ClientConfig, device common.DeviceID, opener tun.Opener, routeTable tun.RouteTable, log *slog.Logger) *Manager {
	return &Manager{
		log:        log,
		config:     config,
		device:     device,
		opener:     opener,
		routeTable: routeTable,
		workers:    make(map[common.PeeringID]*linkWorker),
		events:     make(chan WorkerEvent, 64),
	}
}

// Events 返回 Worker 里程碑通道，仅供控制循环消费。
func (manager *Manager) Events() <-chan WorkerEvent { return manager.events }

// serverIP 解析配置里服务器地址的 IPv4（preflight 用：/32 不得覆盖服务器）。
func (manager *Manager) serverIP() common.IPv4 {
	ip, err := serverIPv4(manager.config.Server.Addr)
	if err != nil {
		return ""
	}
	return common.IPv4(ip.String())
}

// ApplyConfig 全量替换配置。TUN 相关拓扑（本机 IP、CIDR、对端集合）变化时
// 整栈重建：先停全部 Worker（旧地址下的隧道不可能继续服务），再关旧栈开新栈。
// 拓扑未变（如仅对端在线标志变化）则不动栈也不动 Worker——数据面与打洞
// 不受展示性字段影响。
func (manager *Manager) ApplyConfig(config *common.NetworkConfig) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if sameTUNTopology(manager.network, config) {
		// 拓扑未变：仅记录新配置（在线标志之类的展示性字段），栈与 Worker
		// 一律不动——重建会白白打断正在跑的隧道与打洞。
		manager.log.Debug("config applied without topology change; stack kept")
		manager.network = config
		return
	}
	// 拓扑变化：整栈重建。Worker 分两类——本机 IP 未变且配对、对端 IP
	// 仍在 新配置里的「幸存者」保留（UDP 隧道不依赖 TUN，栈重开期间
	// PING 保活继续），其余停止：地址已变，旧隧道物理上不可达。
	survivors := make(map[common.PeeringID]*linkWorker)
	for peeringID, worker := range manager.workers {
		if manager.workerSurvives(config, worker) {
			survivors[peeringID] = worker
			continue
		}
		manager.stopWorker(peeringID)
	}
	if manager.stack != nil {
		manager.stack.close(manager.log)
		manager.stack = nil
	}
	manager.network = config
	if config == nil || config.Network == nil {
		manager.log.Info("no network; data stack closed")
		return
	}
	if !manager.tryOpenStackLocked() {
		// 幸存者的 UDP 隧道虽在，但没有数据面就无法承载流量：
		// 停止并如实上报，不装活。
		for peeringID, worker := range survivors {
			manager.emitFailure(peeringID, worker.token, manager.stackFailureReason)
			manager.stopWorker(peeringID)
		}
		return
	}
	stack := manager.stack
	// 已建成的幸存者重新挂接到新数据面；打洞中的不用处理——
	// 建成事件到达时挂的是当前栈。
	for peeringID, worker := range survivors {
		if worker.connected.Load() {
			if err := stack.plane.RegisterLink(worker, worker.peer.IP); err != nil {
				manager.log.Error("reattach data link", "peering_id", string(peeringID), "error", err)
			}
		}
	}
	manager.log.Info("stack rebuilt", "reattached", len(survivors))
}

// tryOpenStackLocked 尝试打开数据面栈（需持 mu）。栈已在时直接成功；
// 无网络拓扑时失败。ApplyConfig（拓扑重建）与 HandleConnect（失败后
// 重试）共用：开栈失败可能是暂时性的——典型如已被清理的路由残留——
// 收到新的建链意图时值得再试一次，而不是停留在「重启进程才能恢复」
// 的单次闭锁。
func (manager *Manager) tryOpenStackLocked() bool {
	if manager.stack != nil {
		return true
	}
	if manager.network == nil || manager.network.Network == nil {
		return false
	}
	stack, reason, err := openDataStack(manager.opener, manager.routeTable, manager.config, manager.network.Network, manager.serverIP(), manager.log)
	if err != nil {
		manager.stackFailureReason = reason
		manager.log.Error("open data stack failed", "error", err)
		return false
	}
	manager.stackFailureReason = ""
	manager.stack = stack
	return true
}

// workerSurvives 判断一个 Worker 能否跨过本次栈重建：本机虚拟 IP 未变、
// 配对仍在 新配置里、对端 IP 未变。三者任一变化，旧隧道的内层地址
// 校验（dst==本机 IP、src==对端 IP）都会失效，必须重建。
func (manager *Manager) workerSurvives(config *common.NetworkConfig, worker *linkWorker) bool {
	if config == nil || config.Network == nil || manager.network == nil || manager.network.Network == nil {
		return false
	}
	if manager.network.Network.IP != config.Network.IP {
		return false
	}
	for _, peer := range config.Network.Peers {
		if peer.PeeringID == worker.peering && peer.IP == worker.peer.IP {
			return true
		}
	}
	return false
}

// emitFailure 向控制循环投递失败里程碑（由其翻译成转场失败上报）。
// 非阻塞：控制面阻塞时丢失的只是上报时机，重连全量上报兜底。
func (manager *Manager) emitFailure(peeringID common.PeeringID, token common.LinkToken, reason common.Reason) {
	select {
	case manager.events <- WorkerEvent{PeeringID: peeringID, Token: token, Kind: WorkerFailed, Reason: reason}:
	default:
		manager.log.Warn("failure event dropped", "peering_id", string(peeringID), "reason", string(reason))
	}
}

// sameTUNTopology 判断两份配置的 TUN 拓扑是否一致：网络 ID、CIDR、
// 本机 IP、对端集合（设备、配对、IP）。名称与在线标志是展示性字段，
// 变化不触发重建。
func sameTUNTopology(previous, next *common.NetworkConfig) bool {
	if (previous == nil) != (next == nil) {
		return false
	}
	if previous == nil {
		return true
	}
	if (previous.Network == nil) != (next.Network == nil) {
		return false
	}
	if previous.Network == nil {
		return true
	}
	a, b := previous.Network, next.Network
	if a.ID != b.ID || a.CIDR != b.CIDR || a.IP != b.IP || len(a.Peers) != len(b.Peers) {
		return false
	}
	seen := make(map[common.PeeringID]common.NetworkPeer, len(a.Peers))
	for _, peer := range a.Peers {
		seen[peer.PeeringID] = peer
	}
	for _, peer := range b.Peers {
		existing, ok := seen[peer.PeeringID]
		if !ok || existing.DeviceID != peer.DeviceID || existing.IP != peer.IP {
			return false
		}
	}
	return true
}

// localIP 返回本机虚拟 IP（无网络时为空）。
func (manager *Manager) localIP() common.IPv4 {
	if manager.network == nil || manager.network.Network == nil {
		return ""
	}
	return manager.network.Network.IP
}

// deliverInbound 是注入 Worker 的入站投递回调：栈在则投递，栈不在则丢。
// 读栈指针必须持 mu——Worker 接收 goroutine 与 ApplyConfig（写栈）并发。
// 只做快照后立刻放锁：DeliverInbound 是无锁的通道 try-send，
// 长持锁会让重建期间的入站帧无谓阻塞。
func (manager *Manager) deliverInbound(packet []byte) {
	manager.mu.Lock()
	stack := manager.stack
	manager.mu.Unlock()
	if stack != nil {
		stack.deliverInbound(packet)
	}
}

// stopWorker 停止并移除一个 Worker，并从数据面注销其出站队列。需持 mu。
func (manager *Manager) stopWorker(peeringID common.PeeringID) {
	worker, ok := manager.workers[peeringID]
	if !ok {
		return
	}
	worker.Stop()
	delete(manager.workers, peeringID)
	if manager.stack != nil {
		manager.stack.plane.UnregisterLink(worker.peer.IP)
	}
}

// HandleConnect 处理服务器的建链意图：同一配对旧 Worker 先停（重试就是
// 换新 token 重建），再启动新 Worker 走探测→打洞→数据面全流程。
func (manager *Manager) HandleConnect(message *common.Connect) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopWorker(message.PeeringID)
	if manager.stack == nil {
		// 数据面未就绪：先重试一次开栈——失败可能是暂时性的（残留已被
		// 清理、上次的接口拆除已完成）。重开仍失败才立即以真实原因回报
		// 失败，不启动 Worker——打洞成功却装不了数据，只会让管理页面
		// 显示一个不能用的 CONNECTED。
		if !manager.tryOpenStackLocked() {
			reason := manager.stackFailureReason
			if reason == "" {
				reason = common.ReasonTUNCreateFailed
			}
			manager.emitFailure(message.PeeringID, message.Token, reason)
			manager.log.Warn("connect rejected: data stack not ready", "peering_id", string(message.PeeringID), "reason", string(reason))
			return
		}
		manager.log.Info("data stack recovered on connect", "peering_id", string(message.PeeringID))
	}
	manager.workers[message.PeeringID] = startLinkWorker(
		manager.config, manager.device, message.Token, message.PeeringID, message.Peer,
		manager.localIP(), manager.deliverInbound, manager.events, manager.log)
	manager.log.Info("connect received", "peering_id", string(message.PeeringID), "peer", string(message.Peer.DeviceID), "token", string(message.Token))
}

// HandleDisconnect 处理服务器的拆链意图：停止并移除 Worker。
// token 不一致只记日志不拒绝——服务器意图是权威的，按 peering_id 停止即可。
func (manager *Manager) HandleDisconnect(message *common.Disconnect) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if worker, ok := manager.workers[message.PeeringID]; ok && worker.token != message.Token {
		manager.log.Warn("disconnect token differs from current attempt",
			"peering_id", string(message.PeeringID), "current", string(worker.token), "disconnect", string(message.Token))
	}
	manager.stopWorker(message.PeeringID)
	manager.log.Info("disconnect received", "peering_id", string(message.PeeringID))
}

// HandlePeerProfile 把服务器配对出的对端画像投递给对应 Worker。
// token 不匹配的画像属于旧尝试，丢弃。
func (manager *Manager) HandlePeerProfile(message *common.PeerProfile) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	worker, ok := manager.workers[message.PeeringID]
	if !ok || worker.token != message.Token {
		manager.log.Info("peer profile for unknown or stale attempt ignored", "peering_id", string(message.PeeringID))
		return
	}
	worker.deliverPeerProfile(message.Profile)
}

// applyEvent 把 Worker 里程碑落到管理器视图：建成即注册数据面出站队列
// （此后 TUN 读到的目的地址为对端虚拟 IP 的包开始出站），失败与失活
// 移除 Worker 并注销队列。
func (manager *Manager) applyEvent(event WorkerEvent) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	worker, ok := manager.workers[event.PeeringID]
	if ok && worker.token != event.Token {
		// 迟到里程碑属于已被替换的上一次尝试（CONNECT 重试换新 token 重建
		// Worker）：旧 Worker 的建成/失败不得注册或误停新尝试的队列。
		return
	}
	switch event.Kind {
	case WorkerConnected:
		if !ok || manager.stack == nil {
			return
		}
		if err := manager.stack.plane.RegisterLink(worker, worker.peer.IP); err != nil {
			manager.log.Error("register data link", "peering_id", string(event.PeeringID), "error", err)
		} else {
			manager.log.Debug("data link registered to plane", "peering_id", string(event.PeeringID), "peer_ip", string(worker.peer.IP))
		}
	case WorkerFailed, WorkerLost:
		manager.stopWorker(event.PeeringID)
	case WorkerProfile:
		// 状态由 Worker 原子标志承载，无需改表。
	}
}

// StateReport 生成全量状态上报：配置里每个对端一条，Worker 存活时
// 取其实时状态（原子读），否则 IDLE。
//
// 按配置枚举而不是按 Worker 表枚举：上报的语义是「我这边每条链路的
// 真实状态」，链路的集合由配置定义。这让断连期间死掉的隧道
// （Worker 已终结）也有一条 IDLE 上报，把服务器可能陈旧的
// CONNECTED 纠正回来。
func (manager *Manager) StateReport() *common.StateReport {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	report := &common.StateReport{Type: common.MessageStateReport, Full: true, Links: []common.LinkReport{}}
	if manager.network == nil || manager.network.Network == nil {
		return report // 不属于任何网络：没有任何链路可报
	}
	for _, peer := range manager.network.Network.Peers {
		entry := common.LinkReport{PeeringID: peer.PeeringID, State: common.StateIdle}
		if worker, ok := manager.workers[peer.PeeringID]; ok {
			if state, token := worker.currentState(); state != common.StateIdle {
				entry.State = state
				entry.Token = token
			}
		}
		report.Links = append(report.Links, entry)
	}
	return report
}

// Close 停止全部 Worker 并拆卸数据面栈（进程优雅退出路径调用）。
func (manager *Manager) Close() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for peeringID := range manager.workers {
		manager.stopWorker(peeringID)
	}
	if manager.stack != nil {
		manager.stack.close(manager.log)
		manager.stack = nil
	}
}
