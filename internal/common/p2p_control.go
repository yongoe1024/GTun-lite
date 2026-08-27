package common

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxP2PControlDatagram 是 ASCII P2P 控制包固定的收发上限。最长合法
	// 报文是 4 字段的 ACK/OK（约 40 字节），64 是留有余量的整值上界，
	// 收发两侧共用。
	MaxP2PControlDatagram = 64

	P2PTypePunch    = "PUNCH"
	P2PTypePunchACK = "PUNCH_ACK"
	P2PTypePunchOK  = "PUNCH_OK"
	P2PTypePing     = "PING"
)

// P2PControl 表示一个已校验的 ASCII P2P 控制数据报。
type P2PControl struct {
	Type           string
	Token          LinkToken
	TargetSocketID SocketID
	SenderSocketID SocketID
	Sequence       uint32
}

// Validate 按控制类型严格校验字段集合。
func (control P2PControl) Validate() error {
	if !control.Token.Valid() {
		return errors.New("invalid P2P token")
	}
	switch control.Type {
	case P2PTypePunch:
		if !control.SenderSocketID.Valid() || control.TargetSocketID != "" || control.Sequence != 0 {
			return errors.New("invalid PUNCH fields")
		}
	case P2PTypePunchACK, P2PTypePunchOK:
		if !control.TargetSocketID.Valid() || !control.SenderSocketID.Valid() || control.Sequence != 0 {
			return fmt.Errorf("invalid %s fields", control.Type)
		}
	case P2PTypePing:
		if control.TargetSocketID != "" || control.SenderSocketID != "" {
			return errors.New("invalid PING fields")
		}
	default:
		return fmt.Errorf("unknown P2P control type %q", control.Type)
	}
	return nil
}

// MarshalP2PControl 使用单个 ASCII 空格序列化控制包。
func MarshalP2PControl(control P2PControl) ([]byte, error) {
	if err := control.Validate(); err != nil {
		return nil, err
	}
	var encoded string
	switch control.Type {
	case P2PTypePunch:
		encoded = fmt.Sprintf("%s %s %s", control.Type, control.Token, control.SenderSocketID)
	case P2PTypePunchACK, P2PTypePunchOK:
		encoded = fmt.Sprintf("%s %s %s %s", control.Type, control.Token, control.TargetSocketID, control.SenderSocketID)
	case P2PTypePing:
		encoded = fmt.Sprintf("%s %s %d", control.Type, control.Token, control.Sequence)
	}
	if len(encoded) > MaxP2PControlDatagram {
		return nil, errors.New("P2P control datagram exceeds 64 bytes")
	}
	return []byte(encoded), nil
}

// ParseP2PControl 解析一个完整数据报，并拒绝非规范分隔符。
func ParseP2PControl(datagram []byte) (P2PControl, error) {
	if len(datagram) == 0 || len(datagram) > MaxP2PControlDatagram {
		return P2PControl{}, errors.New("P2P control datagram length is outside 1..64")
	}
	// 控制报文禁止不可见字符，避免空白和行结束符产生多种等价编码。
	for _, value := range datagram {
		if value > 0x7f || value < 0x20 {
			return P2PControl{}, errors.New("P2P control datagram must contain printable ASCII")
		}
	}
	fields := strings.Split(string(datagram), " ")
	// Split 保留空字段，因此可明确拒绝连续空格和首尾空格。
	for _, field := range fields {
		if field == "" {
			return P2PControl{}, errors.New("P2P control fields require single spaces")
		}
	}
	control := P2PControl{Type: fields[0]}
	var err error
	// 先按类型固定字段数量，再按字段顺序恢复强类型身份。
	switch control.Type {
	case P2PTypePunch:
		if len(fields) != 3 {
			return P2PControl{}, errors.New("PUNCH requires exactly 3 fields")
		}
		control.Token, err = ParseLinkToken(fields[1])
		if err == nil {
			control.SenderSocketID, err = ParseSocketID(fields[2])
		}
	case P2PTypePunchACK, P2PTypePunchOK:
		if len(fields) != 4 {
			return P2PControl{}, fmt.Errorf("%s requires exactly 4 fields", control.Type)
		}
		control.Token, err = ParseLinkToken(fields[1])
		if err == nil {
			control.TargetSocketID, err = ParseSocketID(fields[2])
		}
		if err == nil {
			control.SenderSocketID, err = ParseSocketID(fields[3])
		}
	case P2PTypePing:
		if len(fields) != 3 {
			return P2PControl{}, errors.New("PING requires exactly 3 fields")
		}
		control.Token, err = ParseLinkToken(fields[1])
		if err == nil {
			if !decimalDigits(fields[2]) {
				err = errors.New("PING sequence must be unsigned decimal")
			} else {
				var sequence uint64
				sequence, err = strconv.ParseUint(fields[2], 10, 32)
				control.Sequence = uint32(sequence)
			}
		}
	default:
		return P2PControl{}, fmt.Errorf("unknown P2P control type %q", control.Type)
	}
	if err != nil {
		return P2PControl{}, err
	}
	// 强类型解析后再次执行跨字段约束，保持编码和解码使用同一规则。
	if err := control.Validate(); err != nil {
		return P2PControl{}, err
	}
	return control, nil
}

// decimalDigits 检查字段是否为非空 ASCII 十进制数字串。
func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
