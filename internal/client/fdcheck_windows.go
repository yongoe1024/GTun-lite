//go:build windows

package client

// EnsureHelperFDHeadroom 在 Windows 上不设启动检查：Winsock 句柄上限
// 与 unix rlimit 模型不同，也无进程内自举可做；helper 创建失败会在
// 打洞阶段如实暴露，不做平台特有的前置防御。
func EnsureHelperFDHeadroom(count int) error { return nil }
