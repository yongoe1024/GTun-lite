package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"unicode/utf8"
)

// 消息类型全表。下行只有三个操作，语义正交，没有一个是另一个的特例：
// CONNECT 下发建链意图，DISCONNECT 强制拆链，QUERY 拉取一个客户端的全部链路状态。
//
// 「重试」不是独立消息——重试就是再下发一次 CONNECT。「清理状态」也不是，
// 它就是 DISCONNECT。探测与打洞是 CONNECT 之后客户端内部的阶段，
// 不需要服务器分阶段下发。
//
// QUERY 与前两个的关键区别：它是只读的单向拉取，不要求双方在线——只问一个
// 客户端，对端离线不影响这次询问的正确性。只有会改变双端状态的
// CONNECT/DISCONNECT 才要求双方在线（见 server 包不变量 2）。
//
// 上行只有一个状态消息 STATE_REPORT，承载重连全量上报、QUERY 响应与
// 成功/失败的即时上报。合成一个是刻意的：三者内容相同，都是「我这边现在
// 是什么状态」，服务器的处置也相同（直接采信）。
const (
	// 上行（客户端 → 服务器）
	MessageDeviceRegister   = "device_register"
	MessageDeviceHeartbeat  = "device_heartbeat"
	MessageDeviceUnregister = "device_unregister"
	// MessageStateReport 是客户端上报链路事实的唯一消息。
	MessageStateReport = "state_report"
	// MessageProfileReport 上报本次尝试的 NAT 画像（探测五端口的观察结果）。
	MessageProfileReport = "profile_report"

	// 下行（服务器 → 客户端）
	MessageDeviceRegistered = "device_registered"
	MessageDuplicateLogin   = "duplicate_login"
	MessageNetworkConfig    = "network_config"
	MessageConnect          = "connect"
	MessageDisconnect       = "disconnect"
	// MessagePeerProfile 下发对端同次尝试的画像，双方画像齐了才各自开始打洞。
	MessagePeerProfile = "peer_profile"
	MessageQuery       = "query"
	MessageError       = "error"
)

// ErrUnknownMessageType 用于区分不支持的消息类型与畸形 JSON。
var ErrUnknownMessageType = errors.New("unknown message type")

// Message 是已校验的 TCP 控制协议消息。
type Message interface {
	MessageType() string
	Validate() error
}

// Endpoint 是规范 IPv4 地址与非零端口的组合。
type Endpoint struct {
	IP   IPv4 `json:"ip"`
	Port Port `json:"port"`
}

// Validate 检查 endpoint 的规范 IPv4 地址和非零端口。
func (endpoint Endpoint) Validate() error {
	if err := endpoint.IP.Validate(); err != nil {
		return err
	}
	if err := validateUnicastAddress(endpoint.IP); err != nil {
		return err
	}
	return endpoint.Port.Validate()
}

// NetworkPeer 是按设备下发的网络配置中的一个明确对端。
type NetworkPeer struct {
	DeviceID  DeviceID  `json:"device_id"`
	PeeringID PeeringID `json:"peering_id"`
	Name      string    `json:"name"`
	IP        IPv4      `json:"ip"`
	Online    bool      `json:"online"`
}

// NetworkDefinition 是 network_config 中的网络定义。
type NetworkDefinition struct {
	ID    NetworkID     `json:"id"`
	Name  string        `json:"name"`
	CIDR  IPv4CIDR      `json:"cidr"`
	IP    IPv4          `json:"ip"`
	Peers []NetworkPeer `json:"peers"`
}

// NetworkConfig 全量替换客户端当前的网络配置快照。
//
// # 不变量：配置推送始终全量
//
// 消息里刻意没有版本号 / 代次字段。这不是省了一个字段，而是一条必须维持的
// 不变量——三条依据支撑它：
//
//   - 同一连接内不会乱序。TCP 有序可靠，而服务端的配置组装与投递在同一个
//     owner goroutine 里串行完成，逻辑乱序的窗口不存在。
//   - 跨连接不需要判新旧。新会话建立后客户端无条件接受第一份全量配置，
//     它就是当下的真相。
//   - 全量应用幂等。同一份配置应用两次结果相同：拓扑未变时客户端直接
//     跳过重建（见 client.Manager 的 ApplyConfig），重复投递无副作用。
//
// 若将来为省流量改成增量推送，必须同时引入代次：增量必须知道「基于哪个版本」，
// 否则会产生「客户端应用了基于错误基线的增量」这类极难定位的缺陷。
// 先加代次，再改增量，顺序不能反。
type NetworkConfig struct {
	Type string `json:"type"`
	// Network 为 nil 表示该设备当前不属于任何网络。
	Network *NetworkDefinition `json:"network"`
}

