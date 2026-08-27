//go:build darwin || linux

package client

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fdReserve 是 helper 之外的描述符冗余量：控制连接、TUN、日志、
// runtime 内部及将来的数据面。刻意取得很大（1024）——启动时先把软上限
// 抬到能装下「档位 + 冗余」为止，抬完仍不够说明硬上限就不够，
// 这是部署层面的约束，拒绝启动并交给人决定。
const fdReserve = 1024

// fdCeiling 是自举抬升的目标封顶。部分平台（macOS）的硬上限是
// RLIM_INFINITY，把软上限直接设成无穷会触发拒绝；按有限大值夹住即可。
const fdCeiling = 1 << 20

// EnsureHelperFDHeadroom 是运行的第一个动作：确认 fd 预算装得下
// helper 档位。不够时先尝试把软上限抬高（进程内抬到硬上限不需要特权），
// 抬完仍不足则禁止启动。
//
// 这与被删除的「RLIMIT 探测自动选档」是两回事：这里只自举配额与体检，
// 档位本身永远按用户配置执行——不够就拒绝启动，绝不降档、绝不换档。
func EnsureHelperFDHeadroom(count int) error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("read fd limit: %w", err)
	}
	needed := uint64(count) + fdReserve
	if limit.Cur >= needed {
		return nil // 已足够，不动系统状态
	}
	target := limit.Max
	if target > fdCeiling {
		target = fdCeiling
	}
	if target < needed {
		return fmt.Errorf("CONFIG_INVALID: fd hard limit %d cannot fit helper_count %d plus %d reserved fds; raise the hard limit (launchctl on macOS, limits.conf on Linux) or choose a smaller tier", limit.Max, count, fdReserve)
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: target, Max: limit.Max}); err != nil {
		return fmt.Errorf("raise fd soft limit to %d: %w", target, err)
	}
	var verify unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &verify); err != nil || verify.Cur < needed {
		return fmt.Errorf("CONFIG_INVALID: fd limit could not be raised to fit helper_count %d plus %d reserved fds", count, fdReserve)
	}
	return nil
}
