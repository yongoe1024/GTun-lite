//go:build linux

package linux

import (
	"fmt"
	"os/exec"

	"gtun-lite/internal/common"
)

// configureInterface 用 ip 命令配置 /32 地址与 MTU。地址按 /32 安装：
// 若按网络前缀安装，内核会自动生成整网段的 connected route，
// 把同网段其他地址的流量都引进 TUN。
func configureInterface(name string, mtu int, localIP common.IPv4) error {
	if err := run("ip", "addr", "add", string(localIP)+"/32", "dev", name); err != nil {
		return fmt.Errorf("addr add: %w", err)
	}
	if err := run("ip", "link", "set", "dev", name, "mtu", fmt.Sprintf("%d", mtu), "up"); err != nil {
		return fmt.Errorf("link set: %w", err)
	}
	return nil
}

// addHostRoute 安装对端虚拟 IP 的 /32 主机路由，指向本接口。
func addHostRoute(name string, peer common.IPv4) error {
	if err := run("ip", "route", "add", string(peer)+"/32", "dev", name); err != nil {
		return err
	}
	return nil
}

// deleteHostRoute 删除 /32 主机路由（回滚）。
func deleteHostRoute(name string, peer common.IPv4) error {
	return run("ip", "route", "del", string(peer)+"/32", "dev", name)
}

// run 执行一条系统命令，失败时附上 stdout/stderr 原文（排障需要）。
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, string(output))
	}
	return nil
}
