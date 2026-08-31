package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
	"unicode/utf8"

	"gtun-lite/internal/common"
)

// sendQueueDepth 是每会话发送缓冲的长度。满则阻塞（控制面不丢消息，
// 见 session.deliver），深度只需吸收写循环的正常抖动。
const sendQueueDepth = 64

// ControlServer 承载 TCP 监听与单连接生命周期：分帧、注册、心跳超时、
// 上下行分发。状态裁决全部在 hub，本文件不持有任何跨连接状态。
type ControlServer struct {
	hub    *Hub
	config ServerConfig
	log    *slog.Logger

	listener net.Listener
}

// NewControlServer 创建控制面服务（未监听）。
func NewControlServer(owner *Hub, config ServerConfig, log *slog.Logger) *ControlServer {
	return &ControlServer{hub: owner, config: config, log: log}
}

// Listen 绑定控制端口。
func (server *ControlServer) Listen() error {
	listener, err := net.Listen("tcp", server.config.Control.Bind)
	if err != nil {
		return fmt.Errorf("listen control %s: %w", server.config.Control.Bind, err)
	}
	server.listener = listener
	return nil
}

// Addr 返回实际监听地址（测试用 0 端口时与配置不同）。
func (server *ControlServer) Addr() net.Addr {
	if server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

// Serve 进入接受循环，直到监听器关闭或 ctx 取消。
func (server *ControlServer) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = server.listener.Close()
	}()
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // 停机路径：监听器被关闭属预期
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go server.handleConnection(connection)
	}
}

// handleConnection 走完一条连接的生命周期：注册 → 消息循环 → 终结。
//
// 心跳超时用读超时实现：每次读前把 deadline 设为 now+HeartbeatTimeout，
// 任何入站消息（心跳或状态上报）都算活着。另一种做法是每会话一个 timer
// 加代次防误删（续期与顶替都换新 timer，回调核对代次才生效）；读超时
// 本身绑定在连接上，不存在旧 timer 误伤新会话的问题，等价且少一整层。
func (server *ControlServer) handleConnection(connection net.Conn) {
	log := server.log.With("remote", connection.RemoteAddr().String())
	reader, err := common.NewLineReader(connection, common.MaxControlMessageBytes)
	if err != nil {
		_ = connection.Close()
		return
	}

	// 注册阶段：未注册连接不占会话表，给它一个更短的沉默上限。
	if err := connection.SetReadDeadline(time.Now().Add(server.config.Control.RegisterTimeout)); err != nil {
		_ = connection.Close()
		return
	}
	line, err := reader.ReadLine()
	if err != nil {
		server.reject(connection, common.ErrorRegisterTimeout, err.Error())
		return
	}
	message, err := common.DecodeMessage(line)
	if err != nil {
		server.reject(connection, errorCode(err), err.Error())
		return
	}
	registration, ok := message.(*common.DeviceRegister)
	if !ok {
		server.reject(connection, common.ErrorUnknownType, "first message must be device_register")
		return
	}

	// 会话对象在注册成功前就已建立：注册把「本连接」登记为该设备的当前
	// 会话，hub 需要能向它投递（首份配置、duplicate_login）。
	sess := &session{
		device: registration.DeviceID,
		conn:   connection,
		send:   make(chan outbound, sendQueueDepth),
		dead:   make(chan struct{}),
	}
	go server.runWriter(sess)
	if err := server.hub.Register(context.Background(), sess, registration); err != nil {
		// 注册失败是协议终态：说明原因后断开。容量满单独成码——
		// 它不是服务器故障，重连也不会成功，客户端日志应能区分两者。
		code := common.ErrorInternal
		if errors.Is(err, ErrConnectionLimit) {
			code = common.ErrorServerFull
		}
		// 写出与关闭都交给 rejectSession：入队成功时写循环写完 error 自行
		// 关闭（closeAfter），失败时 rejectSession 内部兜底 end。这里再补
		// 一个 end 会抢在写出之前关连接，客户端只看到 EOF。
		server.rejectSession(sess, code, err.Error())
		return
	}
	defer func() {
		sess.end()
		// 读循环退出是会话终结的正常路径之一；hub 摘除会话、记一条
		// 离线日志，链路状态不动（不变量 1）。
		server.hub.SessionEnded(context.Background(), sess)
	}()

	for {
		if err := connection.SetReadDeadline(time.Now().Add(server.config.Control.HeartbeatTimeout)); err != nil {
			return
		}
		line, err := reader.ReadLine()
		if err != nil {
			if isTimeout(err) {
				log.Warn("session timed out waiting for heartbeat")
			}
			return
		}
		message, err := common.DecodeMessage(line)
		if err != nil {
			server.rejectSession(sess, errorCode(err), err.Error())
			return
		}
		if terminal := server.dispatch(sess, message); terminal {
			return
		}
	}
}