// MessageType 返回网络配置消息的固定 wire 类型。
func (message NetworkConfig) MessageType() string { return MessageNetworkConfig }

// Validate 检查空配置或完整网络快照。
func (message NetworkConfig) Validate() error {
	if message.Type != MessageNetworkConfig {
		return errors.New("invalid network_config type")
	}
	if message.Network == nil {
		return nil
	}
	return message.Network.Validate()
}

// MarshalJSON 保证 peers 永远序列化为 [] 而非 null。
//
// Go 的 nil slice 会序列化成 null，而 "peers": null 与 "peers": [] 在语义上
// 都表示「没有对端」，却让接收方要处理两种形态。在编码侧统一成 [] ——
// 这比要求每个接收方都对 null 宽容更可靠，因为接收方可能不是本项目的代码。
//
// 「没有对端」是正常状态（设备刚入网、对端全被移除），不是错误，
// 因此不能让它在线上表现成一个特殊值。
func (network NetworkDefinition) MarshalJSON() ([]byte, error) {
	type definition NetworkDefinition // 借别名避免 MarshalJSON 递归
	value := definition(network)
	if value.Peers == nil {
		value.Peers = []NetworkPeer{}
	}
	return json.Marshal(value)
}

// Validate 检查网络定义中的 CIDR、地址和对端唯一性。
func (network NetworkDefinition) Validate() error {
	if !network.ID.Valid() {
		return errors.New("network contains invalid id")
	}
	if err := network.CIDR.Validate(); err != nil {
		return err
	}
	if err := network.IP.Validate(); err != nil {
		return err
	}
	prefix, _ := netip.ParsePrefix(string(network.CIDR))
	ip, _ := netip.ParseAddr(string(network.IP))
	if !usableHostAddress(prefix, ip) {
		return errors.New("local virtual IP is outside the network CIDR")
	}
	// 三个集合分别防止设备、配对和虚拟 IP 在同一快照内重复。
	seenDevices := make(map[DeviceID]struct{}, len(network.Peers))
	seenPeerings := make(map[PeeringID]struct{}, len(network.Peers))
	seenIPs := map[IPv4]struct{}{network.IP: {}}
	for _, peer := range network.Peers {
		if !peer.DeviceID.Valid() || !peer.PeeringID.Valid() {
			return errors.New("network peer contains an invalid ID")
		}
		if !utf8.ValidString(peer.Name) || utf8.RuneCountInString(peer.Name) < 1 || utf8.RuneCountInString(peer.Name) > 128 {
			return errors.New("peer name must contain 1 to 128 UTF-8 characters")
		}
		if err := peer.IP.Validate(); err != nil {
			return err
		}
		peerIP, _ := netip.ParseAddr(string(peer.IP))
		if !usableHostAddress(prefix, peerIP) {
			return errors.New("peer virtual IP is outside the network or duplicates the local IP")
		}
		if _, duplicate := seenDevices[peer.DeviceID]; duplicate {
			return errors.New("network contains duplicate peer device_id")
		}
		if _, duplicate := seenPeerings[peer.PeeringID]; duplicate {
			return errors.New("network contains duplicate peering_id")
		}
		if _, duplicate := seenIPs[peer.IP]; duplicate {
			return errors.New("network contains duplicate virtual IP")
		}
		seenDevices[peer.DeviceID] = struct{}{}
		seenPeerings[peer.PeeringID] = struct{}{}
		seenIPs[peer.IP] = struct{}{}
	}
	return nil
}

