package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
)

// IPv4 是规范形式的单播 IPv4 文本地址。
type IPv4 string

// IPv4CIDR 是规范形式的 RFC1918 IPv4 网络前缀。
type IPv4CIDR string

// Port 是非零 UDP 或 TCP 端口。
type Port uint16

// Validate 检查地址是否使用规范 IPv4 文本形式。
func (address IPv4) Validate() error {
	parsed, err := netip.ParseAddr(string(address))
	if err != nil || !parsed.Is4() || parsed.String() != string(address) {
		return fmt.Errorf("invalid canonical IPv4 address %q", address)
	}
	return nil
}

// Validate 检查前缀是否为规范的 RFC1918 网络地址。
func (prefix IPv4CIDR) Validate() error {
	parsed, err := netip.ParsePrefix(string(prefix))
	if err != nil || !parsed.Addr().Is4() || parsed.String() != string(prefix) || parsed != parsed.Masked() {
		return fmt.Errorf("invalid canonical IPv4 network prefix %q", prefix)
	}
	if !parsed.Addr().IsPrivate() || !LastIPv4Address(parsed).IsPrivate() {
		return fmt.Errorf("IPv4 network prefix %q is not RFC1918", prefix)
	}
	return nil
}

// LastIPv4Address 计算前缀内最后一个地址（广播地址）。CIDR 校验用它排除
// 广播地址；管理端的地址分配也用它，两处共用一份实现。
func LastIPv4Address(prefix netip.Prefix) netip.Addr {
	address := prefix.Addr().As4()
	value := uint32(address[0])<<24 | uint32(address[1])<<16 | uint32(address[2])<<8 | uint32(address[3])
	hostBits := 32 - prefix.Bits()
	value |= uint32((uint64(1) << hostBits) - 1)
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

// Validate 拒绝零端口。
func (port Port) Validate() error {
	if port == 0 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

// UnmarshalJSON 拒绝零值、负数、小数、字符串和溢出的端口值。
func (port *Port) UnmarshalJSON(data []byte) error {
	var value uint16
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed := Port(value)
	if err := parsed.Validate(); err != nil {
		return err
	}
	*port = parsed
	return nil
}
