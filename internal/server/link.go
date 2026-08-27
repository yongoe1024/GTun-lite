package server

import (
	"errors"
	"time"

	"gtun-lite/internal/common"
)

// LinkState 是链路状态，只有三态。
//
// 三态划分的依据是「服务器能区分的事实只有三种」：没在建链（IDLE）、
// 已下发意图但还没听到成功（CONNECTING）、听到过成功且此后没听到失败
// （CONNECTED）。探测与打洞是 CONNECTING 内部的阶段，服务器不需要区分——
// 它对这两个阶段的处置完全相同，拆成独立状态只会让转换表变大而没有新行为。
//
// 「双方都在线但管理员还没下发」不是一个状态，就是 IDLE：在线性是实时查
// session 表得到的，不需要在链路状态里冗余一份。
type LinkState int

const (
	// LinkIdle 未建链。Token 必为 nil。
	LinkIdle LinkState = iota
	// LinkConnecting 已下发 CONNECT，探测与打洞进行中。
	LinkConnecting
	// LinkConnected 任一侧上报握手成功。
	LinkConnected
)

// String 返回线上与日志使用的状态名。
func (state LinkState) String() string {
	switch state {
	case LinkIdle:
		return "IDLE"
	case LinkConnecting:
		return "CONNECTING"
	case LinkConnected:
		return "CONNECTED"
	default:
		return "UNKNOWN"
	}
}

// Link 是一条链路的内存状态。
//
// 三条不变量是整个服务端设计的地基。它们不是三个独立的检查，而是同一个
// 权责划分的三个推论：服务器管下发意图，客户端管隧道事实。改动任何一条前
// 请先想清楚它撑着什么——下方每条都写了「删掉它会需要补什么」。
//
//  1. TCP 断开不触发任何链路状态转换。隧道是 P2P 的、建立后不经过服务器，
//     控制面断连期间它很可能还在跑，服务器对此没有任何事实依据。
//     保持原状态不动，等客户端重连时问它。
//     若改成「断开即转某个状态」，就必须回答「重连后如何判断隧道是否还是
//     原来那条」，而这个问题只能靠保留意图 + 匹配 token + 等双方都回来
//     这类协商去猜，猜错的代价是拆掉一条好隧道或留下一条死记录。
//  2. CONNECT/DISCONNECT 要求双方 TCP 都在线，任一侧离线则拒绝下发
//     （ErrPeerOffline）且不改状态。这两个操作都必须双方同时执行才有意义，
//     单侧下发会产生「一侧在打洞、另一侧不知道」的分裂状态。
//     若允许单侧下发，就必须再写一套机制去发现并清理这类分裂状态，
//     而清理机制本身也会有它自己的边界情况。拒绝下发让分裂在结构上
//     不可能出现，代价是一侧掉线时管理操作会被拒——这个拒绝是诚实的，
//     操作本来就无法正确完成。
//  3. 任一侧报打洞失败或隧道断开，立即判失败，不等另一侧确认。
//     隧道是双向的，一侧不通就是不通。
//     若要求双方共同确认，则当一侧的 TCP 恰好也断了时，它的上报永远不会
//     到达，链路会卡在 CONNECTING 直到超时——于是又需要为这个超时写兜底
//     回收。悲观判定让失败立即收敛，不需要兜底。
//
// 链路状态是纯内存的，从不落库。服务器重启必然丢失全部链路状态，这是设计上
// 接受的行为，不要为它加持久化：重建方式就是客户端重连时全量上报、服务器
// 直接采信，比从库里读一份可能已经过期的记录更接近现实。
type Link struct {
	// State 是最后已知状态。客户端在线时它就是当前状态；
	// 离线时它是客户端最后一次亲口说的事实，配合 UpdatedAt 判断新鲜度。
	State LinkState
	// Token 是当前这次链路尝试的 token。
	//
	// 与 State 严格配对，两者是同一件事的两面：IDLE 必然空 token
	// （没在尝试就没有 token），CONNECTING 与 CONNECTED 必然非空。
	// 这个配对由 set 的调用点维持，Invariants 方法可用于测试断言。
	Token common.LinkToken
	// UpdatedAt 是 State 的采集时刻。管理页面在客户端离线时依赖它展示
	// "最后更新于何时"，因此每次 State 变更都必须刷新——漏刷会让页面把
	// 陈旧状态显示成新鲜的，比状态本身错更难排查。
	UpdatedAt time.Time
	// LastReason 是最近一次已结束尝试的失败原因，供管理页面展示。
	// 语义刻意宽松：失败上报时写入（客户端单方声明，不校验不仲裁），
	// IssueConnect 开新尝试时清空；其余路径一律不动。因此它是「上次失败
	// 原因」而非「当前失败原因」——CONNECTING/CONNECTED 时必然为空，
	// 重启丢失，QUERY 快照也不刷新它。
	LastReason common.Reason
}