// usableHostAddress 排除前缀外、网络地址和广播地址。
func usableHostAddress(prefix netip.Prefix, address netip.Addr) bool {
	if !prefix.Contains(address) || address == prefix.Addr() {
		return false
	}
	return address != LastIPv4Address(prefix)
}

// validateUnicastAddress 要求地址是规范的可路由单播 IPv4。
func validateUnicastAddress(address IPv4) error {
	parsed, _ := netip.ParseAddr(string(address))
	if parsed.IsUnspecified() || parsed.IsMulticast() || address == "255.255.255.255" {
		return fmt.Errorf("IPv4 address %q is not valid unicast", address)
	}
	return nil
}

// MaxControlMessageBytes 是单条 TCP 控制消息（不含 LF）的全局上限，两端一致。
// 定死 64KB：最大的 network_config 也不会超过它的零头，这个上限存在的意义
// 是挡住错误对端与恶意连接的超长行，不是可调参数。放在 common 与
// ProbePortCount、MaxP2PControlDatagram 同源，两端不需要各自维护一份。
const MaxControlMessageBytes = 64 * 1024

// DeviceRegister 是一条连接的第一条消息，先于一切业务消息。
//
// 设备身份持久存在客户端的身份文件里，注册只是「拿着既有身份报到」——
// 服务器据此 upsert devices 行并开始推送配置。name 与 platform 随注册更新，
// 让管理页面显示的可读名称跟随客户端本机配置。
type DeviceRegister struct {
	Type     string   `json:"type"`
	DeviceID DeviceID `json:"device_id"`
	Name     string   `json:"name"`
	Platform string   `json:"platform"`
}

// MessageType 返回注册请求的固定 wire 类型。
func (message DeviceRegister) MessageType() string { return MessageDeviceRegister }

// Validate 检查身份、可读名与平台。
func (message DeviceRegister) Validate() error {
	if message.Type != MessageDeviceRegister {
		return errors.New("invalid device_register type")
	}
	if !message.DeviceID.Valid() {
		return errors.New("device_register contains an invalid device_id")
	}
	if !utf8.ValidString(message.Name) || utf8.RuneCountInString(message.Name) < 1 || utf8.RuneCountInString(message.Name) > 128 {
		return errors.New("device name must contain 1 to 128 UTF-8 characters")
	}
	switch message.Platform {
	case "linux", "darwin", "windows", "android":
	default:
		return errors.New("platform must be linux, darwin, windows or android")
	}
	return nil
}

// DeviceRegistered 确认注册成功，客户端见到它即完成握手。
type DeviceRegistered struct {
	Type string `json:"type"`
}

// MessageType 返回注册确认的固定 wire 类型。
func (message DeviceRegistered) MessageType() string { return MessageDeviceRegistered }

// Validate 检查类型字段。
func (message DeviceRegistered) Validate() error {
	if message.Type != MessageDeviceRegistered {
		return errors.New("invalid device_registered type")
	}
	return nil
}

// DeviceHeartbeat 只证明连接活着，不携带任何业务字段。
//
// 链路状态不搭心跳的车：状态变化有独立的 StateReport，重连时也有全量上报。
// 把状态塞进心跳会让「活着但没变化」与「死了」两种情况产生相同的线上静默，
// 排查时无法区分丢的是心跳还是状态。
type DeviceHeartbeat struct {
	Type string `json:"type"`
}

// MessageType 返回心跳的固定 wire 类型。
func (message DeviceHeartbeat) MessageType() string { return MessageDeviceHeartbeat }

// Validate 检查类型字段。
func (message DeviceHeartbeat) Validate() error {
	if message.Type != MessageDeviceHeartbeat {
		return errors.New("invalid device_heartbeat type")
	}
	return nil
}

// DeviceUnregister 是客户端的礼貌告别：进程将正常退出，主动让服务器立即
// 标记离线，而不是等心跳超时。收到后服务器关闭会话，链路状态照旧不动
// （不变量 1：隧道是 P2P 的，客户端退出不拆隧道）。
type DeviceUnregister struct {
	Type string `json:"type"`
}

// MessageType 返回注销消息的固定 wire 类型。
func (message DeviceUnregister) MessageType() string { return MessageDeviceUnregister }

