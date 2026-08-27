package common

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// 截短标识符的线上长度（小写十六进制字符数），长度即字节数的两倍。
// 这些不是 UUID，是从 UUID 随机字节里截取的前缀——它们各有自己的长度理由，
// 其中 LinkToken 的 6 字节由 GTUN 帧头的固定字段宽度决定，不能改。
const (
	networkIDLength = 8  // 4 字节
	peeringIDLength = 32 // 16 字节
	linkTokenLength = 12 // 6 字节
	socketIDLength  = 8  // 4 字节
)

// DeviceID 是持久化的客户端身份，取 UUID 的规范文本形式
// （36 字符、小写、带连字符，如 "5f3e9c1a-7b24-4d80-9e11-2a6c8f0b3d45"）。
//
// 直接用 UUID 原本的样子，不再转成紧凑十六进制：它就是一个 UUID，
// 剥掉连字符只会多一层无谓转换，还得自己维护一套长度与字符集校验。
// 规范形式是唯一接受的形式（见 Valid），因为它是主键——同一身份的两种写法
// 会在 devices 表里存成两行。
type DeviceID string

// NetworkID 标识一个持久化虚拟网络。
type NetworkID string

// PeeringID 标识一对规范化设备配对，是持久化的配置身份
// （network_peerings 表的一列），跨越该对设备的任意多次建链。
// 「这一次建链尝试」由 LinkToken 标识，两者不要混用。
type PeeringID string

// LinkToken 将控制包和数据包绑定到同一次链路尝试：服务器下发 CONNECT 时生成，
// 打洞握手与 GTUN 帧头都带它，链路转 IDLE 时丢弃。
//
// token 是随机的、不可比较大小的——这是刻意的。链路尝试之间没有「新旧」序关系，
// 因此不存在「按编号拒绝迟到消息」这种判定；迟到消息靠 token 不匹配被丢弃。
type LinkToken string

// SocketID 标识一个客户端 Worker 内的 UDP socket。
type SocketID string

// randomHex 从一个 UUID v4 取前 byteLength 字节并十六进制编码，
// 供需要比 UUID 更短的标识符使用（截短的理由见上方常量注释）。
//
// 生成即返回，不查库检测碰撞：16 字节的 PeeringID 碰撞概率不在工程考虑范围内。
// 4 字节的 NetworkID（2^32）在网络数量很大时并非绝对安全，但它有主键约束
// 兜底——真撞上会在建网时报错，把问题暴露给用户，而不是静默重试掩盖过去。
func randomHex(byteLength int) string {
	value := uuid.New()
	return hex.EncodeToString(value[:byteLength])
}

// GenerateDeviceID 生成 DeviceID，即一个 UUID v4 的规范文本形式。
func GenerateDeviceID() DeviceID { return DeviceID(uuid.NewString()) }

// GenerateNetworkID 生成 NetworkID。
func GenerateNetworkID() NetworkID { return NetworkID(randomHex(4)) }

// GeneratePeeringID 生成 PeeringID。
func GeneratePeeringID() PeeringID { return PeeringID(randomHex(16)) }

// GenerateLinkToken 生成 LinkToken。
func GenerateLinkToken() LinkToken { return LinkToken(randomHex(6)) }

// GenerateSocketID 生成 SocketID。
func GenerateSocketID() SocketID { return SocketID(randomHex(4)) }

// ParseDeviceID 校验并转换线上的 DeviceID。
//
// 只接受规范文本形式。uuid.Parse 本身还接受带大括号、带 urn: 前缀、
// 无连字符等多种变体，因此解析成功后要求回写的文本与输入逐字相等——
// 主键必须只有一种写法。
func ParseDeviceID(value string) (DeviceID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", fmt.Errorf("device_id must be a canonical UUID: %w", err)
	}
	if parsed.String() != value {
		return "", errors.New("device_id must be a canonical lowercase hyphenated UUID")
	}
	return DeviceID(value), nil
}

// ParseLinkToken 校验并转换线上的 LinkToken。
func ParseLinkToken(value string) (LinkToken, error) {
	if err := validateHexID("token", value, linkTokenLength); err != nil {
		return "", err
	}
	return LinkToken(value), nil
}

// ParseSocketID 校验并转换线上的 SocketID。
func ParseSocketID(value string) (SocketID, error) {
	if err := validateHexID("socket_id", value, socketIDLength); err != nil {
		return "", err
	}
	return SocketID(value), nil
}

// Valid 返回 id 是否为规范形式的 UUID。
func (id DeviceID) Valid() bool {
	_, err := ParseDeviceID(string(id))
	return err == nil
}

// Valid 返回 id 是否为合法 NetworkID。
func (id NetworkID) Valid() bool {
	return validateHexID("network_id", string(id), networkIDLength) == nil
}

// Valid 返回 id 是否为合法 PeeringID。
func (id PeeringID) Valid() bool {
	return validateHexID("peering_id", string(id), peeringIDLength) == nil
}

// Valid 返回 token 是否为合法 LinkToken。
func (token LinkToken) Valid() bool {
	return validateHexID("token", string(token), linkTokenLength) == nil
}

// Valid 返回 id 是否为合法 SocketID。
func (id SocketID) Valid() bool {
	return validateHexID("socket_id", string(id), socketIDLength) == nil
}

// Link 按字典序保存一对设备。规范化排序让「同一对设备」在服务器与两端客户端
// 有唯一表示，链路状态才能按 Link 索引而不需要额外的方向字段。
type Link [2]DeviceID

// NewLink 将两个不同且合法的设备 ID 规范化排序。
func NewLink(a, b DeviceID) (Link, error) {
	if !a.Valid() || !b.Valid() {
		return Link{}, errors.New("link contains an invalid device_id")
	}
	if a == b {
		return Link{}, errors.New("link devices must differ")
	}
	if a < b {
		return Link{a, b}, nil
	}
	return Link{b, a}, nil
}

// Validate 拒绝畸形或未规范化的 Link。
func (link Link) Validate() error {
	if !link[0].Valid() || !link[1].Valid() {
		return errors.New("link contains an invalid device_id")
	}
	if link[0] >= link[1] {
		return errors.New("link must contain two distinct device IDs in lexicographic order")
	}
	return nil
}

// UnmarshalJSON 强制使用协议规定的双元素 Link 表示。
// 这是边界校验（线上数据进入内存），按 CLAUDE.md 第 2 条属于必要而非过度防御。
func (link *Link) UnmarshalJSON(data []byte) error {
	var values []DeviceID
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if len(values) != 2 {
		return fmt.Errorf("link must contain exactly 2 device IDs, got %d", len(values))
	}
	parsed := Link{values[0], values[1]}
	if err := parsed.Validate(); err != nil {
		return err
	}
	*link = parsed
	return nil
}

// validateHexID 统一检查固定长度的小写十六进制 ID 表示。
func validateHexID(name, value string, length int) error {
	if len(value) != length {
		return fmt.Errorf("%s must contain exactly %d lowercase hexadecimal characters", name, length)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%s must contain only lowercase hexadecimal characters", name)
		}
	}
	return nil
}
