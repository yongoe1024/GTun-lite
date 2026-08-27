package common

import "fmt"

// Reason 是机器可读的链路状态转换原因，也是客户端上报失败的分类。
type Reason string

// 全部 12 个原因。这张表是封闭的，ValidateReason 拒绝表外的任意文本——
// 原因值会进日志、进管理页面、进运维的判断，自由文本会让它退化成不可聚合的
// 字符串。新增原因必须同时改这里和 reasons 数组。
//
// 每个原因都对应一个客户端能观测到的具体事实。刻意不设「链路状态未知」
// 一类的原因：状态要么是客户端报的事实，要么是服务器下发的意图，没有第三种。
const (
	// ReasonProbeTimeout 探测超时。
	ReasonProbeTimeout Reason = "PROBE_TIMEOUT"
	// ReasonProbeIPChanged 同一 socket 向服务器五个探测端口发包，回显的公网 IP
	// 不全相同。部分家用宽带按流轮换出口 IP，此时端口画像失去意义（预测的候选
	// 端口属于另一个出口地址），必须拒绝而不是拿着不一致的样本去打洞。
	ReasonProbeIPChanged Reason = "PROBE_IP_CHANGED"
	// ReasonPunchTimeout 打洞在预算内未完成握手。
	ReasonPunchTimeout Reason = "PUNCH_TIMEOUT"
	// ReasonNATUnsupported 双方 NAT 都是 variable（端口不可预测）。
	// 直连需要至少一侧 stable，Range 邻域预测需要一侧 stable 作锚点，
	// 两条路径都不适用，因此在打洞前就判定失败而不是白跑一轮。
	ReasonNATUnsupported Reason = "NAT_UNSUPPORTED"
	// ReasonRouteConflict 路由预检发现冲突，不得发布配置。
	ReasonRouteConflict Reason = "ROUTE_CONFLICT"
	// ReasonTUNCreateFailed TUN 设备创建或配置失败。
	ReasonTUNCreateFailed Reason = "TUN_CREATE_FAILED"
	// ReasonConfigInvalid 配置非法，含 helper_count 不是允许的三档之一。
	// 只在启动校验阶段报出：配置错误要在启动时拒绝，不在运行期靠降级掩盖。
	ReasonConfigInvalid Reason = "CONFIG_INVALID"
	// ReasonTunnelLost 已建成的隧道保活失活。任一侧上报即判失败，
	// 不等另一侧确认（server 包不变量 3）。
	ReasonTunnelLost Reason = "TUNNEL_LOST"
	// ReasonPeerOffline 链路某一侧控制连接不在线，因此拒绝下发
	// CONNECT/DISCONNECT，链路状态保持原样（server 包不变量 2）。
	ReasonPeerOffline Reason = "PEER_OFFLINE"
	// ReasonHandshakeFailed 打洞握手失败。
	ReasonHandshakeFailed Reason = "HANDSHAKE_FAILED"
	// ReasonQueueFull 队列满。
	ReasonQueueFull Reason = "QUEUE_FULL"
	// ReasonInternalError 内部错误。
	ReasonInternalError Reason = "INTERNAL_ERROR"
)

// maxReasonBytes 限制线上 reason 字段长度。
const maxReasonBytes = 200

var reasons = [...]Reason{
	ReasonProbeTimeout,
	ReasonProbeIPChanged,
	ReasonPunchTimeout,
	ReasonNATUnsupported,
	ReasonRouteConflict,
	ReasonTUNCreateFailed,
	ReasonConfigInvalid,
	ReasonTunnelLost,
	ReasonPeerOffline,
	ReasonHandshakeFailed,
	ReasonQueueFull,
	ReasonInternalError,
}

var reasonSet = func() map[Reason]struct{} {
	set := make(map[Reason]struct{}, len(reasons))
	for _, reason := range reasons {
		set[reason] = struct{}{}
	}
	return set
}()

// AllReasons 返回完整原因表的副本。
func AllReasons() []Reason {
	result := make([]Reason, len(reasons))
	copy(result, reasons[:])
	return result
}

// ValidateReason 拒绝任意文本和原因表之外的值。
func ValidateReason(reason Reason) error {
	if len(reason) == 0 || len(reason) > maxReasonBytes {
		return fmt.Errorf("reason length must be between 1 and %d bytes", maxReasonBytes)
	}
	if _, ok := reasonSet[reason]; !ok {
		return fmt.Errorf("unknown reason %q", reason)
	}
	return nil
}

// 「控制连接断开」刻意不是一个 Reason：隧道是 P2P 的，控制面断开不影响它，
// 因此这件事只值一条日志，不构成链路失败原因、不进入状态判定
// （server 包不变量 1）。若把它加成 Reason，就等于承认服务器可以凭自己的
// 连接状态去改链路状态——那正是本设计要避免的。