// Validate 检查类型字段。
func (message DeviceUnregister) Validate() error {
	if message.Type != MessageDeviceUnregister {
		return errors.New("invalid device_unregister type")
	}
	return nil
}

// LinkStateOnWire 是链路状态在线上的三种取值，与 server.LinkState 一一对应。
// 状态枚举是两端协议的一部分，字符串形式让日志与抓包可直接读。
const (
	StateIdle       = "IDLE"
	StateConnecting = "CONNECTING"
	StateConnected  = "CONNECTED"
)

// LinkReport 是客户端对一条链路的事实陈述。
//
// 字段组合随 Full 取两种形态，校验规则也随形态切换（见 StateReport.Validate）：
//
//   - 快照条目（Full=true）：State 与 Token 的配对和服务器内存状态相同——
//     IDLE 必然空 token，CONNECTING / CONNECTED 必然携带 token。
//   - 转场条目（Full=false）：必然携带 token 标识「哪一次尝试」出了结果，
//     State 只取 CONNECTED（握手成功）或 IDLE（尝试失败）——「仍在打洞」
//     不是结果，服务器下发 CONNECT 后本来就假设 CONNECTING。
//     失败条目可带 Reason，供日志与管理页面归类，服务器不依据它做判定。
type LinkReport struct {
	PeeringID PeeringID `json:"peering_id"`
	State     string    `json:"state"`
	Token     LinkToken `json:"token"`
	// Reason 用空串表示「无」：它是封闭表（ValidateReason 拒绝空值与表外值），
	// 不需要指针来区分缺失与空——指针只留下 nil 解引用噪音。
	Reason Reason `json:"reason,omitempty"`
}

// Validate 做字段级检查；State 与 Token 的配对规则取决于所属消息的
// Full 标志，由 StateReport.Validate 按形态校验。
func (report LinkReport) Validate() error {
	if !report.PeeringID.Valid() {
		return errors.New("link report contains an invalid peering_id")
	}
	switch report.State {
	case StateIdle, StateConnecting, StateConnected:
	default:
		return errors.New("link report state must be IDLE, CONNECTING or CONNECTED")
	}
	if report.Token != "" && !report.Token.Valid() {
		return errors.New("link report contains an invalid token")
	}
	if report.Reason != "" {
		if err := ValidateReason(report.Reason); err != nil {
			return err
		}
	}
	return nil
}

// StateReport 是客户端上报链路事实的唯一消息，三种用途共用一个形状：
// 重连后的全量上报、QUERY 的响应、以及尝试进行中的即时单条上报。
//
// Full 区分前两种与第三种，服务器据此选择采信方式（见 server 包的处置）：
//
//   - Full=true 是快照。服务器对每条记录直接 AdoptClientReport 覆盖——
//     客户端是唯一事实来源，快照里说 IDLE 就是 IDLE，哪怕服务器自己
//     记的是 CONNECTED（那只是它重启前或断连前的旧缓存）。
//   - Full=false 是进行中尝试的单条转场。服务器走 token 守卫的
//     ReportSuccess/ReportFailure：token 不匹配说明这是上一次尝试的
//     迟到上报，忽略——链路尝试之间没有新旧序关系（token 随机），
//     没有别的依据能判定一条上报是否过期。
//
// 若不区分这两种语义，快照里的 IDLE 会被 token 守卫挡掉（快照的 IDLE
// 天然不带 token），断连期间死掉的隧道就永远无法把服务器的陈旧
// CONNECTED 纠正回来。
type StateReport struct {
	Type  string       `json:"type"`
	Full  bool         `json:"full"`
	Links []LinkReport `json:"links"`
}

// MessageType 返回状态上报的固定 wire 类型。
func (message StateReport) MessageType() string { return MessageStateReport }

