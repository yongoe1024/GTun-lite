package client

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig 是客户端配置。键与默认值见 设计文档/08-构建配置与部署.md 的配置全表；
// cmd/client/client.yaml 是默认值事实源。
type ClientConfig struct {
	Server struct {
		// Addr 是服务器控制面地址。
		Addr string `yaml:"addr"`
		// ProbeBasePort 是服务器探测端口段的起点（连续 5 个端口，
		// 与服务端 probe.base_port 对应）。画像探测向这 5 个端口发包。
		ProbeBasePort int `yaml:"probe_base_port"`
	} `yaml:"server"`
	Identity struct {
		// Path 是设备身份文件路径。文件不存在则首次生成并写回。
		Path string `yaml:"path"`
		// Name 是注册时上报的可读名，默认取主机名。
		Name string `yaml:"name"`
	} `yaml:"identity"`
	Control struct {
		// HeartbeatInterval 是心跳周期，须明显小于服务端 heartbeat_timeout。
		HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
		// RegisterTimeout 限制等待注册响应的时长。
		RegisterTimeout time.Duration `yaml:"register_timeout"`
		// ConnectTimeout 是 TCP 拨号超时。
		ConnectTimeout time.Duration `yaml:"connect_timeout"`
		// ReconnectInterval 是重连间隔。固定值，不做指数退避：
		// 断连常见原因是服务器重启或网络抖动，可预测的固定间隔
		// 让「多久内应该恢复」有明确答案。
		ReconnectInterval time.Duration `yaml:"reconnect_interval"`
		// WriteTimeout 是单条上行消息的写超时。
		WriteTimeout time.Duration `yaml:"write_timeout"`
	} `yaml:"control"`
	TUN struct {
		// Name 是请求的 TUN 接口名（macOS 仅作日志前缀，实际为 utunN）。
		Name string `yaml:"name"`
		// MTU 是内部 IPv4 包上限。本机虚拟 IP 由服务器下发，不在此配置。
		// 上限 1456 = 1500（外层 MTU）- 44（外层开销：IPv4 头 20 + UDP 头 8 +
		// GTUN 帧头 16）——超了就会在物理链路上被分片或丢弃。
		MTU int `yaml:"mtu"`
	} `yaml:"tun"`
	Tunnel struct {
		// OutboundQueuePackets 是每条链路出站队列的包数上限（TUN → 对端）。
		OutboundQueuePackets int `yaml:"outbound_queue"`
		// InboundQueuePackets 是全局入站队列的包数上限（对端 → TUN）。
		InboundQueuePackets int `yaml:"inbound_queue"`
	} `yaml:"tunnel"`
	Probe struct {
		// Timeout 是五端口画像的总预算。
		Timeout time.Duration `yaml:"timeout"`
		// PerPortTimeout 是单个探测端口的响应等待。
		PerPortTimeout time.Duration `yaml:"per_port_timeout"`
		// Retries 是每端口的重试次数。
		Retries int `yaml:"retries"`
	} `yaml:"probe"`
	Punch struct {
		// StableTimeout 是 stable-stable 直连打洞预算。
		StableTimeout time.Duration `yaml:"stable_timeout"`
		// VariableTimeout 是涉及 variable 侧（Range 预测 + helper）的打洞预算。
		VariableTimeout time.Duration `yaml:"variable_timeout"`
		// HelperCount 是 variable 侧 helper 档位：256 / 512 / 1024，
		// 其他值在配置加载阶段即拒绝（CONFIG_INVALID）。
		HelperCount int `yaml:"helper_count"`
	} `yaml:"punch"`
	Logging struct {
		Level string `yaml:"level"`
		// File 与 ErrorFile 是可选的日志落盘路径：普通日志（info 及以下）
		// 与错误日志（warn 及以上）分文件追加写，服务化部署（schtasks/
		// systemd）不依赖启动器重定向。留空维持 stderr。
		File      string `yaml:"file"`
		ErrorFile string `yaml:"error_file"`
	} `yaml:"logging"`
}

// LoadClientConfig 读取并校验客户端配置。严格模式拒绝未知键，理由同服务端：
// 配置是本机文件，拼错键名静默失效比启动报错糟糕。
func LoadClientConfig(path string) (ClientConfig, error) {
	var config ClientConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read client config: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("parse client config: %w", err)
	}
	config.applyDefaults()
	if err := config.validate(); err != nil {
		return config, fmt.Errorf("invalid client config: %w", err)
	}
	return config, nil
}

