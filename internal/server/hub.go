package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"gtun-lite/internal/common"
)

// ErrSessionNotCurrent 表示消息来自一条已被顶替或已结束的连接。
// 顶替发生在同一设备重连时：旧连接的后续消息不得再影响任何状态。
var ErrSessionNotCurrent = errors.New("session is not current")

// ErrDeviceOffline 表示目标设备控制连接不在线（QUERY 之类单播操作的目标）。
var ErrDeviceOffline = errors.New("device is not online")

// ErrServerClosed 表示 hub 已停止，拒绝受理新命令。
var ErrServerClosed = errors.New("server hub is closed")

// ErrConnectionLimit 表示在线会话数已到 max_connections，注册被拒。
// 单独成哨兵是为了让连接层能把它翻译成 server_full 而非 internal_error：
// 前者告诉客户端「重试无意义，等别人下线」，后者只会被当成服务器故障。
var ErrConnectionLimit = errors.New("connection limit reached")

// outbound 是写向一条连接的一条消息；closeAfter 为 true 时写完即关连接，
// 用于 duplicate_login 与 error 这类终止消息。
type outbound struct {
	message    common.Message
	closeAfter bool
}

// session 是一台设备的当前控制连接。发送缓冲满时阻塞（控制面语义，
// 见设计 2.3），由 deliver 的卡死超时兜底。
type session struct {
	device  common.DeviceID
	conn    net.Conn
	send    chan outbound
	dead    chan struct{}
	endOnce sync.Once
}

// end 关闭连接并广播会话终结。幂等：读循环退出、写循环失败、hub 顶替
// 三条路径都会走到这里，重复调用必须无害。
func (sess *session) end() {
	sess.endOnce.Do(func() {
		close(sess.dead)
		_ = sess.conn.Close()
	})
}

// deliver 把消息排进发送队列，满则阻塞直到写循环消费、会话终结或卡死超时。
//
// 控制面不丢消息：CONNECT/DISCONNECT 半途丢弃会制造「一侧收到、另一侧
// 不知道」的分裂状态——那正是不变量 2 要在结构上排除的东西。真正的发送
// 卡死意味着 TCP 已经病危，此时关闭会话、让链路靠客户端上报自行收敛
// （不变量 3 保证一侧的超时上报足以把链路打回 IDLE）。
func (sess *session) deliver(message common.Message, closeAfter bool, stall time.Duration) error {
	select {
	case sess.send <- outbound{message: message, closeAfter: closeAfter}:
		return nil
	case <-sess.dead:
		return ErrSessionNotCurrent
	case <-time.After(stall):
		return fmt.Errorf("send queue stalled for over %s", stall)
	}
}

// hubState 只被 owner goroutine 触碰，因此全部字段无锁。
type hubState struct {
	// sessions 是设备 → 当前会话的唯一映射。键存在即在线。
	sessions map[common.DeviceID]*session
	// links 是链路的内存状态，键为字典序规范化的设备对。
	// 条目按需创建（下发过 CONNECT 或收到过上报），不随会话增删——
	// TCP 断开不触发链路状态转换（不变量 1），自然也不触发它的增删。
	links map[common.Link]*Link
	// pending 是每条链路「正在收集的本次尝试画像」。两侧的 profile_report
	// 都到齐时，服务器把对方的画像发给每一侧，随后清掉条目。
	// 条目随 IssueConnect 重建：新尝试的画像不与旧尝试的混放。
	pending map[common.Link]*pendingProfiles
	// pendingDevices 是已注册但未通过审批的设备（注册审批制）：只有
	// name/platform，纯内存实时状态——在线即存在（会话建立时写入），
	// 断开即消失。管理页「同意注册」才把它落库为正式设备。
	pendingDevices map[common.DeviceID]deviceRegistration
}

// deviceRegistration 是待审批设备的注册信息快照，来自其注册报文。
type deviceRegistration struct {
	Name     string
	Platform string
}

// pendingProfiles 是一次尝试的画像收集状态。token 与链路当前 token
// 不符的迟到上报整条忽略——旧尝试的画像不能为新尝试决定打洞路径。
type pendingProfiles struct {
	token common.LinkToken
	sides map[common.DeviceID]common.NATProfile
}