// Validate 逐条检查上报内容，并按 Full 校验条目的字段组合。
// nil links 视为空——「没有任何链路事实」与「一条事实都没有」是同一件事，
// 不让接收方处理两种形态。
func (message StateReport) Validate() error {
	if message.Type != MessageStateReport {
		return errors.New("invalid state_report type")
	}
	seen := make(map[PeeringID]struct{}, len(message.Links))
	for _, report := range message.Links {
		if err := report.Validate(); err != nil {
			return err
		}
		switch {
		case message.Full:
			// 快照条目：IDLE ⇔ 空 token，与服务器内存状态的配对一致。
			if (report.State == StateIdle) != (report.Token == "") {
				return errors.New("snapshot entry token must be empty exactly when state is IDLE")
			}
		default:
			// 转场条目：token 必在（它是「哪次尝试」的唯一标识），
			// 状态只允许两种结果。
			if report.Token == "" {
				return errors.New("transition entry must carry the attempt token")
			}
			if report.State != StateIdle && report.State != StateConnected {
				return errors.New("transition entry state must be CONNECTED or IDLE")
			}
		}
		if _, duplicate := seen[report.PeeringID]; duplicate {
			return errors.New("state_report contains a duplicate peering_id")
		}
		seen[report.PeeringID] = struct{}{}
	}
	return nil
}

// MarshalJSON 保证 links 永远序列化为 [] 而非 null，理由与
// NetworkDefinition.MarshalJSON 相同：null 与 [] 语义相同却迫使接收方
// 处理两种形态，在编码侧统一比要求所有接收方宽容更可靠。
func (message StateReport) MarshalJSON() ([]byte, error) {
	type stateReport StateReport // 借别名避免 MarshalJSON 递归
	value := stateReport(message)
	if value.Links == nil {
		value.Links = []LinkReport{}
	}
	return json.Marshal(value)
}

// NAT 类型在线上的两种取值。
const (
	NATStable   = "stable"
	NATVariable = "variable"
)

// NATProfile 是一次探测的观察结果：同一 socket 向服务器 5 个连续 UDP 端口
// 各发一个 PROBE，服务器回显观察到的公网地址。
//
//   - PublicIP 一律取首个回显，五次回显是否一致不校验：家用宽带会按流
//     轮换出口 IP，取首个当固定值继续打洞，轮换仅日志留痕（不拒绝；
//     真机实证见设计文档 05）。
//   - NAT 分类：五个映射端口全部相同为 stable（端口可预测），
//     否则 variable。stable ⇔ 五端口全等是结构不变量，校验强制它。
//
// LocalIP/LocalPort 不上线：它们只供本机 helper 绑定同一本地地址，
// 对端与服务器都不需要。
type NATProfile struct {
	NAT      string `json:"nat"`
	PublicIP IPv4   `json:"public_ip"`
	Ports    []Port `json:"ports"`
}

// Validate 检查分类、公网地址与五端口结构不变量。
func (profile NATProfile) Validate() error {
	if profile.NAT != NATStable && profile.NAT != NATVariable {
		return errors.New("nat must be stable or variable")
	}
	if err := profile.PublicIP.Validate(); err != nil {
		return err
	}
	if err := validateUnicastAddress(profile.PublicIP); err != nil {
		return err
	}
	if len(profile.Ports) != ProbePortCount {
		return errors.New("nat profile must contain exactly 5 mapped ports")
	}
	stable := true
	for _, port := range profile.Ports {
		if err := port.Validate(); err != nil {
			return err
		}
		if port != profile.Ports[0] {
			stable = false
		}
	}
	if stable != (profile.NAT == NATStable) {
		return errors.New("nat classification must match port equality")
	}
	return nil
}

// MarshalJSON 保证 ports 永远是数组而非 null，理由同 NetworkDefinition。
func (profile NATProfile) MarshalJSON() ([]byte, error) {
	type natProfile NATProfile
	value := natProfile(profile)
	if value.Ports == nil {
		value.Ports = []Port{}
	}
	return json.Marshal(value)
}

// ProfileReport 上报本端在本次尝试里的 NAT 画像。带 token：画像属于
// 特定尝试，服务器只接受与链路当前 token 匹配的上报——迟到的旧尝试
// 画像不得为新尝试决定打洞路径。
type ProfileReport struct {
	Type      string     `json:"type"`
	Token     LinkToken  `json:"token"`
	PeeringID PeeringID  `json:"peering_id"`
	Profile   NATProfile `json:"profile"`
}

