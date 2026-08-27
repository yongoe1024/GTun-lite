package common

import (
	"strings"
	"testing"
)

// TestIDParsersAcceptCanonical 各类 wire ID 的合法解析路径。
func TestIDParsersAcceptCanonical(t *testing.T) {
	t.Parallel()
	if got, err := ParseDeviceID(string(testDeviceA)); err != nil || got != testDeviceA {
		t.Fatalf("ParseDeviceID = %q, %v", got, err)
	}
	if got, err := ParseLinkToken(string(testToken)); err != nil || got != testToken {
		t.Fatalf("ParseLinkToken = %q, %v", got, err)
	}
	if got, err := ParseSocketID(string(testSocketA)); err != nil || got != testSocketA {
		t.Fatalf("ParseSocketID = %q, %v", got, err)
	}
}

// TestParseDeviceIDRejectsNonCanonical DeviceID 只接受 UUID 的规范文本形式。
//
// 重点是那几种 uuid.Parse 本身会接受、但作为主键必须拒绝的变体：无连字符、
// 带大括号、带 urn: 前缀、大写。它们都指向同一个 UUID，若都放进来，
// 同一设备会在 devices 表里存成多行。
func TestParseDeviceIDRejectsNonCanonical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"compact hex without hyphens", "11111111111111114111811111111111"},
		{"braced", "{11111111-1111-4111-8111-111111111111}"},
		{"urn prefixed", "urn:uuid:11111111-1111-4111-8111-111111111111"},
		{"uppercase", "11111111-1111-4111-8111-11111111111A"},
		{"truncated", "11111111-1111-4111-8111"},
		{"non-hex character", "1111111g-1111-4111-8111-111111111111"},
		{"hyphens in wrong places", "111111111-111-4111-8111-111111111111"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseDeviceID(c.value); err == nil {
				t.Fatalf("ParseDeviceID(%q) accepted a non-canonical value", c.value)
			}
			if DeviceID(c.value).Valid() {
				t.Fatalf("DeviceID(%q).Valid() = true, want false", c.value)
			}
		})
	}
}

// TestParseTruncatedIDsRejectMalformed 截短标识符仍按固定长度的小写十六进制校验。
func TestParseTruncatedIDsRejectMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
	}{
		{"too short", strings.Repeat("1", linkTokenLength-1)},
		{"too long", strings.Repeat("1", linkTokenLength+1)},
		{"uppercase hex", strings.Repeat("A", linkTokenLength)},
		{"non-hex", strings.Repeat("g", linkTokenLength)},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseLinkToken(c.value); err == nil {
				t.Fatalf("ParseLinkToken(%q) accepted a malformed value", c.value)
			}
		})
	}
}

// TestGeneratedIDsAreValidAndDistinct 生成的 ID 必须自洽（能通过自己的 Valid）
// 且互不相同——长度写错会让生成值无法被解析，这类错误只有生成后立即校验才能发现。
func TestGeneratedIDsAreValidAndDistinct(t *testing.T) {
	t.Parallel()
	// 每类各生成两个：既验合法性，也验随机性没有退化成常量。
	if a, b := GenerateDeviceID(), GenerateDeviceID(); !a.Valid() || !b.Valid() || a == b {
		t.Fatalf("device IDs invalid or identical: %q %q", a, b)
	}
	if a, b := GenerateNetworkID(), GenerateNetworkID(); !a.Valid() || !b.Valid() || a == b {
		t.Fatalf("network IDs invalid or identical: %q %q", a, b)
	}
	if a, b := GeneratePeeringID(), GeneratePeeringID(); !a.Valid() || !b.Valid() || a == b {
		t.Fatalf("peering IDs invalid or identical: %q %q", a, b)
	}
	if a, b := GenerateLinkToken(), GenerateLinkToken(); !a.Valid() || !b.Valid() || a == b {
		t.Fatalf("link tokens invalid or identical: %q %q", a, b)
	}
	if a, b := GenerateSocketID(), GenerateSocketID(); !a.Valid() || !b.Valid() || a == b {
		t.Fatalf("socket IDs invalid or identical: %q %q", a, b)
	}
}

// TestNewLinkNormalizesOrder Link 必须按字典序规范化：同一对设备只有一种表示，
// 否则链路状态会按两个不同的键各存一份。
func TestNewLinkNormalizesOrder(t *testing.T) {
	t.Parallel()
	forward, err := NewLink(testDeviceA, testDeviceB)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := NewLink(testDeviceB, testDeviceA)
	if err != nil {
		t.Fatal(err)
	}
	if forward != reverse {
		t.Fatalf("link order not normalized: %v vs %v", forward, reverse)
	}
	if forward[0] >= forward[1] {
		t.Fatalf("link is not in lexicographic order: %v", forward)
	}
	if err := forward.Validate(); err != nil {
		t.Fatalf("normalized link rejected: %v", err)
	}
}

// TestNewLinkRejectsInvalidPairs 同一设备与自己配对、含非法 ID 的配对都必须拒绝。
func TestNewLinkRejectsInvalidPairs(t *testing.T) {
	t.Parallel()
	if _, err := NewLink(testDeviceA, testDeviceA); err == nil {
		t.Fatal("a device paired with itself was accepted")
	}
	if _, err := NewLink(testDeviceA, "short"); err == nil {
		t.Fatal("an invalid device ID was accepted")
	}
}

// TestLinkValidateRejectsUnnormalized 未规范化的 Link 即使两个 ID 都合法也必须拒绝，
// 否则线上消息可以绕过 NewLink 送进一个反序的键。
func TestLinkValidateRejectsUnnormalized(t *testing.T) {
	t.Parallel()
	reversed := Link{testDeviceB, testDeviceA}
	if err := reversed.Validate(); err == nil {
		t.Fatal("a reversed link passed validation")
	}
	duplicate := Link{testDeviceA, testDeviceA}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("a self-paired link passed validation")
	}
}

// TestLinkUnmarshalJSON 线上 Link 必须是恰好两元素、已规范化的数组。
func TestLinkUnmarshalJSON(t *testing.T) {
	t.Parallel()
	var link Link
	valid := `["` + string(testDeviceA) + `","` + string(testDeviceB) + `"]`
	if err := link.UnmarshalJSON([]byte(valid)); err != nil {
		t.Fatalf("valid link rejected: %v", err)
	}
	if link[0] != testDeviceA || link[1] != testDeviceB {
		t.Fatalf("decoded link = %v", link)
	}
	rejects := []string{
		`["` + string(testDeviceA) + `"]`, // 只有一个元素
		`["` + string(testDeviceA) + `","` + string(testDeviceB) + `","` + string(testDeviceA) + `"]`, // 三个
		`["` + string(testDeviceB) + `","` + string(testDeviceA) + `"]`,                               // 反序
		`"not-an-array"`,
	}
	for _, value := range rejects {
		var target Link
		if err := target.UnmarshalJSON([]byte(value)); err == nil {
			t.Fatalf("malformed link accepted: %s", value)
		}
	}
}