// applyDefaults 填充省略字段的默认值。
func (config *ClientConfig) applyDefaults() {
	if config.Identity.Path == "" {
		config.Identity.Path = "gtun-device-id"
	}
	if config.Identity.Name == "" {
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			config.Identity.Name = hostname
		} else {
			config.Identity.Name = "gtun-device"
		}
	}
	if config.Control.HeartbeatInterval == 0 {
		config.Control.HeartbeatInterval = 20 * time.Second
	}
	if config.Control.RegisterTimeout == 0 {
		config.Control.RegisterTimeout = 10 * time.Second
	}
	if config.Control.ConnectTimeout == 0 {
		config.Control.ConnectTimeout = 10 * time.Second
	}
	if config.Control.ReconnectInterval == 0 {
		config.Control.ReconnectInterval = 5 * time.Second
	}
	if config.Server.ProbeBasePort == 0 {
		config.Server.ProbeBasePort = 10000
	}
	if config.TUN.Name == "" {
		config.TUN.Name = "gtun0"
	}
	if config.TUN.MTU == 0 {
		// 1456 = 1500 - 44（IPv4+UDP+GTUN 外层开销）：干净 1500 路径上的
		// 无分片上限，每包载荷比 1280 多 14%。物理路径更差（如 PPPoE 1492、
		// 嵌套隧道 1400）时应手动下调到「路径 MTU - 44」，否则外层被分片。
		config.TUN.MTU = 1456
	}
	if config.Tunnel.OutboundQueuePackets == 0 {
		config.Tunnel.OutboundQueuePackets = 4096
	}
	if config.Tunnel.InboundQueuePackets == 0 {
		config.Tunnel.InboundQueuePackets = 4096
	}
	if config.Probe.Timeout == 0 {
		config.Probe.Timeout = 30 * time.Second
	}
	if config.Probe.PerPortTimeout == 0 {
		// 单端口等待取 1s：五个端口串行、每端口含重试最多 4 次等待，
		// 1s×4×5=20s 装得进 30s 总预算；5s 时单端口最坏吃掉 20s，
		// 一个端口被滤会把整轮画像挤爆成 PROBE_TIMEOUT。
		config.Probe.PerPortTimeout = time.Second
	}
	if config.Probe.Retries == 0 {
		config.Probe.Retries = 3
	}
	if config.Punch.StableTimeout == 0 {
		config.Punch.StableTimeout = 5 * time.Second
	}
	if config.Punch.VariableTimeout == 0 {
		config.Punch.VariableTimeout = 15 * time.Second
	}
	if config.Punch.HelperCount == 0 {
		config.Punch.HelperCount = 256
	}
	if config.Control.WriteTimeout == 0 {
		config.Control.WriteTimeout = 5 * time.Second
	}
	if config.Logging.Level == "" {
		config.Logging.Level = "info"
	}
}

// validate 拒绝缺失或自相矛盾的取值。服务器地址没有默认值：
// 它是部署参数，猜一个只会掩盖配置文件没读到的问题。
func (config ClientConfig) validate() error {
	if config.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if config.Logging.File != "" && config.Logging.File == config.Logging.ErrorFile {
		return errors.New("logging.file and logging.error_file must differ")
	}
	if config.Control.HeartbeatInterval <= 0 || config.Control.RegisterTimeout <= 0 ||
		config.Control.ConnectTimeout <= 0 || config.Control.ReconnectInterval <= 0 || config.Control.WriteTimeout <= 0 {
		return errors.New("control timeouts must be positive")
	}
	if config.Identity.Name == "" || len(config.Identity.Name) > 128 {
		return errors.New("identity.name must contain 1 to 128 bytes")
	}
	if config.Server.ProbeBasePort < 1 || config.Server.ProbeBasePort > 65531 {
		return errors.New("server.probe_base_port must be between 1 and 65531")
	}
	if config.Probe.Timeout <= 0 || config.Probe.PerPortTimeout <= 0 || config.Probe.Timeout < config.Probe.PerPortTimeout {
		return errors.New("probe timeouts must be positive and total must cover per-port wait")
	}
	if config.Probe.Retries < 0 {
		return errors.New("probe.retries must not be negative")
	}
	if config.Punch.StableTimeout <= 0 || config.Punch.VariableTimeout <= 0 {
		return errors.New("punch timeouts must be positive")
	}
	// MTU 按 44 字节外层开销校验：IPv4 20 + UDP 8 + GTUN 帧头 16。
	// 外层 MTU 取以太网标准 1500 常量——按最保守物理链路约束，
	// 巨帧环境需要更大 MTU 时再把它做成配置项，现在不做。
	if config.TUN.MTU < 20 || config.TUN.MTU > 1500-44 {
		return fmt.Errorf("CONFIG_INVALID: tun.mtu must be between 20 and %d (1500 outer MTU minus 44 bytes of IPv4+UDP+GTUN headers), got %d", 1500-44, config.TUN.MTU)
	}
	if config.Tunnel.OutboundQueuePackets <= 0 || config.Tunnel.InboundQueuePackets <= 0 {
		return errors.New("tunnel queue lengths must be positive")
	}
	// 档位校验在配置加载阶段执行：非法值属于 CONFIG_INVALID，
	// 启动即拒绝，绝不静默取整或降档（档位决定端口覆盖率，是可比较的调优旋钮）。
	if err := ValidateHelperCount(config.Punch.HelperCount); err != nil {
		return fmt.Errorf("CONFIG_INVALID: %w", err)
	}
	switch config.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logging.level must be debug, info, warn or error")
	}
	return nil
}