// ErrPeerOffline 表示链路某一侧控制连接不在线。
// 此时禁止下发 CONNECT/DISCONNECT（不变量 2）。
var ErrPeerOffline = errors.New("both devices must be online to issue commands")

// set 是唯一的状态写入口。任何状态变更都必须经由此函数，不要直接赋值字段。
//
// 集中在一处有两个作用：保证 UpdatedAt 不会漏刷（漏刷会让管理页面把陈旧状态
// 显示成新鲜的，比状态本身错更难排查），以及维持 State 与 Token 的配对——
// 转到 IDLE 时无条件清空 token，不依赖每个调用点自己记得传空串。
func (link *Link) set(state LinkState, token common.LinkToken, now time.Time) {
	if state == LinkIdle {
		token = "" // IDLE 没有进行中的尝试，token 必须清掉
	}
	link.State = state
	link.Token = token
	link.UpdatedAt = now
}

// Invariants 检查 State 与 Token 的配对是否成立，不成立则返回错误。
// 供测试断言使用：任何操作序列执行完毕后它都必须返回 nil。
func (link *Link) Invariants() error {
	if (link.State == LinkIdle) != (link.Token == "") {
		return errors.New("link token must be empty exactly when state is IDLE")
	}
	return nil
}

// IssueConnect 下发建链意图（双方在线性由调用方 hub 保证——它本来就要
// 取出两侧会话来投递消息，检查与投递在同一条 owner 命令内，中间不存在
// 会话消失的窗口）。
//
// 没有单独的「重试」操作：重试就是再调用一次本函数，它会换一个新 token 重新
// 开始。任何状态下调用都合法——对已 CONNECTED 的链路调用意味着「重建这条链路」，
// 旧 token 随之作废，携带旧 token 的迟到消息会因 token 不匹配被丢弃。
func (link *Link) IssueConnect(token common.LinkToken, now time.Time) {
	link.set(LinkConnecting, token, now)
	link.LastReason = "" // 新尝试开始，上次失败原因随之作废
}

// IssueDisconnect 下发强制拆链，客户端收到后必须停 Worker 并拆路由。
// 在线性保证同 IssueConnect（不变量 2，检查在 hub）。
//
// 没有单独的「清理状态」操作：把链路清回 IDLE 就是拆链，两者是同一件事，
// 分成两个操作会让「清了但没拆」成为一种可能的中间状态。
//
// 任何状态下调用都合法（与 IssueConnect 对称）：链路状态是服务器内存里
// 的缓存，IDLE 只说明「最后一次听说没有链路」，不保证两端真的没有——
// 服务器重启后、客户端上报到达前，隧道可能还在跑。因此拆链的语义是
// 「确保没有」而非「关掉一个开着的」：对 IDLE 下发同样送达两端，常态下
// 客户端无可拆即空操作；状态失真的窗口期里它才是真正干活的动作。
// 内存无 token 时消息改带占位值，见 hub.issueDisconnect。
func (link *Link) IssueDisconnect(now time.Time) {
	link.set(LinkIdle, "", now)
}

