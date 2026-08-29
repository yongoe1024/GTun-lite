package notice

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintfFormat 验证行格式「HH:MM:SS 内容」，并确认 nil 接收者安全。
func TestPrintfFormat(t *testing.T) {
	var buffer bytes.Buffer
	window := New(&buffer)
	window.Printf("已连接端口服务器 %s", "127.0.0.1:10000")

	line := buffer.String()
	if !strings.HasSuffix(line, " 已连接端口服务器 127.0.0.1:10000\n") {
		t.Fatalf("unexpected line: %q", line)
	}
	if stamp := line[:8]; len(strings.Split(stamp, ":")) != 3 {
		t.Fatalf("timestamp must be HH:MM:SS, got %q", stamp)
	}

	var nilNotice *Notice
	nilNotice.Printf("must not panic")
}

// TestNATText 验证 NAT 中文映射，未知取值原样透传。
func TestNATText(t *testing.T) {
	cases := map[string]string{
		"stable":   "稳定型",
		"variable": "变化型",
		"weird":    "weird",
	}
	for input, want := range cases {
		if got := NAT(input); got != want {
			t.Errorf("NAT(%q) = %q, want %q", input, got, want)
		}
	}
}