// hub 是会话与链路状态的唯一 owner。
//
// 所有会改变这两张表的操作——注册、顶替、状态上报、CONNECT/DISCONNECT、
// 配置推送——都经由 commands 串行执行。串行化同时解决了三件事：
//
//   - 并发安全：hubState 无锁。
//   - 配置顺序：DB 写入、配置组装与投递在同一条命令内完成，两条管理写操作
//     不会交错出「新库旧配置后到」的乱序（全量推送不变量的会话内依据）。
//   - 在线判定原子性：CONNECT 的双方在线检查与下发在同一条命令内，
//     检查通过后连接不会在命令执行中途消失。
//
// SQLite 写入只发生在 owner goroutine，连接池永远只有一个写入者。
type Hub struct {
	store    *Store
	config   ServerConfig
	log      *slog.Logger
	commands chan func(*hubState)
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewHub 创建并启动 owner 循环。
func NewHub(store *Store, config ServerConfig, log *slog.Logger) *Hub {
	owner := &Hub{
		store:    store,
		config:   config,
		log:      log,
		commands: make(chan func(*hubState)),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go owner.run()
	return owner
}

// run 是两张表的唯一执行线程。
func (owner *Hub) run() {
	defer close(owner.done)
	state := &hubState{
		sessions:       make(map[common.DeviceID]*session),
		links:          make(map[common.Link]*Link),
		pending:        make(map[common.Link]*pendingProfiles),
		pendingDevices: make(map[common.DeviceID]deviceRegistration),
	}
	for {
		select {
		case command := <-owner.commands:
			command(state)
		case <-owner.stop:
			// 停机先排干已入队的命令再退出：submit 的契约是 err 非 nil ⇔
			// 未生效，排干让「入队成功」与「必然执行」重新等价。收敛保证：
			// commands 无缓冲，本循环退出后再无接收者，排队段的新命令必然
			// 落入 stop 分支被拒，不会出现永远排不完的队列。
			for {
				select {
				case command := <-owner.commands:
					command(state)
				default:
					// 停机时主动断开全部会话；链路状态就此丢弃，这是设计行为：
					// 它是纯内存缓存，客户端重连全量上报即重建。
					for _, sess := range state.sessions {
						sess.end()
					}
					return
				}
			}
		}
	}
}

// submit 把操作排入 owner 并等它执行完，返回值契约：err 非 nil ⇔ 命令
// 未生效。排队阶段响应取消与停机（此时命令尚未入队，丢弃无副作用）；
// 入队成功后由 owner 的停机排干保证执行（见 run），这里只等 done——
// 若中途对停机让步，已执行完的命令会被误报为未生效。
func (owner *Hub) submit(ctx context.Context, execute func(*hubState)) error {
	done := make(chan struct{})
	select {
	case owner.commands <- func(state *hubState) { defer close(done); execute(state) }:
	case <-owner.stop:
		return ErrServerClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	<-done
	return nil
}

// Close 停止 owner 并断开全部会话。
func (owner *Hub) Close() {
	owner.stopOnce.Do(func() { close(owner.stop) })
	<-owner.done
}

// Register 处理一条连接的注册消息：落库设备身份、顶替旧会话、推送首份配置。
// 返回的错误仅表示协议失败（身份非法、库错误），此时连接必须终止。
func (owner *Hub) Register(ctx context.Context, sess *session, registration *common.DeviceRegister) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		failure = owner.register(state, sess, registration)
	})
	if err != nil {
		return err
	}
	return failure
}

// register 是注册的 owner 内实现。顶替顺序：先落库（或记待审批），再顶旧
// pendingDeviceLimit 是待审批连接的独立上限。不进配置：它的职责只是
// 防未审批连接无限堆积（内存与连接资源），额度与正式容量语义无关，
// 暴露成配置项徒增调优面。
const pendingDeviceLimit = 128

// approvedOnlineCount 由现有两张表推导已批准设备的在线数：sessions 含
// 全部在线会话（批准与否），pendingDevices 的键必是其中待审批的子集。
// 推导式计数不引入需多点维护的增量状态。
func approvedOnlineCount(state *hubState) int {
	pendingOnline := 0
	for device := range state.pendingDevices {
		if _, online := state.sessions[device]; online {
			pendingOnline++
		}
	}
	return len(state.sessions) - pendingOnline
}