// MessageType 返回画像上报的固定 wire 类型。
func (message ProfileReport) MessageType() string { return MessageProfileReport }

// Validate 检查 token、配对与画像本身。
func (message ProfileReport) Validate() error {
	if message.Type != MessageProfileReport {
		return errors.New("invalid profile_report type")
	}
	if !message.Token.Valid() {
		return errors.New("profile_report contains an invalid token")
	}
	if !message.PeeringID.Valid() {
		return errors.New("profile_report contains an invalid peering_id")
	}
	return message.Profile.Validate()
}

// PeerProfile 下发对端同次尝试的画像。双方各自的 profile_report 都到达
// 服务器后，服务器把「对方的画像」发给每一侧，两侧据此选择打洞路径。
type PeerProfile struct {
	Type      string     `json:"type"`
	Token     LinkToken  `json:"token"`
	PeeringID PeeringID  `json:"peering_id"`
	Profile   NATProfile `json:"profile"`
}

// MessageType 返回对端画像的固定 wire 类型。
func (message PeerProfile) MessageType() string { return MessagePeerProfile }

// Validate 检查 token、配对与画像本身。
func (message PeerProfile) Validate() error {
	if message.Type != MessagePeerProfile {
		return errors.New("invalid peer_profile type")
	}
	if !message.Token.Valid() {
		return errors.New("peer_profile contains an invalid token")
	}
	if !message.PeeringID.Valid() {
		return errors.New("peer_profile contains an invalid peering_id")
	}
	return message.Profile.Validate()
}

// ConnectPeer 描述 CONNECT 下发中对端的信息，客户端用它记日志、
// 以及在数据面建立后向对端虚拟 IP 收发。
type ConnectPeer struct {
	DeviceID DeviceID `json:"device_id"`
	Name     string   `json:"name"`
	IP       IPv4     `json:"ip"`
}

// Connect 向一侧客户端下发建链意图，与发给对端的那条携带同一个 token。
//
// token 由服务器在 IssueConnect 时生成，两端据它认领同一次尝试：
// 打洞握手带它，GTUN 帧头也带它。Phase 3 的探测结果（对端公网端点）
// 落地后本消息会补充对端端点字段；两端版本不必严格同步，旧字段集
// 对新客户端仍然合法（未知字段宽容是双向的）。
type Connect struct {
	Type      string      `json:"type"`
	Token     LinkToken   `json:"token"`
	PeeringID PeeringID   `json:"peering_id"`
	Peer      ConnectPeer `json:"peer"`
}

// MessageType 返回建链消息的固定 wire 类型。
func (message Connect) MessageType() string { return MessageConnect }

// Validate 检查 token、配对身份与对端信息。
func (message Connect) Validate() error {
	if message.Type != MessageConnect {
		return errors.New("invalid connect type")
	}
	if !message.Token.Valid() {
		return errors.New("connect contains an invalid token")
	}
	if !message.PeeringID.Valid() {
		return errors.New("connect contains an invalid peering_id")
	}
	if !message.Peer.DeviceID.Valid() {
		return errors.New("connect peer contains an invalid device_id")
	}
	if !utf8.ValidString(message.Peer.Name) || utf8.RuneCountInString(message.Peer.Name) < 1 || utf8.RuneCountInString(message.Peer.Name) > 128 {
		return errors.New("connect peer name must contain 1 to 128 UTF-8 characters")
	}
	return message.Peer.IP.Validate()
}

// Disconnect 强制拆链：客户端收到后必须停止该配对当前的尝试并拆除路由。
// 客户端按 peering_id 停止尝试；token 仅供日志比对（服务器意图是权威的，
// 不因 token 不同而拒绝执行）。
type Disconnect struct {
	Type      string    `json:"type"`
	Token     LinkToken `json:"token"`
	PeeringID PeeringID `json:"peering_id"`
}

// MessageType 返回拆链消息的固定 wire 类型。
func (message Disconnect) MessageType() string { return MessageDisconnect }

// Validate 检查 token 与配对身份。
func (message Disconnect) Validate() error {
	if message.Type != MessageDisconnect {
		return errors.New("invalid disconnect type")
	}
	if !message.Token.Valid() {
		return errors.New("disconnect contains an invalid token")
	}
	if !message.PeeringID.Valid() {
		return errors.New("disconnect contains an invalid peering_id")
	}
	return nil
}