// ReportSuccess 处理任一侧上报的握手成功，token 不匹配则忽略。
// 返回是否接受了这次上报，供调用方决定要不要记日志。
//
// 只有 CONNECTING 会前进到 CONNECTED：链路已回到 IDLE 说明期间发生过失败或
// 拆链，此时迟到的成功上报不得把它拉回 CONNECTED。
func (link *Link) ReportSuccess(token common.LinkToken, now time.Time) bool {
	if link.State != LinkConnecting || !link.matches(token) {
		return false
	}
	link.set(LinkConnected, link.Token, now)
	return true
}

// ReportFailure 处理任一侧上报的失败（打洞失败或隧道断开），
// token 不匹配则忽略。返回是否接受了这次上报。
//
// 悲观判定：只要 token 对得上，一侧报失败即判失败，不等另一侧确认（不变量 3）。
// reason 原样留存（LastReason）：它是客户端的陈述，服务器不校验也不仲裁。
func (link *Link) ReportFailure(token common.LinkToken, reason common.Reason, now time.Time) bool {
	if !link.matches(token) {
		return false
	}
	link.set(LinkIdle, "", now)
	link.LastReason = reason
	return true
}

// matches 判断上报携带的 token 是否属于当前这次尝试。
//
// 这个检查是不变量 3 成立的前提，不是可选的防御。链路尝试之间没有新旧序关系
// （token 是随机的，见 common.LinkToken），因此判断一条上报是否过期的唯一
// 依据就是 token 是否等于当前 token。少了它，一条针对上一次尝试的迟到失败
// 上报会把正在进行的新尝试打回 IDLE——而悲观判定又要求「见到失败立刻收敛」，
// 两者叠加会让重试在网络抖动时反复自杀。
//
// IDLE 时 Token 为空，任何非空 token 都不匹配：链路没在尝试，
// 就没有任何上报是「针对当前尝试」的。
func (link *Link) matches(token common.LinkToken) bool {
	return link.Token != "" && link.Token == token
}

// AdoptClientReport 采信客户端上报的链路状态，用于重连时的全量上报与
// QUERY 响应。
//
// 直接覆盖，不做任何一致性校验——不比对 token、不判断新旧、不与对端上报
// 交叉验证。这是权责划分的直接结果：客户端是隧道事实的唯一来源，服务器的
// 记录只是缓存，缓存与事实冲突时永远以事实为准。「校验」在这里没有意义，
// 因为没有第二个可信来源可供比对。
//
// 前提是客户端诚实上报。本系统定位于受信网络内的开发工具，这个前提成立；
// 若将来要面对不受信客户端，整套权责划分都需要重新评估，而不只是这个函数。
//
// now 必须用上报到达服务器的时刻，不能用客户端时钟：两端时钟未必同步，
// 而这个时间戳的语义是「服务器多久前听说过这件事」，服务器本地时钟才是
// 正确基准。
//
// 唯一的拒绝理由是上报本身自相矛盾：声称 CONNECTING/CONNECTED 却不带 token，
// 或声称 IDLE 却带着 token。这不是对客户端诚实性的怀疑（那是采信前提），
// 而是结构校验——这种上报无法存成一个满足 State/Token 配对的状态，
// 采信它会让内存状态自相矛盾。返回 false 时状态保持原样，调用方应记日志。
func (link *Link) AdoptClientReport(reported LinkState, token common.LinkToken, now time.Time) bool {
	if (reported == LinkIdle) != (token == "") {
		return false
	}
	link.set(reported, token, now)
	return true
}

// 管理页面展示链路时，用 State 与 UpdatedAt 两个字段，外加实时查 session 表
// 得到的在线性，三者分开呈现：「已离线 · CONNECTED · 最后更新 12 分钟前」。
//
// 离线时不要显示"未知"。状态是已知的——它是客户端最后一次亲口说的事实，
// 抹成"未知"丢掉了运维能用的信息。时间戳负责把"这是缓存"显式化：
// CONNECTED · 12 分钟前与 CONNECTED · 3 天前的运维含义完全不同。
