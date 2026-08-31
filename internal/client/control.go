package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"gtun-lite/internal/common"
	"gtun-lite/internal/notice"
)

// ErrDuplicateIdentity 是进程级终态：同一设备身份的另一条连接完成了注册，
// 本进程被顶替。继续重连只会与新实例互相顶替，把问题暴露出来交给运维。
var ErrDuplicateIdentity = errors.New("device identity was taken over by another connection")

// ControlClient 维护与服务器的重连控制会话。
//
//   - 重连间隔固定（设计决定，见 ClientConfig.ReconnectInterval 的注释）。
//   - 会话建立即全量上报：服务器可能刚重启过，它的链路状态表是空的，
//     客户端的第一份快照就是它的重建来源。
//   - 控制连接断开不影响已建成的隧道：隧道是 P2P 的，不经过服务器。
//     断连期间不停 Worker、不拆路由，只重连。
type ControlClient struct {
	config   ClientConfig
	manager  *Manager
	identity common.DeviceID
	log      *slog.Logger
	window   *notice.Notice
	// writeMu 串行化同一连接上的上行写：主循环（心跳/事件上报）与读
	// goroutine（QUERY 回应）并发写同一 TCP 连接，JSON Lines 的消息边界
	// 依赖单次完整 Write，交错即协议帧损坏。
	writeMu sync.Mutex
}

// NewControlClient 创建控制客户端。
func NewControlClient(config ClientConfig, manager *Manager, identity common.DeviceID, log *slog.Logger, window *notice.Notice) *ControlClient {
	return &ControlClient{config: config, manager: manager, identity: identity, log: log, window: window}
}

// Run 维持控制会话直到 ctx 取消，或收到顶替通知（ErrDuplicateIdentity）。
// 单次会话失败不返回错误：重连是常态路径，只有进程级终态才向上冒泡。
func (client *ControlClient) Run(ctx context.Context) error {
	for {
		err := client.runSession(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrDuplicateIdentity):
			return err
		case err != nil:
			client.log.Warn("control session ended; reconnecting", "error", err, "interval", client.config.Control.ReconnectInterval.String())
			client.window.Printf("与端口服务器连接中断，%s 后重连", client.config.Control.ReconnectInterval.String())
		}
		select {
		case <-time.After(client.config.Control.ReconnectInterval):
		case <-ctx.Done():
			return nil
		}
	}
}

// runSession 走完一条控制连接的完整生命周期。
func (client *ControlClient) runSession(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: client.config.Control.ConnectTimeout, KeepAlive: 30 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", client.config.Server.Addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// 会话失败路径统一收口到这一个 defer：关连接、礼貌注销（尽力而为）。
	graceful := false
	defer func() {
		if graceful {
			client.writeMessage(connection, &common.DeviceUnregister{Type: common.MessageDeviceUnregister})
		}
		_ = connection.Close()
	}()

	reader, err := common.NewLineReader(connection, common.MaxControlMessageBytes)
	if err != nil {
		return err
	}
	if err := client.register(connection, reader); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	// 注册成功后的首份配置在服务器侧紧随 device_registered 发出，
	// 等到它再上报——快照按配置枚举，配置未到时上报是空的。
	initialConfig, err := client.awaitInitialConfig(connection, reader)
	if err != nil {
		return fmt.Errorf("await initial config: %w", err)
	}
	client.manager.ApplyConfig(initialConfig)
	if err := client.writeMessage(connection, client.manager.StateReport()); err != nil {
		return fmt.Errorf("report initial state: %w", err)
	}
	client.log.Info("control session established", "server", client.config.Server.Addr)
	client.window.Printf("已连接端口服务器 %s", client.config.Server.Addr)

	graceful = true // 从这里起，异常退出都尝试礼貌注销；正常退出路径同样注销
	return client.messageLoop(ctx, connection, reader)
}

// register 完成注册握手：发出 device_register 并等待确认。
// 一条连接一生只有一次注册，TCP 有序保证第一条 device_registered
// 就是本次请求的答案，因此不需要请求关联 ID。
func (client *ControlClient) register(connection net.Conn, reader *common.LineReader) error {
	message := &common.DeviceRegister{
		Type:     common.MessageDeviceRegister,
		DeviceID: client.identity,
		Name:     client.config.Identity.Name,
		Platform: platform(),
	}
	if err := client.writeMessage(connection, message); err != nil {
		return err
	}
	if err := connection.SetReadDeadline(time.Now().Add(client.config.Control.RegisterTimeout)); err != nil {
		return err
	}
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return err
		}
		decoded, err := common.DecodeMessage(line)
		if err != nil {
			return err
		}
		switch typed := decoded.(type) {
		case *common.DeviceRegistered:
			return nil
		case *common.ErrorMessage:
			return fmt.Errorf("server rejected registration: %s: %s", typed.Code, typed.Message)
		case *common.DuplicateLogin:
			return ErrDuplicateIdentity
		default:
			// 注册确认前的其他消息属于协议乱序，跳过等下去没有意义。
			return fmt.Errorf("unexpected message before device_registered: %s", decoded.MessageType())
		}
	}
}

