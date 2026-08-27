//go:build windows

package win

import (
	"fmt"
	"os/exec"

	"gtun-lite/internal/common"
)

// configureInterface 用 netsh 配置 /32 地址与 MTU。地址按 /32 安装：
// 若按网络前缀安装，系统会把整个网段的流量都路由到本接口，覆盖既有路径。
func configureInterface(name string, mtu int, localIP common.IPv4) error {
	if err := run("netsh", "interface", "ipv4", "set", "address", name, "static", string(localIP), "255.255.255.255"); err != nil {
		return fmt.Errorf("netsh addr: %w", err)
	}
	if err := run("netsh", "interface", "ipv4", "set", "subinterface", name, "mtu=", fmt.Sprintf("%d", mtu)); err != nil {
		return fmt.Errorf("netsh mtu: %w", err)
	}
	return nil
}

// addHostRoute 用 route 命令安装对端虚拟 IP 的 /32 主机路由。
func addHostRoute(name string, peer common.IPv4) error {
	// route add <peer> mask 255.255.255.255 0.0.0.0 IF <index> 需接口索引；
	// 这里用 netsh 按接口名添加，避免依赖接口索引查找。
	return run("netsh", "interface", "ipv4", "add", "route", string(peer)+"/32", name)
}

// deleteHostRoute 删除 /32 主机路由（回滚）。
func deleteHostRoute(name string, peer common.IPv4) error {
	return run("netsh", "interface", "ipv4", "delete", "route", string(peer)+"/32", name)
}

// run 执行一条系统命令，失败时附上 stdout/stderr 原文（排障需要）。
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, string(output))
	}
	return nil
}
