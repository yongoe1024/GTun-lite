package client

import (
	"os"
	"testing"
)

// TestMain 把测试工作目录切到临时目录：观察日志（临时设施）路径写死为
// 相对路径，不重定向会在包目录里落下 gtun-scan.log。
func TestMain(m *testing.M) {
	previous, _ := os.Getwd()
	dir, err := os.MkdirTemp("", "gtun-client-test-*")
	if err != nil {
		os.Exit(m.Run()) // 建不出临时目录：照常跑，宁可多一个日志文件也不跳过测试
	}
	if os.Chdir(dir) != nil {
		_ = os.RemoveAll(dir)
		os.Exit(m.Run())
	}
	code := m.Run()
	_ = os.Chdir(previous)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