// 会话，最后入表——任何一步失败都不产生半注册状态。
func (owner *Hub) register(state *hubState, sess *session, registration *common.DeviceRegister) error {
	// 批准状态先行：容量检查按批准与否分流——正式设备的名额只被正式
	// 设备占用，未审批连接堆满 max_connections 也不该把已批准设备拒之
	// 门外（server_full）。重连的旧设备不受影响——顶替替换的是既有
	// 表项，不增加条目。
	approved, err := owner.store.HasDevice(context.Background(), registration.DeviceID)
	if err != nil {
		return fmt.Errorf("check device approved: %w", err)
	}
	if _, exists := state.sessions[sess.device]; !exists {
		if approved && approvedOnlineCount(state) >= owner.config.Control.MaxConnections {
			return fmt.Errorf("%w: %d sessions in use", ErrConnectionLimit, owner.config.Control.MaxConnections)
		}
		if !approved && len(state.pendingDevices) >= pendingDeviceLimit {
			return fmt.Errorf("%w: %d pending registrations", ErrConnectionLimit, pendingDeviceLimit)
		}
	}
	// 注册审批制：库里已有的设备（此前被同意过）照常 upsert 刷新；
	// 新设备不落库，只记入内存待审批表，等管理页「同意注册」。
	if approved {
		if err := owner.store.UpsertDevice(context.Background(), registration.DeviceID, registration.Name, registration.Platform); err != nil {
			return fmt.Errorf("persist device: %w", err)
		}
	} else {
		state.pendingDevices[sess.device] = deviceRegistration{Name: registration.Name, Platform: registration.Platform}
	}
	if previous, replaced := state.sessions[sess.device]; replaced && previous != sess {
		// 顶替：给旧连接发 duplicate_login 并在其写出后关闭。注意不能在这里
		// 直接 end()：end 会立刻关闭连接，写循环可能还没把通知写出去——
		// 关闭动作由写循环在 closeAfter 消息写出后自己执行。入队失败
		// （对端早已死透）才由这里兜底终结。
		if err := previous.deliver(&common.DuplicateLogin{Type: common.MessageDuplicateLogin}, true, owner.config.Control.WriteTimeout); err != nil {
			previous.end()
		}
		owner.log.Warn("device re-registered; previous session replaced", "device_id", string(sess.device))
	}
	state.sessions[sess.device] = sess
	owner.log.Info("device registered", "device_id", string(sess.device), "name", registration.Name, "platform", registration.Platform, "approved", approved)
	// 确认先于首份配置入队：客户端的注册握手在等到 device_registered 后
	// 才开始等 network_config，顺序颠倒会让握手把配置当成乱序消息拒掉。
	// 两条消息走同一发送队列，入队顺序即线上顺序。
	_ = sess.deliver(&common.DeviceRegistered{Type: common.MessageDeviceRegistered}, false, owner.config.Control.WriteTimeout)
	owner.pushConfig(state, sess.device)
	return nil
}

// SessionEnded 通报一条连接的读或写循环退出。只在该会话仍是当前会话时
// 把它摘出在线表；链路状态什么都不做——这是不变量 1 的实现位置：
// 控制面断开在服务器侧的全部后果就是「这台设备离线了」，一条日志而已。
func (owner *Hub) SessionEnded(ctx context.Context, sess *session) {
	_ = owner.submit(ctx, func(state *hubState) {
		if current, ok := state.sessions[sess.device]; ok && current == sess {
			delete(state.sessions, sess.device)
			// 待审批设备是纯内存实时状态：断开即从待加入列表消失，
			// 重连会重新出现。只清当前会话的——被顶替的旧会话退出时
			// 条目属于新会话，不能误删。
			delete(state.pendingDevices, sess.device)
			owner.log.Info("device offline", "device_id", string(sess.device))
		}
	})
}

// Heartbeat 校验会话仍是当前会话。读超时已由连接层的 SetReadDeadline 承担，
// 心跳消息唯一的语义就是「这条连接还活着并且自认是当前会话」。
func (owner *Hub) Heartbeat(ctx context.Context, sess *session) error {
	current := false
	err := owner.submit(ctx, func(state *hubState) {
		current = state.sessions[sess.device] == sess
	})
	if err != nil {
		return err
	}
	if !current {
		return ErrSessionNotCurrent
	}
	return nil
}

// HandleStateReport 处理客户端状态上报，Full 决定采信方式（见
// common.StateReport 的注释）：快照直接覆盖，转场上报走 token 守卫。
func (owner *Hub) HandleStateReport(ctx context.Context, sess *session, report *common.StateReport) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		if state.sessions[sess.device] != sess {
			failure = ErrSessionNotCurrent
			return
		}
		for _, entry := range report.Links {
			owner.adoptReport(state, sess.device, entry, report.Full)
		}
	})
	if err != nil {
		return err
	}
	return failure
}

