package common

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestLineReaderSkipsEmptyLines 验证读取器跳过协议允许忽略的纯空行。
func TestLineReaderSkipsEmptyLines(t *testing.T) {
	t.Parallel()
	reader, err := NewLineReader(strings.NewReader("\n\n{}\n"), 16)
	if err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "{}" {
		t.Fatalf("line = %q", line)
	}
	if _, err := reader.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, want EOF", err)
	}
}

// TestLineReaderDoesNotTreatWhitespaceAsEmpty 验证含空白字符的行不算空行。
// 分帧层不判断内容是否为合法 JSON，只负责切行——是否可解析交给上层。
func TestLineReaderDoesNotTreatWhitespaceAsEmpty(t *testing.T) {
	t.Parallel()
	reader, err := NewLineReader(strings.NewReader(" \n"), 16)
	if err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != " " {
		t.Fatalf("whitespace line = %q", line)
	}
}

// TestLineReaderBoundariesAndRecovery 验证长度边界及超长行丢弃后的下一行恢复。
func TestLineReaderBoundariesAndRecovery(t *testing.T) {
	t.Parallel()
	reader, err := NewLineReader(strings.NewReader("1234\n12345\nok\n"), 4)
	if err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadLine()
	if err != nil || string(line) != "1234" {
		t.Fatalf("exact boundary: line=%q err=%v", line, err)
	}
	if _, err := reader.ReadLine(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("oversize error = %v", err)
	}
	line, err = reader.ReadLine()
	if err != nil || string(line) != "ok" {
		t.Fatalf("reader did not recover: line=%q err=%v", line, err)
	}
}

// TestLineReaderRejectsUnterminatedAndInvalidConstruction 验证 EOF 半行和非法上限被拒绝。
func TestLineReaderRejectsUnterminatedAndInvalidConstruction(t *testing.T) {
	t.Parallel()
	if _, err := NewLineReader(nil, 1); err == nil {
		t.Error("nil reader accepted")
	}
	if _, err := NewLineReader(strings.NewReader(""), 0); err == nil {
		t.Error("zero maximum accepted")
	}
	reader, err := NewLineReader(strings.NewReader("{}"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadLine(); !errors.Is(err, ErrUnterminatedLine) {
		t.Fatalf("error = %v, want ErrUnterminatedLine", err)
	}
}

// 消息编码侧的边界用例（LF 是否计入大小上限）随消息封装层一起补，
// 当前本包只提供 LineReader 这一半分帧能力。