// awaitInitialConfig 读取注册后服务器推送的首份全量配置。
func (client *ControlClient) awaitInitialConfig(connection net.Conn, reader *common.LineReader) (*common.NetworkConfig, error) {
	if err := connection.SetReadDeadline(time.Now().Add(client.config.Control.RegisterTimeout)); err != nil {
		return nil, err
	}
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		decoded, err := common.DecodeMessage(line)
		if err != nil {
			return nil, err
		}
		switch typed := decoded.(type) {
		case *common.NetworkConfig:
			return typed, nil
		case *common.ErrorMessage:
			return nil, fmt.Errorf("server error: %s: %s", typed.Code, typed.Message)
		case *common.DuplicateLogin:
			return nil, ErrDuplicateIdentity
		default:
			return nil, fmt.Errorf("unexpected message before network_config: %s", decoded.MessageType())
		}
	}
}

// messageLoop 是稳态：定时心跳，处理下行消息，直到出错或 ctx 取消。
func (client *ControlClient) messageLoop(ctx context.Context, connection net.Conn, reader *common.LineReader) error {
	// 读侧不设 deadline：服务器静默由 TCP keepalive 兜底检测，
	// 写侧失败（心跳写不出去）是更早的失效信号。
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	heartbeat := time.NewTicker(client.config.Control.HeartbeatInterval)
	defer heartbeat.Stop()
	readFailure := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadLine()
			if err != nil {
				readFailure <- err
				return
			}
			decoded, err := common.DecodeMessage(line)
			if err != nil {
				readFailure <- fmt.Errorf("decode: %w", err)
				return
			}
			if err := client.handleServerMessage(connection, decoded); err != nil {
				readFailure <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil // 优雅路径：defer 发 unregister
		case err := <-readFailure:
			return err
		case event := <-client.manager.Events():
			if err := client.sendWorkerEvent(connection, event); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := client.writeMessage(connection, &common.DeviceHeartbeat{Type: common.MessageDeviceHeartbeat}); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
	}
}

// sendWorkerEvent 把 Worker 里程碑翻译成 TCP 上报：
// 画像 → profile_report；建成 → 转场成功；失败/失活 → 转场失败（带原因）。
// 管理器视图的同步（移除终结 Worker）也在这里完成。
func (client *ControlClient) sendWorkerEvent(connection net.Conn, event WorkerEvent) error {
	client.manager.applyEvent(event)
	switch event.Kind {
	case WorkerProfile:
		return client.writeMessage(connection, &common.ProfileReport{
			Type: common.MessageProfileReport, Token: event.Token, PeeringID: event.PeeringID, Profile: *event.Profile,
		})
	case WorkerConnected:
		return client.writeMessage(connection, &common.StateReport{
			Type: common.MessageStateReport, Full: false,
			Links: []common.LinkReport{{PeeringID: event.PeeringID, State: common.StateConnected, Token: event.Token}},
		})
	default: // 失败与失活同为转场失败，原因随事件携带
		return client.writeMessage(connection, &common.StateReport{
			Type: common.MessageStateReport, Full: false,
			Links: []common.LinkReport{{PeeringID: event.PeeringID, State: common.StateIdle, Token: event.Token, Reason: event.Reason}},
		})
	}
}

// handleServerMessage 处理一条下行消息。返回错误表示会话必须终止。
func (client *ControlClient) handleServerMessage(connection net.Conn, message common.Message) error {
	client.log.Debug("server message received", "type", message.MessageType())
	switch typed := message.(type) {
	case *common.NetworkConfig:
		client.manager.ApplyConfig(typed)
		// Network 为 nil（设备被移出网络）是合法配置，日志不能解引用它。
		peers := 0
		if typed.Network != nil {
			peers = len(typed.Network.Peers)
		}
		client.log.Info("network config applied", "network", networkLabel(typed), "peers", peers)
		return nil
	case *common.Connect:
		client.manager.HandleConnect(typed)
		return nil
	case *common.PeerProfile:
		client.manager.HandlePeerProfile(typed)
		return nil
	case *common.Disconnect:
		client.manager.HandleDisconnect(typed)
		return nil
	case *common.Query:
		// QUERY 的回应与重连上报同一形状：全量快照。
		return client.writeMessage(connection, client.manager.StateReport())
	case *common.DuplicateLogin:
		return ErrDuplicateIdentity
	case *common.ErrorMessage:
		// 服务器终止性错误：结束会话走重连。重连若仍被拒，
		// 日志会持续出现同一错误码，问题暴露而非静默。
		return fmt.Errorf("server error: %s: %s", typed.Code, typed.Message)
	default:
		return fmt.Errorf("unexpected message direction: %s", message.MessageType())
	}
}

// writeMessage 序列化并写一条上行消息（JSON Lines）。writeMu 保证消息
// 不与并发写交错（见结构体注释）。
func (client *ControlClient) writeMessage(connection net.Conn, message common.Message) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := connection.SetWriteDeadline(time.Now().Add(client.config.Control.WriteTimeout)); err != nil {
		return err
	}
	client.writeMu.Lock()
	_, err = connection.Write(append(data, '\n'))
	client.writeMu.Unlock()
	return err
}

// networkLabel 取配置的网络名，空配置显示 none。
func networkLabel(config *common.NetworkConfig) string {
	if config.Network == nil {
		return "none"
	}
	return config.Network.Name
}

// platform 返回注册上报的平台标识，与 devices 表的 CHECK 集合一致。
func platform() string {
	switch runtime.GOOS {
	case "darwin", "linux", "windows", "android":
		return runtime.GOOS
	default:
		// 未预期的平台按 linux 报：注册不会被拒，真机验收只覆盖已列平台，
		// 这里不值得为不可达分支引入复杂度。
		return "linux"
	}
}