// adoptReport 把单条上报换算到对应链路。peering 已被删除的上报直接忽略——
// 配置先行删除时客户端迟早会收到新配置并停止对应尝试。
func (owner *Hub) adoptReport(state *hubState, reporter common.DeviceID, entry common.LinkReport, full bool) {
	peering, err := owner.store.PeeringByID(context.Background(), entry.PeeringID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			owner.log.Warn("state report for unknown peering ignored", "peering_id", string(entry.PeeringID))
		} else {
			owner.log.Error("lookup peering for state report", "error", err)
		}
		return
	}
	link, err := common.NewLink(peering.DeviceA, peering.DeviceB)
	if err != nil {
		owner.log.Error("peering row contains invalid device pair", "error", err)
		return
	}
	// 上报者必须是链路的一端。token 守卫挡住不知情者，这一行挡住
	// 「知情第三方冒充上报」——身份与配对关系的绑定是防线的另一半。
	if reporter != peering.DeviceA && reporter != peering.DeviceB {
		owner.log.Warn("state report from device outside the peering ignored",
			"peering_id", string(entry.PeeringID), "reporter", string(reporter))
		return
	}
	record := state.linkFor(link)
	token := entry.Token
	reason := string(entry.Reason) // 空串=无，封闭表里空值本来就非法
	switch {
	case full:
		// 快照：直接采信，客户端是唯一事实来源。
		if !record.AdoptClientReport(parseWireState(entry.State), token, time.Now()) {
			owner.log.Warn("self-contradictory report ignored", "peering_id", string(entry.PeeringID), "state", entry.State)
		}
	case entry.State == common.StateConnected:
		// 转场成功：token 守卫，迟到的旧尝试成功不得复活已回 IDLE 的链路。
		if !record.ReportSuccess(token, time.Now()) {
			owner.log.Info("stale success report ignored", "peering_id", string(entry.PeeringID))
		} else {
			owner.log.Info("link connected", "devices", linkDeviceNames(peering), "reported_by", string(reporter))
		}
	default:
		// 转场失败：token 守卫，旧尝试的迟到失败不得打断新尝试。
		if !record.ReportFailure(token, entry.Reason, time.Now()) {
			owner.log.Info("stale failure report ignored", "peering_id", string(entry.PeeringID), "state", entry.State, "reason", reason)
		} else {
			owner.log.Info("link failed", "devices", linkDeviceNames(peering), "reported_by", string(reporter), "reason", reason)
		}
	}
}

// linkFor 返回链路的内存状态，不存在则建 IDLE 空条目。
func (state *hubState) linkFor(link common.Link) *Link {
	record, ok := state.links[link]
	if !ok {
		record = &Link{}
		state.links[link] = record
	}
	return record
}

// parseWireState 把线上状态字符串换算为 LinkState。DecodeMessage 已拒绝
// 表外取值，这里 default 分支覆盖不到，返回 IDLE 仅为让函数可编译。
func parseWireState(state string) LinkState {
	switch state {
	case common.StateConnecting:
		return LinkConnecting
	case common.StateConnected:
		return LinkConnected
	default:
		return LinkIdle
	}
}

// HandleProfileReport 处理客户端的画像上报：token 匹配当前尝试才收纳，
// 两侧到齐即向双方下发对端画像（peer_profile），打洞由此开始。
func (owner *Hub) HandleProfileReport(ctx context.Context, sess *session, report *common.ProfileReport) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		if state.sessions[sess.device] != sess {
			failure = ErrSessionNotCurrent
			return
		}
		owner.adoptProfile(state, sess.device, report)
	})
	if err != nil {
		return err
	}
	return failure
}

// adoptProfile 收纳一侧画像；到齐则下发对端画像并清理收集状态。
func (owner *Hub) adoptProfile(state *hubState, reporter common.DeviceID, report *common.ProfileReport) {
	peering, err := owner.store.PeeringByID(context.Background(), report.PeeringID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			owner.log.Warn("profile report for unknown peering ignored", "peering_id", string(report.PeeringID))
		} else {
			owner.log.Error("lookup peering for profile report", "error", err)
		}
		return
	}
	link, err := common.NewLink(peering.DeviceA, peering.DeviceB)
	if err != nil {
		owner.log.Error("peering row contains invalid device pair", "error", err)
		return
	}
	// 画像只能来自链路一端：外来上报会顶占 pending 的一个侧位，
	// 两侧「到齐」后下发的将是全零画像。
	if reporter != peering.DeviceA && reporter != peering.DeviceB {
		owner.log.Warn("profile report from device outside the peering ignored",
			"peering_id", string(report.PeeringID), "reporter", string(reporter))
		return
	}
	record := state.linkFor(link)
	if record.State != LinkConnecting || record.Token != report.Token {
		// 迟到的旧尝试画像：链路已换 token 或已回 IDLE，忽略。
		owner.log.Info("stale profile report ignored", "peering_id", string(report.PeeringID), "token", string(report.Token))
		return
	}
	waiting := state.pending[link]
	if waiting == nil || waiting.token != report.Token {
		waiting = &pendingProfiles{token: report.Token, sides: make(map[common.DeviceID]common.NATProfile)}
		state.pending[link] = waiting
	}
	if _, duplicate := waiting.sides[reporter]; duplicate {
		return // 同侧重复上报，保留首份
	}
	waiting.sides[reporter] = report.Profile
	if len(waiting.sides) < 2 {
		return
	}
	// 两侧画像齐了：把「对方的」发给每一侧。到不了的一方随链路
	// 超时上报自行收敛（不变量 3），不阻塞已下发的一侧。
	owner.log.Info("peer profiles paired", "devices", [2]string{string(link[0]), string(link[1])},
		"nat_a", waiting.sides[link[0]].NAT, "nat_b", waiting.sides[link[1]].NAT)
	for _, side := range link {
		peerProfile := waiting.sides[otherSide(link, side)]
		message := &common.PeerProfile{Type: common.MessagePeerProfile, Token: report.Token, PeeringID: report.PeeringID, Profile: peerProfile}
		if sess, online := state.sessions[side]; online {
			if err := sess.deliver(message, false, owner.config.Control.WriteTimeout); err != nil {
				owner.log.Error("deliver peer profile", "device_id", string(side), "error", err)
				sess.end()
			}
		}
	}
	delete(state.pending, link)
}

