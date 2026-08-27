// Command client 启动 GTun-Lite 客户端进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"gtun-lite/internal/client"
	"gtun-lite/internal/tun"
)

// main 只负责退出码，生命周期全部在 run 里。
//
// 这个拆分是必需的，不是风格选择：os.Exit 不执行 defer，若在 main 里直接
// os.Exit(1) 退出，TUN 设备与路由的显式清理不会执行。优雅停止的正确性
// 必须由结构保证（defer 走到），不依赖「进程退出时内核替我们回收」——
// 后者在当前 macOS 实测成立（接口随进程消亡，2026-08-27 复验），但属于
// 平台行为的偶然而非承诺；darwin 显式地址清理另有理由，见 tun/mac 的
// RouteCleanup 注释。
func main() {
	os.Exit(run())
}

// run 装配并运行客户端，返回进程退出码。
func run() int {
	configPath := flag.String("config", "client.yaml", "path to client configuration")
	flag.Parse()

	config, err := client.LoadClientConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gtun-lite client: %v\n", err)
		return 1
	}
	log := newLogger(config.Logging.Level)

	// 运行的第一个动作：fd 预算自举——不够先抬高软上限（无特权要求），
	// 抬完仍装不下「helper 档位 + 冗余」则禁止启动。把决定权留给运维：
	// 调大硬上限，或换个小的档位；程序自己绝不降档。
	if err := client.EnsureHelperFDHeadroom(config.Punch.HelperCount); err != nil {
		log.Error("helper fd headroom", "error", err)
		return 1
	}

	identity, err := client.LoadIdentity(config.Identity.Path)
	if err != nil {
		log.Error("load identity", "error", err)
		return 1
	}

	manager := client.NewManager(config, identity, client.PlatformOpener(), tun.NewSystemRouteTable(), log)
	// 优雅停止：manager.Close 停 Worker、拆数据面与 TUN/路由。
	// 退出路径必须走到这里（os.Exit 在 main 里，defer 得以执行）。
	defer manager.Close()
	control := client.NewControlClient(config, manager, identity, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("gtun-lite client started", "device_id", string(identity), "server", config.Server.Addr)
	if err := control.Run(ctx); err != nil {
		// 顶替是进程级终态：同一身份的另一实例在线上，退出把冲突留给运维。
		log.Error("control client terminated", "error", err)
		return 1
	}
	log.Info("gtun-lite client stopped")
	return 0
}

// newLogger 按配置级别构造结构化日志。
func newLogger(level string) *slog.Logger {
	var value slog.Level
	switch level {
	case "debug":
		value = slog.LevelDebug
	case "warn":
		value = slog.LevelWarn
	case "error":
		value = slog.LevelError
	default:
		value = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: value}))
}
