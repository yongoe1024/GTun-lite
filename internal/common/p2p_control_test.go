package common

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestP2PControlRoundTrip 验证四类 ASCII P2P 控制报文的规范往返。
func TestP2PControlRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []P2PControl{
		{Type: P2PTypePunch, Token: testToken, SenderSocketID: testSocketA},
		{Type: P2PTypePunchACK, Token: testToken, TargetSocketID: testSocketA, SenderSocketID: testSocketB},
		{Type: P2PTypePunchOK, Token: testToken, TargetSocketID: testSocketB, SenderSocketID: testSocketA},
		{Type: P2PTypePing, Token: testToken, Sequence: 0},
		{Type: P2PTypePing, Token: testToken, Sequence: ^uint32(0)},
	}
	for _, expected := range tests {
		encoded, err := MarshalP2PControl(expected)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > MaxP2PControlDatagram {
			t.Fatalf("encoded datagram too large: %d", len(encoded))
		}
		actual, err := ParseP2PControl(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("round trip mismatch: got %#v want %#v", actual, expected)
		}
	}
}

// TestP2PControlRejectsInvalidDatagrams 覆盖长度、ASCII、分隔符和字段数量错误。
func TestP2PControlRejectsInvalidDatagrams(t *testing.T) {
	t.Parallel()
	invalid := [][]byte{
		{},
		bytes.Repeat([]byte{'A'}, MaxP2PControlDatagram+1),
		[]byte("PUNCH 666666666666 77777777\n"),
		[]byte("PUNCH\t666666666666 77777777"),
		[]byte("PUNCH  666666666666 77777777"),
		[]byte("PÜNCH 666666666666 77777777"),
		[]byte("UNKNOWN 666666666666 77777777"),
		[]byte("PUNCH 66666666666A 77777777"),
		[]byte("PUNCH 666666666666 7777777"),
		[]byte("PUNCH 666666666666 77777777 extra"),
		[]byte("PUNCH_ACK 666666666666 77777777"),
		[]byte("PING 666666666666 -1"),
		[]byte("PING 666666666666 +1"),
		[]byte("PING 666666666666 4294967296"),
	}
	for _, datagram := range invalid {
		if _, err := ParseP2PControl(datagram); err == nil {
			t.Errorf("invalid datagram accepted: %q", datagram)
		}
	}
}

// TestP2PControlMarshalRejectsCrossFieldValues 验证报文类型与字段集合必须匹配。
func TestP2PControlMarshalRejectsCrossFieldValues(t *testing.T) {
	t.Parallel()
	invalid := []P2PControl{
		{Type: P2PTypePunch, Token: testToken},
		{Type: P2PTypePunch, Token: testToken, SenderSocketID: testSocketA, TargetSocketID: testSocketB},
		{Type: P2PTypePunchACK, Token: testToken, TargetSocketID: testSocketA},
		{Type: P2PTypePing, Token: testToken, SenderSocketID: testSocketA},
		{Type: "UNKNOWN", Token: testToken},
	}
	for _, control := range invalid {
		if _, err := MarshalP2PControl(control); err == nil {
			t.Errorf("invalid control accepted: %#v", control)
		}
	}
	if _, err := ParseP2PControl([]byte(strings.Repeat("A", 65))); err == nil {
		t.Error("65-byte packet accepted")
	}
}