// otherSide 返回设备对中的另一侧。
func otherSide(link common.Link, side common.DeviceID) common.DeviceID {
	if side == link[0] {
		return link[1]
	}
	return link[0]
}

// IssueConnect 下发建链意图（管理操作）。检查双方在线、生成 token、
// 状态转 CONNECTING、向双方投递 Connect——全部在同一条 owner 命令内，
// 在线检查与投递之间不存在会话消失的窗口。任一侧离线返回 ErrPeerOffline
// 且状态不动（不变量 2）。
func (owner *Hub) IssueConnect(ctx context.Context, pair common.Link) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		failure = owner.issueConnect(state, pair)
	})
	if err != nil {
		return err
	}
	return failure
}

// issueConnect 是 CONNECT 的 owner 内实现。
func (owner *Hub) issueConnect(state *hubState, pair common.Link) error {
	peering, err := owner.store.PeeringForDevicePair(context.Background(), pair)
	if err != nil {
		return err // ErrNotFound：这对设备没有配对，无从建链
	}
	sideA, onlineA := state.sessions[pair[0]]
	sideB, onlineB := state.sessions[pair[1]]
	if !onlineA || !onlineB {
		// 不变量 2：拒绝下发且不改状态。返回的 ErrPeerOffline 由管理 API
		// 翻译成 PEER_OFFLINE，操作者看到的是「先让两端上线」而不是静默失败。
		return ErrPeerOffline
	}
	token := common.GenerateLinkToken()
	record := state.linkFor(pair)
	record.IssueConnect(token, time.Now())
	// 新尝试作废旧画像：尚未配对的收集状态随 token 一并重置。
	delete(state.pending, pair)
	// 同一 token 发给双方；投递失败意味着该侧连接病危，直接终结会话。
	// 链路状态保持 CONNECTING：另一侧会开始打洞并最终以 PUNCH_TIMEOUT
	// 上报失败，链路经不变量 3 收敛回 IDLE。
	messageA := &common.Connect{Type: common.MessageConnect, Token: token, PeeringID: peering.PeeringID, Peer: common.ConnectPeer{DeviceID: pair[1], Name: peering.NameB, IP: peering.VirtualIPB}}
	messageB := &common.Connect{Type: common.MessageConnect, Token: token, PeeringID: peering.PeeringID, Peer: common.ConnectPeer{DeviceID: pair[0], Name: peering.NameA, IP: peering.VirtualIPA}}
	if err := sideA.deliver(messageA, false, owner.config.Control.WriteTimeout); err != nil {
		owner.log.Error("deliver connect to first side", "device_id", string(pair[0]), "error", err)
		sideA.end()
	}
	if err := sideB.deliver(messageB, false, owner.config.Control.WriteTimeout); err != nil {
		owner.log.Error("deliver connect to second side", "device_id", string(pair[1]), "error", err)
		sideB.end()
	}
	owner.log.Info("connect issued", "devices", linkDeviceNames(peering), "token", string(token))
	return nil
}

// IssueDisconnect 下发强制拆链（管理操作）。同样要求双方在线（不变量 2）。
func (owner *Hub) IssueDisconnect(ctx context.Context, pair common.Link) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		failure = owner.issueDisconnect(state, pair)
	})
	if err != nil {
		return err
	}
	return failure
}

