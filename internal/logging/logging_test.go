package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSplitByLevel 普通日志进 file，warn/error 进 error_file。
func TestSplitByLevel(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gtun.log")
	errPath := filepath.Join(dir, "gtun.err")
	logger, closeLogs, err := New(Options{File: logPath, ErrorFile: errPath})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello", "k", "v")
	logger.Warn("warning")
	logger.Error("boom")
	closeLogs()

	logBody := readFile(t, logPath)
	errBody := readFile(t, errPath)
	if !strings.Contains(logBody, "hello") {
		t.Errorf("info stream missing info record: %q", logBody)
	}
	if strings.Contains(logBody, "boom") || strings.Contains(logBody, "warning") {
		t.Errorf("info stream must not contain warn/error: %q", logBody)
	}
	if !strings.Contains(errBody, "warning") || !strings.Contains(errBody, "boom") {
		t.Errorf("error stream missing warn/error records: %q", errBody)
	}
	if strings.Contains(errBody, "hello") {
		t.Errorf("error stream must not contain info: %q", errBody)
	}
}

// TestLevelFilter level=warn 时 info 被过滤，两个文件都不出现。
func TestLevelFilter(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gtun.log")
	errPath := filepath.Join(dir, "gtun.err")
	logger, closeLogs, err := New(Options{Level: "warn", File: logPath, ErrorFile: errPath})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Error("visible")
	closeLogs()
	if body := readFile(t, logPath); body != "" {
		t.Errorf("info must be filtered at warn level: %q", body)
	}
	if body := readFile(t, errPath); !strings.Contains(body, "visible") {
		t.Errorf("error record missing: %q", body)
	}
}

// TestAppendMode 落盘是追加写：重启不清掉历史日志。
func TestAppendMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gtun.log")
	if err := os.WriteFile(path, []byte("previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logger, closeLogs, err := New(Options{File: path})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("new-record")
	closeLogs()
	body := readFile(t, path)
	if !strings.Contains(body, "previous") || !strings.Contains(body, "new-record") {
		t.Errorf("append mode broken: %q", body)
	}
}

// TestOpenFailureFailFast 路径打不开立即报错（fail-fast，拒启动）。
func TestOpenFailureFailFast(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "x.log")
	if _, _, err := New(Options{File: bad}); err == nil {
		t.Fatal("expected open failure")
	}
}

// TestSingleFileFallback 只配 file 时错误流回落 stderr：构造成功即可
// （stderr 内容不做断言），错误日志仍可用。
func TestSingleFileFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gtun.log")
	logger, closeLogs, err := New(Options{File: path})
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("to-stderr")
	closeLogs()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
