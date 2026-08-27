package common

import (
	"encoding/binary"
	"testing"
)

// TestGTUNFrameRoundTripAndCopy 验证帧往返及解码载荷不引用输入缓冲区。
func TestGTUNFrameRoundTripAndCopy(t *testing.T) {
	t.Parallel()
	payload := testIPv4Packet(48, 5)
	encoded, err := EncodeGTUNFrame(testToken, ^uint32(0), payload, 1280)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeGTUNFrame(encoded, 1280)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Token != testToken || decoded.Sequence != ^uint32(0) || string(decoded.Payload) != string(payload) {
		t.Fatalf("decoded frame mismatch: %#v", decoded)
	}
	encoded[GTUNHeaderBytes] ^= 0xff
	if string(decoded.Payload) != string(payload) {
		t.Fatal("decoded payload aliases input datagram")
	}
}

// TestGTUNFrameMaximumPayload 验证最大合法 TUN MTU 仍可形成 UDP 载荷。
func TestGTUNFrameMaximumPayload(t *testing.T) {
	t.Parallel()
	payload := testIPv4Packet(MaxTUNMTU, 5)
	encoded, err := EncodeGTUNFrame(testToken, 1, payload, MaxTUNMTU)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 65507 {
		t.Fatalf("maximum UDP payload length = %d, want 65507", len(encoded))
	}
	if _, err := DecodeGTUNFrame(encoded, MaxTUNMTU); err != nil {
		t.Fatal(err)
	}
}

// TestGTUNRejectsFrameBoundaries 覆盖 magic、token、长度、MTU 和载荷结构错误。
func TestGTUNRejectsFrameBoundaries(t *testing.T) {
	t.Parallel()
	// 从合法帧逐字段变异，确保每个固定头和 IPv4 边界独立生效。
	payload := testIPv4Packet(40, 5)
	valid, err := EncodeGTUNFrame(testToken, 7, payload, 1280)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		mtu  int
	}{
		{"short header", valid[:15], 1280},
		{"bad magic", mutateCopy(valid, 0, 'X'), 1280},
		{"bad declared length", mutateCopy(valid, 15, valid[15]+1), 1280},
		{"payload over mtu", valid, 39},
		{"bad checksum", mutateCopy(valid, GTUNHeaderBytes+10, valid[GTUNHeaderBytes+10]^0xff), 1280},
		{"bad version", mutateCopy(valid, GTUNHeaderBytes, 0x65), 1280},
		{"bad ihl", mutateCopy(valid, GTUNHeaderBytes, 0x44), 1280},
		{"bad total length", mutateCopy(valid, GTUNHeaderBytes+3, 39), 1280},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeGTUNFrame(test.data, test.mtu); err == nil {
				t.Fatal("invalid frame accepted")
			}
		})
	}
	if _, err := EncodeGTUNFrame("INVALID", 0, payload, 1280); err == nil {
		t.Error("invalid token accepted")
	}
	if _, err := EncodeGTUNFrame(testToken, 0, payload, 19); err == nil {
		t.Error("invalid MTU accepted")
	}
	if _, err := EncodeGTUNFrame(testToken, 0, payload, 39); err == nil {
		t.Error("payload above MTU accepted")
	}
}

// TestValidateIPv4PacketOptionsAndFragments 验证 IPv4 options 可接受而分片包被拒绝。
func TestValidateIPv4PacketOptionsAndFragments(t *testing.T) {
	t.Parallel()
	packet := testIPv4Packet(64, 6)
	binary.BigEndian.PutUint16(packet[6:8], 0x2000)
	setIPv4Checksum(packet, 24)
	if err := ValidateIPv4Packet(packet); err != nil {
		t.Fatalf("valid IPv4 options/fragment packet rejected: %v", err)
	}
	for _, packet := range [][]byte{
		nil,
		make([]byte, 19),
		testIPv4Packet(20, 5)[:19],
	} {
		if err := ValidateIPv4Packet(packet); err == nil {
			t.Error("invalid packet accepted")
		}
	}
}

// testIPv4Packet 构造指定总长度和头长的合法最小 IPv4 测试包。
func testIPv4Packet(length, ihlWords int) []byte {
	packet := make([]byte, length)
	packet[0] = 0x40 | byte(ihlWords)
	binary.BigEndian.PutUint16(packet[2:4], uint16(length))
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], []byte{10, 0, 0, 1})
	copy(packet[16:20], []byte{10, 0, 0, 2})
	for index := ihlWords * 4; index < len(packet); index++ {
		packet[index] = byte(index)
	}
	setIPv4Checksum(packet, ihlWords*4)
	return packet
}

// setIPv4Checksum 重算测试包 IPv4 头校验和。
func setIPv4Checksum(packet []byte, headerLength int) {
	packet[10], packet[11] = 0, 0
	var sum uint32
	for index := 0; index < headerLength; index += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
}

// mutateCopy 复制测试数据并修改单字节，避免用例之间共享可变底层数组。
func mutateCopy(data []byte, index int, value byte) []byte {
	copyData := append([]byte(nil), data...)
	copyData[index] = value
	return copyData
}