// issueDisconnect 是 DISCONNECT 的 owner 内实现。检查顺序与 CONNECT 一致：
// 先确认配对存在（404），再查双方在线（409），都过了才改状态——
// 查库失败时链路条目根本不会被创建，不存在无配对的孤儿状态。
func (owner *Hub) issueDisconnect(state *hubState, pair common.Link) error {
	peering, err := owner.store.PeeringForDevicePair(context.Background(), pair)
	if err != nil {
		return err
	}
	sideA, onlineA := state.sessions[pair[0]]
	sideB, onlineB := state.sessions[pair[1]]
	if !onlineA || !onlineB {
		return ErrPeerOffline
	}
	record := state.linkFor(pair)
	token := record.Token
	if token == "" {
		// 链路在内存里已是 IDLE：从未建链、失败已收敛，或服务器重启丢了
		// 状态而客户端隧道可能还在。协议要求 Disconnect.token 非空，而
		// 客户端本就不校验它（按 peering_id 执行，token 仅供日志比对）——
		// 生成占位值保持消息合法，拆链意图照常送达。
		token = common.GenerateLinkToken()
	}
	record.IssueDisconnect(time.Now())
	messageA := &common.Disconnect{Type: common.MessageDisconnect, Token: token, PeeringID: peering.PeeringID}
	messageB := &common.Disconnect{Type: common.MessageDisconnect, Token: token, PeeringID: peering.PeeringID}
	if err := sideA.deliver(messageA, false, owner.config.Control.WriteTimeout); err != nil {
		owner.log.Error("deliver disconnect to first side", "device_id", string(pair[0]), "error", err)
		sideA.end()
	}
	if err := sideB.deliver(messageB, false, owner.config.Control.WriteTimeout); err != nil {
		owner.log.Error("deliver disconnect to second side", "device_id", string(pair[1]), "error", err)
		sideB.end()
	}
	owner.log.Info("disconnect issued", "devices", [2]string{string(pair[0]), string(pair[1])})
	return nil
}

// ApproveDevice 把待审批设备落库为正式设备（管理页「同意注册」）。
// 待审批是纯内存实时状态：设备不在线即条目不存在，无从批准（ErrNotFound）。
// 批准只改变「能不能入网」，不改变配置——无成员设备的全量配置本来就是空。
func (owner *Hub) ApproveDevice(ctx context.Context, device common.DeviceID) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		entry, ok := state.pendingDevices[device]
		if !ok {
			failure = ErrNotFound
			return
		}
		if err := owner.store.UpsertDevice(context.Background(), device, entry.Name, entry.Platform); err != nil {
			failure = fmt.Errorf("persist approved device: %w", err)
			return
		}
		delete(state.pendingDevices, device)
		owner.log.Info("device approved", "device_id", string(device), "name", entry.Name)
	})
	if err != nil {
		return err
	}
	return failure
}

// Kick 终结设备的当前控制会话（删除在线设备时调用）。设备不在线则无事可做。
// 客户端会按重连间隔重试；已批准设备重连照常，已删除设备回到待审批。
func (owner *Hub) Kick(ctx context.Context, device common.DeviceID) error {
	return owner.submit(ctx, func(state *hubState) {
		if sess, online := state.sessions[device]; online {
			sess.end()
		}
	})
}

// IsOnline 查询设备控制连接是否在线（管理操作前置检查用）。
func (owner *Hub) IsOnline(ctx context.Context, device common.DeviceID) bool {
	online := false
	_ = owner.submit(ctx, func(state *hubState) {
		_, online = state.sessions[device]
	})
	return online
}

// IssueQuery 向单个在线客户端下发 QUERY，客户端以全量 state_report 回应。
// 只读单向，不要求对端在线；目标设备自己离线则无从询问，如实报错。
func (owner *Hub) IssueQuery(ctx context.Context, device common.DeviceID) error {
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		sess, online := state.sessions[device]
		if !online {
			failure = ErrDeviceOffline
			return
		}
		if err := sess.deliver(&common.Query{Type: common.MessageQuery}, false, owner.config.Control.WriteTimeout); err != nil {
			owner.log.Error("deliver query", "device_id", string(device), "error", err)
			sess.end()
			failure = err
		}
	})
	if err != nil {
		return err
	}
	return failure
}

// PushConfig 给一台在线设备重推全量配置（配置变更后由管理写操作调用）。
// 离线设备跳过：它重连时注册流程会推首份配置，不需要补发。
// PushConfig 向设备推送其当前生效配置。不接受调用方 ctx：配置推送是
// 管理写操作的第二步（库已提交），跟随请求取消会造成「库已变更、在线
// 端永远不知情」的半提交，客户端要等到下次重连才收敛。推送失败如实记
// 错误日志；收敛兜底是客户端重连时的全量上报。
func (owner *Hub) PushConfig(device common.DeviceID) {
	if err := owner.submit(context.Background(), func(state *hubState) {
		owner.pushConfig(state, device)
	}); err != nil {
		owner.log.Error("push config after admin write", "device_id", string(device), "error", err)
	}
}