// Query 拉取单个客户端当前的全部链路状态，客户端以 Full=true 的
// StateReport 回应。只读且单向，不要求对端在线。
type Query struct {
	Type string `json:"type"`
}

// MessageType 返回查询消息的固定 wire 类型。
func (message Query) MessageType() string { return MessageQuery }

// Validate 检查类型字段。
func (message Query) Validate() error {
	if message.Type != MessageQuery {
		return errors.New("invalid query type")
	}
	return nil
}

// ErrorMessage 是服务器终止会话前的最后一条消息，说明终止原因。
// code 是封闭集合，客户端据此区分「重连有意义」（网络类）与
// 「重连没有意义」（身份类，如 duplicate_login 单独成消息）。
type ErrorMessage struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 错误码全表。新增必须同时改这里与 errorCodes。
const (
	ErrorInvalidMessage  = "invalid_message"
	ErrorUnknownType     = "unknown_type"
	ErrorRegisterTimeout = "register_timeout"
	// ErrorServerFull 是唯一一个「重试不会好转」的注册拒绝：
	// 在线会话数已达 max_connections，等别的设备下线才有意义。
	ErrorServerFull = "server_full"
	ErrorInternal   = "internal_error"
)

// MessageType 返回错误消息的固定 wire 类型。
func (message ErrorMessage) MessageType() string { return MessageError }

// Validate 检查类型、错误码与说明文本。
func (message ErrorMessage) Validate() error {
	if message.Type != MessageError {
		return errors.New("invalid error type")
	}
	switch message.Code {
	case ErrorInvalidMessage, ErrorUnknownType, ErrorRegisterTimeout, ErrorServerFull, ErrorInternal:
	default:
		return errors.New("error message contains an unknown code")
	}
	if message.Message == "" || len(message.Message) > 512 {
		return errors.New("error message text must contain 1 to 512 bytes")
	}
	return nil
}

// DuplicateLogin 由服务器发给被顶替的旧连接：同一设备已用新连接注册，
// 旧连接随即关闭。客户端收到它是进程级终态——继续重连只会与新实例
// 互相顶替，把问题暴露给运维优于静默震荡。
type DuplicateLogin struct {
	Type string `json:"type"`
}

// MessageType 返回顶替通知的固定 wire 类型。
func (message DuplicateLogin) MessageType() string { return MessageDuplicateLogin }

// Validate 检查类型字段。
func (message DuplicateLogin) Validate() error {
	if message.Type != MessageDuplicateLogin {
		return errors.New("invalid duplicate_login type")
	}
	return nil
}

// DecodeMessage 按消息头的 type 字段分发到具体结构并完整校验。
//
// 分发是封闭的：表外类型返回 ErrUnknownMessageType，调用方据此区分
// 「对端发了我们不认识的消息」（升级了版本或连错了服务）与
// 「消息畸形」（JSON 或校验失败），两者的处置不同。
func DecodeMessage(data []byte) (Message, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	var message Message
	switch header.Type {
	case MessageDeviceRegister:
		message = &DeviceRegister{}
	case MessageDeviceRegistered:
		message = &DeviceRegistered{}
	case MessageDeviceHeartbeat:
		message = &DeviceHeartbeat{}
	case MessageDeviceUnregister:
		message = &DeviceUnregister{}
	case MessageStateReport:
		message = &StateReport{}
	case MessageProfileReport:
		message = &ProfileReport{}
	case MessageNetworkConfig:
		message = &NetworkConfig{}
	case MessageConnect:
		message = &Connect{}
	case MessageDisconnect:
		message = &Disconnect{}
	case MessagePeerProfile:
		message = &PeerProfile{}
	case MessageQuery:
		message = &Query{}
	case MessageError:
		message = &ErrorMessage{}
	case MessageDuplicateLogin:
		message = &DuplicateLogin{}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownMessageType, header.Type)
	}
	if err := json.Unmarshal(data, message); err != nil {
		return nil, err
	}
	return message, message.Validate()
}
