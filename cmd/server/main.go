// Command server 启动 GTun-Lite 服务端进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"gtun-lite/internal/common"
	"gtun-lite/internal/logging"
	"gtun-lite/internal/server"
)

// main 只负责退出码，生命周期全部在 run 里：
// os.Exit 不执行 defer，清理逻辑必须在一个能正常返回的函数里。
func main() {
	os.Exit(run())
}

// run 装配并运行服务端，返回进程退出码。
func run() int {
	configPath := flag.String("config", "server.yaml", "path to server configuration")
	flag.Parse()

	config, err := server.LoadServerConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gtun-lite server: %v\n", err)
		return 1
	}
	log, closeLogs, err := logging.New(logging.Options{
		Level:     config.Logging.Level,
		File:      config.Logging.File,
		ErrorFile: config.Logging.ErrorFile,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gtun-lite server: %v\n", err)
		return 1
	}
	defer closeLogs()

	store, err := server.OpenStore(context.Background(), config.Database.Path)
	if err != nil {
		log.Error("open store", "error", err)
		return 1
	}
	// defer 链按装配逆序拆卸：hub（会话与链路状态）→ 控制/管理监听 → 库。
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("close store", "error", err)
		}
	}()

	owner := server.NewHub(store, config, log)
	defer owner.Close()

	control := server.NewControlServer(owner, config, log)
	if err := control.Listen(); err != nil {
		log.Error("listen control", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 探测反射器：5 个 UDP 端口，无状态回显。绑定或运行失败都让进程退出：
	// 端口被占用属于部署错误，带残缺探测面继续跑会让客户端画像永远缺样本
	// 且毫无告警——启动即失败优于静默残缺。
	reflector, err := server.NewProbeReflector(config.Probe.Bind, config.Probe.BasePort, log)
	if err != nil {
		log.Error("create probe reflector", "error", err)
		return 1
	}
	reflectorErr := make(chan error, 1)
	go func() { reflectorErr <- reflector.ListenAndServe(ctx) }()
	defer reflector.Close()

	admin := server.NewAdminAPI(owner, store, config, log)
	// 管理面同步绑定：端口被占属于部署错误，与控制面、探测面同等对待，
	// 启动即失败优于带着没有管理面的进程继续跑。
	adminListener, err := net.Listen("tcp", config.Admin.Bind)
	if err != nil {
		log.Error("listen admin", "error", err)
		return 1
	}
	adminServer := &http.Server{Handler: admin.Routes()}

	// 控制面与管理面的运行期故障同样终止进程：残缺控制面比没有更糟
	//（客户端全部失联），与探测反射器的 fail-fast 同一立场（见其注释）。
	serveErr := make(chan error, 2)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		go func() {
			if err := control.Serve(ctx); err != nil {
				log.Error("control serve", "error", err)
				serveErr <- err
			}
		}()
		if err := adminServer.Serve(adminListener); err != nil && err != http.ErrServerClosed {
			log.Error("admin serve", "error", err)
			serveErr <- err
		}
	}()
	log.Info("gtun-lite server started",
		"control", control.Addr().String(), "admin", config.Admin.Bind,
		"probe", fmt.Sprintf("%s:%d-%d", config.Probe.Bind, config.Probe.BasePort, config.Probe.BasePort+common.ProbePortCount-1),
		"database", config.Database.Path)

	select {
	case <-ctx.Done():
	case err := <-reflectorErr:
		if err != nil {
			log.Error("probe reflector failed; exiting", "error", err)
			return 1
		}
	case err := <-serveErr:
		log.Error("control or admin listener failed; exiting", "error", err)
		return 1
	}
	log.Info("shutting down")
	// 拆卸顺序与管理面负载无关紧要：两个监听各自独立，
	// 管理连接优雅等待，控制面靠 hub.Close 断开全部会话。
	if err := adminServer.Shutdown(context.Background()); err != nil {
		log.Error("admin shutdown", "error", err)
	}
	<-serveDone
	return 0
}