// PruneLinks 清除指定设备对的内存链路条目。配对/成员被删除后调用：
// 链路视图按库里的配对枚举，孤儿条目既不再展示也不再被上报触达，
// 留着只会随时间累积。
func (owner *Hub) PruneLinks(pairs []common.Link) {
	// 与 PushConfig 同一立场：管理写的后续清理不随请求取消。
	if err := owner.submit(context.Background(), func(state *hubState) {
		for _, pair := range pairs {
			delete(state.links, pair)
			delete(state.pending, pair)
		}
	}); err != nil {
		owner.log.Error("prune links after admin write", "error", err)
	}
}

// pushConfig 组装并投递一台设备的全量配置。在 owner 内执行保证与
// 其他写操作的顺序。投递失败终结会话，客户端重连后重新拿配置。
func (owner *Hub) pushConfig(state *hubState, device common.DeviceID) {
	sess, online := state.sessions[device]
	if !online {
		return
	}
	config, err := owner.assembleConfig(state, device)
	if err != nil {
		owner.log.Error("assemble network config", "device_id", string(device), "error", err)
		return
	}
	if err := sess.deliver(config, false, owner.config.Control.WriteTimeout); err != nil {
		owner.log.Error("deliver network config", "device_id", string(device), "error", err)
		sess.end()
	}
}

// assembleConfig 从库里组装设备的全量网络配置。在线标志来自会话表快照，
// 因此必须在 owner 内调用才能与注册/断开串行化。
func (owner *Hub) assembleConfig(state *hubState, device common.DeviceID) (*common.NetworkConfig, error) {
	member, err := owner.store.Membership(context.Background(), device)
	if errors.Is(err, ErrNotFound) {
		// 设备不属于任何网络：全量配置就是「你什么都没有」。
		return &common.NetworkConfig{Type: common.MessageNetworkConfig, Network: nil}, nil
	}
	if err != nil {
		return nil, err
	}
	network, err := owner.store.GetNetwork(context.Background(), member.NetworkID)
	if err != nil {
		return nil, err
	}
	definition := &common.NetworkDefinition{
		ID:    network.ID,
		Name:  network.Name,
		CIDR:  network.CIDR,
		IP:    member.VirtualIP,
		Peers: []common.NetworkPeer{},
	}
	peerings, err := owner.store.ListPeerings(context.Background(), network.ID)
	if err != nil {
		return nil, err
	}
	for _, peering := range peerings {
		var peerSide *common.ConnectPeer
		switch device {
		case peering.DeviceA:
			peerSide = &common.ConnectPeer{DeviceID: peering.DeviceB, Name: peering.NameB, IP: peering.VirtualIPB}
		case peering.DeviceB:
			peerSide = &common.ConnectPeer{DeviceID: peering.DeviceA, Name: peering.NameA, IP: peering.VirtualIPA}
		default:
			continue // 这条配对与我无关
		}
		_, peerOnline := state.sessions[peerSide.DeviceID]
		definition.Peers = append(definition.Peers, common.NetworkPeer{
			DeviceID:  peerSide.DeviceID,
			PeeringID: peering.PeeringID,
			Name:      peerSide.Name,
			IP:        peerSide.IP,
			Online:    peerOnline,
		})
	}
	config := &common.NetworkConfig{Type: common.MessageNetworkConfig, Network: definition}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("assembled config failed validation: %w", err)
	}
	return config, nil
}

// DeviceView 是 GET /api/devices 的条目：设备行加实时在线性。
type DeviceView struct {
	ID       common.DeviceID `json:"device_id"`
	Name     string          `json:"name"`
	Platform string          `json:"platform"`
	Online   bool            `json:"online"`
	// Pending 为 true 表示已注册但未通过审批：只存在于内存待审批表，
	// 恒在线（断开即消失），不能入网。false 即已落库的正式设备。
	Pending bool `json:"pending"`
	// NetworkID 空串=不属于任何网络，与项目「空串即无」惯例一致。
	NetworkID common.NetworkID `json:"network_id"`
}

// LinkView 是 GET /api/links 的条目。三段展示的数据源在这里齐备：
// 在线性（实时会话表）、最后已知状态与采集时刻（内存链路状态）。
// Token 一并给出——排查「哪次尝试」时它是唯一标识。
// LastReason 是最近一次已结束尝试的失败原因（宽松语义见 Link.LastReason）。
type LinkView struct {
	NetworkID  common.NetworkID `json:"network_id"`
	PeeringID  common.PeeringID `json:"peering_id"`
	DeviceA    common.DeviceID  `json:"device_a"`
	NameA      string           `json:"name_a"`
	VirtualIPA common.IPv4      `json:"virtual_ip_a"`
	OnlineA    bool             `json:"online_a"`
	DeviceB    common.DeviceID  `json:"device_b"`
	NameB      string           `json:"name_b"`
	VirtualIPB common.IPv4      `json:"virtual_ip_b"`
	OnlineB    bool             `json:"online_b"`
	State      string           `json:"state"`
	Token      string           `json:"token"`
	UpdatedAt  time.Time        `json:"updated_at"`
	LastReason string           `json:"last_reason"`
}

