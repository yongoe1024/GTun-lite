package client

import (
	"context"
	"fmt"
	"log/slog"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// dataStack 是一次「TUN + 路由 + 数据面」的完整装配，随网络配置拓扑创建，
// 随拓扑变化整体重建。字段只在 manager（控制会话 goroutine）里读写。
type dataStack struct {
	device  tun.Device
	cleanup tun.RouteCleanup
	plane   *tun.DataPlane
	// localIP 是本机虚拟 IP；peerIPs 是当前配置的全部对端虚拟 IP。
	localIP common.IPv4
	peerIPs []common.IPv4
}

// openDataStack 执行 preflight → 打开 TUN → 装配数据面的完整序列。
// 任一步失败都会回滚已创建的资源；返回的 Reason 是失败原因的分类
// （ROUTE_CONFLICT / TUN_CREATE_FAILED），供「栈未就绪时收到 CONNECT
// 立即以真实原因回报失败」使用，错误原文交给日志。
func openDataStack(opener tun.Opener, table tun.RouteTable, config ClientConfig, network *common.NetworkDefinition, serverIP common.IPv4, log *slog.Logger) (*dataStack, common.Reason, error) {
	if network == nil {
		return nil, common.ReasonTUNCreateFailed, fmt.Errorf("open data stack requires a network definition")
	}
	peerIPs := make([]common.IPv4, 0, len(network.Peers))
	for _, peer := range network.Peers {
		peerIPs = append(peerIPs, peer.IP)
	}
	// preflight 在打开新 TUN 之前执行：上一栈关闭时已拆掉自己的路由，
	// 此刻系统里若仍有 /32，那是上次异常退出的残留——如实报冲突，
	// 让运维清理，不静默接管（幂等由「拓扑未变不重建」保证）。
	if err := tun.Preflight(tun.PreflightInput{
		LocalIP: network.IP, Peers: peerIPs, ServerIP: serverIP,
	}, table); err != nil {
		return nil, common.ReasonRouteConflict, fmt.Errorf("route preflight: %w", err)
	}
	device, cleanup, err := opener.Open(context.Background(), config.TUN.Name, config.TUN.MTU, network.IP, peerIPs)
	if err != nil {
		return nil, common.ReasonTUNCreateFailed, fmt.Errorf("open tun: %w", err)
	}
	plane, err := tun.NewDataPlane(device, tun.DataPlaneConfig{
		TUNMTU:               config.TUN.MTU,
		OutboundQueuePackets: config.Tunnel.OutboundQueuePackets,
		InboundQueuePackets:  config.Tunnel.InboundQueuePackets,
		Logger:               log,
	}, network.IP)
	if err != nil {
		_ = device.Close()
		_ = cleanup.Close()
		return nil, common.ReasonTUNCreateFailed, fmt.Errorf("create data plane: %w", err)
	}
	plane.Start()
	log.Info("data stack open", "interface", device.Name(), "mtu", config.TUN.MTU,
		"local_ip", string(network.IP), "peers", len(peerIPs))
	return &dataStack{
		device: device, cleanup: cleanup, plane: plane,
		localIP: network.IP, peerIPs: peerIPs,
	}, "", nil
}

// deliverInbound 把入站包投递进全局队列（队满丢弃由数据面处理）。
func (stack *dataStack) deliverInbound(packet []byte) {
	stack.plane.DeliverInbound(packet)
}

// close 拆卸整栈：先拆路由与地址（此刻接口仍在，ifconfig 有对象可操作；
// 内核对接口的拆除是异步的，不能指望 fd 一关地址就立刻消失——拆栈后
// 立即 preflight/重建的路径不能看到将死接口的地址），再停数据面
// （cancel → 设备关闭 → 等读循环经 poller 唤醒退出，见 tun.DataPlane.Close）。
func (stack *dataStack) close(log *slog.Logger) {
	if err := stack.cleanup.Close(); err != nil {
		log.Warn("tun cleanup reported error", "error", err)
	}
	_ = stack.plane.Close()
	log.Info("data stack closed", "interface", stack.device.Name())
}