// dispatch 处理注册后的上行消息，返回 true 表示连接必须终止。
func (server *ControlServer) dispatch(sess *session, message common.Message) bool {
	server.log.Debug("client message received", "device_id", string(sess.device), "type", message.MessageType())
	switch typed := message.(type) {
	case *common.DeviceHeartbeat:
		if err := server.hub.Heartbeat(context.Background(), sess); err != nil {
			// 会话已被顶替或服务器停机：静默终止，被顶替的旧连接
			// 已收到 duplicate_login。
			return true
		}
		return false
	case *common.StateReport:
		if err := server.hub.HandleStateReport(context.Background(), sess, typed); err != nil {
			server.rejectSession(sess, common.ErrorInternal, err.Error())
			return true
		}
		return false
	case *common.ProfileReport:
		if err := server.hub.HandleProfileReport(context.Background(), sess, typed); err != nil {
			server.rejectSession(sess, common.ErrorInternal, err.Error())
			return true
		}
		return false
	case *common.DeviceUnregister:
		// 客户端礼貌退出。会话在 defer 里摘除，这里只终止读循环。
		server.log.Info("device unregistered", "device_id", string(sess.device))
		return true
	default:
		// 下行消息类型（network_config/connect/...）出现在上行方向是
		// 协议错误，按 unknown_type 终止——方向表是封闭的。
		server.rejectSession(sess, common.ErrorUnknownType, "message direction is not allowed")
		return true
	}
}

// runWriter 是会话唯一的网络写者，保证下行顺序。
func (server *ControlServer) runWriter(sess *session) {
	for {
		select {
		case out := <-sess.send:
			data, err := json.Marshal(out.message)
			if err != nil {
				server.log.Error("marshal outbound message", "error", err)
				sess.end()
				return
			}
			if err := sess.conn.SetWriteDeadline(time.Now().Add(server.config.Control.WriteTimeout)); err != nil {
				sess.end()
				return
			}
			if _, err := sess.conn.Write(append(data, '\n')); err != nil {
				sess.end()
				return
			}
			if out.closeAfter {
				sess.end()
				return
			}
		case <-sess.dead:
			return
		}
	}
}

// reject 给未注册连接发终止消息并断开。
func (server *ControlServer) reject(connection net.Conn, code, detail string) {
	message := &common.ErrorMessage{Type: common.MessageError, Code: code, Message: detail}
	data, err := json.Marshal(message)
	if err != nil {
		_ = connection.Close()
		return
	}
	_ = connection.SetWriteDeadline(time.Now().Add(server.config.Control.WriteTimeout))
	_, _ = connection.Write(append(data, '\n'))
	_ = connection.Close()
}

// rejectSession 给已注册会话发终止消息并触发关闭。
// clampDetail 把错误回执正文压进协议上限以内。ErrorMessage.Validate 要求
// 1..512 字节；detail 可能携带外来内容（如 DecodeMessage 对未知 type 的
// %q 转义，可达 64KB），超限回执会被对端 Validate 拒收，错误分类随之丢失。
// 截断按字节回退到 UTF-8 边界，并留出省略号余量。
func clampDetail(detail string) string {
	const limit = 480 // 512 上限减去截断后缀与富余
	if len(detail) <= limit {
		return detail
	}
	truncated := detail[:limit]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…(truncated)"
}

func (server *ControlServer) rejectSession(sess *session, code, detail string) {
	message := &common.ErrorMessage{Type: common.MessageError, Code: code, Message: clampDetail(detail)}
	if err := sess.deliver(message, true, server.config.Control.WriteTimeout); err != nil {
		sess.end()
	}
}

// errorCode 把解码错误归类到协议错误码。
func errorCode(err error) string {
	if errors.Is(err, common.ErrUnknownMessageType) {
		return common.ErrorUnknownType
	}
	return common.ErrorInvalidMessage
}

// isTimeout 识别 net 层超时错误。
func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