// Snapshot 一次取得设备与链路的展示视图。全部在 owner 内组装，
// 在线性与链路状态取自同一瞬间，不会出现「A 侧在线、B 侧离线」
// 与「链路 CONNECTED」这种自相矛盾的组合。
func (owner *Hub) Snapshot(ctx context.Context) ([]DeviceView, []LinkView, error) {
	// 空结果也必须是 []：设计约定 nil slice 统一序列化为空数组，
	// 不让「一台设备都没有」在线上表现成 null。
	devices := make([]DeviceView, 0)
	links := make([]LinkView, 0)
	// 查询失败必须原样返回给管理端：闭包内 err := 会遮蔽外层，
	// 库故障被吞成「成功的空列表」曾让运维面对假数据。设备/网络/
	// 配对三类是列表的主体，任一失败即整体失败——部分视图违反
	// 本方法「同一瞬间一致视图」的约定；membership 失败仅记日志，
	// 该设备网络列显示为空，不拖垮整页。
	var failure error
	err := owner.submit(ctx, func(state *hubState) {
		rows, err := owner.store.ListDevices(context.Background())
		if err != nil {
			failure = fmt.Errorf("list devices: %w", err)
			return
		}
		for _, row := range rows {
			networkID := common.NetworkID("")
			if member, err := owner.store.Membership(context.Background(), row.ID); err == nil {
				networkID = member.NetworkID
			} else if !errors.Is(err, ErrNotFound) {
				owner.log.Error("membership for snapshot", "device_id", string(row.ID), "error", err)
			}
			_, online := state.sessions[row.ID]
			devices = append(devices, DeviceView{ID: row.ID, Name: row.Name, Platform: row.Platform, Online: online, NetworkID: networkID})
		}
		// 待审批设备追加在正式设备之后，按 ID 排序保证输出稳定
		// （map 遍历无序，管理页与测试都需要确定性）。
		pendingIDs := make([]common.DeviceID, 0, len(state.pendingDevices))
		for id := range state.pendingDevices {
			pendingIDs = append(pendingIDs, id)
		}
		sort.Slice(pendingIDs, func(i, j int) bool { return pendingIDs[i] < pendingIDs[j] })
		for _, id := range pendingIDs {
			entry := state.pendingDevices[id]
			devices = append(devices, DeviceView{ID: id, Name: entry.Name, Platform: entry.Platform, Online: true, Pending: true})
		}
		networks, err := owner.store.ListNetworks(context.Background())
		if err != nil {
			failure = fmt.Errorf("list networks: %w", err)
			return
		}
		for _, network := range networks {
			peerings, err := owner.store.ListPeerings(context.Background(), network.ID)
			if err != nil {
				failure = fmt.Errorf("list peerings: %w", err)
				return
			}
			for _, peering := range peerings {
				pair, err := common.NewLink(peering.DeviceA, peering.DeviceB)
				if err != nil {
					continue
				}
				_, onlineA := state.sessions[pair[0]]
				_, onlineB := state.sessions[pair[1]]
				// 读路径不建条目：没有内存记录的链路就是 IDLE（零值），
				// GET /api/links 不得往状态表里写东西。
				view := LinkView{
					NetworkID: network.ID,
					PeeringID: peering.PeeringID,
					DeviceA:   pair[0], NameA: peering.NameA, VirtualIPA: peering.VirtualIPA, OnlineA: onlineA,
					DeviceB: pair[1], NameB: peering.NameB, VirtualIPB: peering.VirtualIPB, OnlineB: onlineB,
					State: LinkIdle.String(),
				}
				if record, exists := state.links[pair]; exists {
					view.State = record.State.String()
					view.Token = string(record.Token)
					view.UpdatedAt = record.UpdatedAt
					view.LastReason = string(record.LastReason)
				}
				links = append(links, view)
			}
		}
	})
	return devices, links, errors.Join(err, failure)
}

// linkDeviceNames 把配对行整理成日志可读的两端名称。
func linkDeviceNames(peering PeeringRow) [2]string {
	return [2]string{peering.NameA + " (" + string(peering.DeviceA) + ")", peering.NameB + " (" + string(peering.DeviceB) + ")"}
}
