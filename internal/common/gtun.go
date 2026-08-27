package common

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	// GTUNHeaderBytes 是内部 IPv4 包之前的固定二进制头长度。
	GTUNHeaderBytes = 16
	// MaxTUNMTU 保证完整 GTUN 帧不超过 UDP 最大载荷。
	MaxTUNMTU = 65491
)

var gtunMagic = [4]byte{'G', 'T', 'U', 'N'}

// GTUNFrame 是解码后的隧道帧；Payload 独立持有数据包字节副本。
type GTUNFrame struct {
	Token LinkToken
	// Sequence 是发送方单调递增的帧序号，仅供抓包对账（判断丢包与乱序）。
	// 接收端刻意不校验、不去重：隧道承载的内层流量自带可靠性（TCP 重传
	// 兜住丢包），在 UDP 上再做一层序号判定是重复付费。
	Sequence uint32
	Payload  []byte
}

// EncodeGTUNFrame 序列化一个内部 IPv4 包。
//
// 刻意不校验 payload 的 IPv4 结构与头校验和。出站包由本机内核经 TUN 交出，
// 结构必然合法；而数据面在入队前已经验过一次（见 tun.DataPlane 的 TUN 读循环），
// 这里再验一次是每包重复计算一遍校验和，纯属热路径浪费。
// 入站方向不同：DecodeGTUNFrame 必须校验，那里的数据来自网络，会有传输误码。
func EncodeGTUNFrame(token LinkToken, sequence uint32, payload []byte, tunMTU int) ([]byte, error) {
	if !token.Valid() {
		return nil, errors.New("invalid GTUN token")
	}
	if err := validateTUNMTU(tunMTU); err != nil {
		return nil, err
	}
	if len(payload) > tunMTU {
		return nil, errors.New("GTUN payload exceeds configured TUN MTU")
	}
	tokenBytes, err := hex.DecodeString(string(token))
	if err != nil || len(tokenBytes) != 6 {
		return nil, errors.New("invalid GTUN token encoding")
	}
	frame := make([]byte, GTUNHeaderBytes+len(payload))
	copy(frame[0:4], gtunMagic[:])
	copy(frame[4:10], tokenBytes)
	binary.BigEndian.PutUint32(frame[10:14], sequence)
	binary.BigEndian.PutUint16(frame[14:16], uint16(len(payload)))
	copy(frame[16:], payload)
	return frame, nil
}

// DecodeGTUNFrame 校验一个完整数据报，并复制其中的内部数据包。
func DecodeGTUNFrame(datagram []byte, tunMTU int) (GTUNFrame, error) {
	if err := validateTUNMTU(tunMTU); err != nil {
		return GTUNFrame{}, err
	}
	// 固定头字段必须全部验证完，才能按声明长度切出 payload。
	if len(datagram) < GTUNHeaderBytes {
		return GTUNFrame{}, errors.New("GTUN datagram is shorter than the fixed header")
	}
	if string(datagram[:4]) != string(gtunMagic[:]) {
		return GTUNFrame{}, errors.New("invalid GTUN magic")
	}
	payloadLength := int(binary.BigEndian.Uint16(datagram[14:16]))
	if payloadLength != len(datagram)-GTUNHeaderBytes {
		return GTUNFrame{}, errors.New("GTUN payload length does not match datagram length")
	}
	if payloadLength > tunMTU {
		return GTUNFrame{}, errors.New("GTUN payload exceeds configured TUN MTU")
	}
	// 返回值复制 UDP 缓冲区，避免调用方复用 datagram 后篡改已解码帧。
	payload := append([]byte(nil), datagram[GTUNHeaderBytes:]...)
	if err := ValidateIPv4Packet(payload); err != nil {
		return GTUNFrame{}, err
	}
	token := LinkToken(hex.EncodeToString(datagram[4:10]))
	if !token.Valid() {
		return GTUNFrame{}, errors.New("invalid GTUN token bytes")
	}
	return GTUNFrame{
		Token:    token,
		Sequence: binary.BigEndian.Uint32(datagram[10:14]),
		Payload:  payload,
	}, nil
}

// ValidateIPv4Packet 检查 GTUN 接收路径要求的纯 IPv4 结构。
func ValidateIPv4Packet(packet []byte) error {
	if len(packet) < 20 {
		return errors.New("IPv4 packet is shorter than 20 bytes")
	}
	if packet[0]>>4 != 4 {
		return errors.New("inner packet is not IPv4")
	}
	headerLength := int(packet[0]&0x0f) * 4
	if headerLength < 20 || headerLength > 60 || headerLength > len(packet) {
		return errors.New("invalid IPv4 IHL")
	}
	totalLength := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLength != len(packet) || totalLength < headerLength {
		return errors.New("IPv4 total length does not match payload length")
	}
	if !validIPv4Checksum(packet[:headerLength]) {
		return errors.New("invalid IPv4 header checksum")
	}
	return nil
}

// validateTUNMTU 拒绝无法形成合法 IPv4 包或会使 UDP 载荷溢出的 MTU。
func validateTUNMTU(tunMTU int) error {
	if tunMTU < 20 || tunMTU > MaxTUNMTU {
		return fmt.Errorf("TUN MTU must be between 20 and %d", MaxTUNMTU)
	}
	return nil
}

// validIPv4Checksum 对完整 IPv4 头执行一补和校验。
func validIPv4Checksum(header []byte) bool {
	var sum uint32
	for index := 0; index < len(header); index += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(sum) == 0xffff
}
