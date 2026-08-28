package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig 是服务端配置。键与默认值见 设计文档/08-构建配置与部署.md 的配置全表；
// cmd/server/server.yaml 是默认值事实源。
type ServerConfig struct {
	Control struct {
		// Bind 是 TCP 控制面监听地址。
		Bind string `yaml:"bind"`
		// RegisterTimeout 限制注册前连接可以沉默多久。未注册连接不占会话
		// 表，超时即断，防止裸连接堆积。
		RegisterTimeout time.Duration `yaml:"register_timeout"`
		// HeartbeatTimeout 是两次任意入站消息（含心跳）的最大间隔。
		// 超过即判定会话失活——只影响在线性，不动链路状态（不变量 1）。
		HeartbeatTimeout time.Duration `yaml:"heartbeat_timeout"`
		// WriteTimeout 是单条下行消息的写超时，也是发送队列的卡死上限。
		WriteTimeout time.Duration `yaml:"write_timeout"`
		// MaxConnections 是同时在线的会话数上限，超出即拒绝注册。
		MaxConnections int `yaml:"max_connections"`
	} `yaml:"control"`
	Probe struct {
		// Bind 是 5 个 UDP 探测端点的绑定地址。
		Bind string `yaml:"bind"`
		// BasePort 是探测端口段的起点，连续占用 5 个端口。
		BasePort int `yaml:"base_port"`
	} `yaml:"probe"`
	Admin struct {
		// Bind 是管理 API 监听地址。默认只绑回环地址：管理面没有鉴权，
		// 暴露到非回环地址是运维显式做出的决定。
		Bind string `yaml:"bind"`
	} `yaml:"admin"`
	Database struct {
		// Path 是 SQLite 数据库文件路径。
		Path string `yaml:"path"`
	} `yaml:"database"`
	Limits struct {
		// MaxDevicesPerNetwork 限制单个网络的成员数。上限由虚拟 IP 空间
		// 与「单机小规模」的产品定位决定，不是性能测量值。
		MaxDevicesPerNetwork int `yaml:"max_devices_per_network"`
		// MinCIDRPrefix / MaxCIDRPrefix 约束建网时的 CIDR 前缀长度，
		// 同时排除「网段大到浪费」与「小到放不下设备」两种配置错误。
		MinCIDRPrefix int `yaml:"min_cidr_prefix"`
		MaxCIDRPrefix int `yaml:"max_cidr_prefix"`
	} `yaml:"limits"`
	Logging struct {
		Level string `yaml:"level"`
		// File 与 ErrorFile 是可选的日志落盘路径：普通日志（info 及以下）
		// 与错误日志（warn 及以上）分文件追加写，服务化部署不依赖启动器
		// 重定向。留空维持 stderr。
		File      string `yaml:"file"`
		ErrorFile string `yaml:"error_file"`
	} `yaml:"logging"`
}

// LoadServerConfig 读取并校验服务端配置。
//
// YAML 用严格模式（KnownFields）：配置是本机文件不是线上协议，拼错键名
// 静默失效比启动报错糟糕得多——这与控制协议「允许未知字段」相反，
// 两者的兼容性需求方向本就相反。
func LoadServerConfig(path string) (ServerConfig, error) {
	var config ServerConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read server config: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("parse server config: %w", err)
	}
	config.applyDefaults()
	if err := config.validate(); err != nil {
		return config, fmt.Errorf("invalid server config: %w", err)
	}
	return config, nil
}

// applyDefaults 填充省略字段的默认值。默认值即推荐值；要偏离就在文件里
// 显式写出来，配置显式优于隐式协商。
func (config *ServerConfig) applyDefaults() {
	if config.Control.Bind == "" {
		config.Control.Bind = "0.0.0.0:10000"
	}
	if config.Control.RegisterTimeout == 0 {
		config.Control.RegisterTimeout = 10 * time.Second
	}
	if config.Control.HeartbeatTimeout == 0 {
		config.Control.HeartbeatTimeout = 60 * time.Second
	}
	if config.Control.WriteTimeout == 0 {
		config.Control.WriteTimeout = 5 * time.Second
	}
	if config.Control.MaxConnections == 0 {
		config.Control.MaxConnections = 1000
	}
	if config.Probe.Bind == "" {
		config.Probe.Bind = "0.0.0.0"
	}
	if config.Probe.BasePort == 0 {
		config.Probe.BasePort = 10000
	}
	if config.Admin.Bind == "" {
		config.Admin.Bind = "127.0.0.1:9090"
	}
	if config.Database.Path == "" {
		config.Database.Path = "gtun.db"
	}
	if config.Limits.MaxDevicesPerNetwork == 0 {
		config.Limits.MaxDevicesPerNetwork = 8
	}
	if config.Limits.MinCIDRPrefix == 0 {
		config.Limits.MinCIDRPrefix = 24
	}
	if config.Limits.MaxCIDRPrefix == 0 {
		config.Limits.MaxCIDRPrefix = 28
	}
	if config.Logging.Level == "" {
		config.Logging.Level = "info"
	}
}

// validate 拒绝自相矛盾的取值。
func (config ServerConfig) validate() error {
	if config.Logging.File != "" && config.Logging.File == config.Logging.ErrorFile {
		return errors.New("logging.file and logging.error_file must differ")
	}
	if config.Control.RegisterTimeout <= 0 || config.Control.HeartbeatTimeout <= 0 || config.Control.WriteTimeout <= 0 {
		return errors.New("control timeouts must be positive")
	}
	// 心跳超时与客户端心跳间隔的配比不做前置校验：错配的暴露形式是会话
	// 周期性掉线重连，日志立刻可见；把「客户端默认 20s」硬编码进服务端
	// 校验反而制造两端默认值的隐性耦合。
	if config.Control.MaxConnections < 1 {
		return errors.New("max_connections must be at least 1")
	}
	// 0 不进配置文件：那是测试用「内核分配」的特殊值，生产端口必须显式。
	if config.Probe.BasePort < 1 || config.Probe.BasePort > 65531 {
		return errors.New("probe.base_port must be between 1 and 65531")
	}
	if config.Limits.MaxDevicesPerNetwork < 2 {
		return errors.New("max_devices_per_network must be at least 2")
	}
	if config.Limits.MinCIDRPrefix < 16 || config.Limits.MaxCIDRPrefix > 29 || config.Limits.MinCIDRPrefix > config.Limits.MaxCIDRPrefix {
		return errors.New("cidr prefix limits must satisfy 16 <= min <= max <= 29")
	}
	// 设备上限与最小网段的交叉校验：可用主机数 = 2^(32-前缀) - 2（去网络
	// 地址与广播地址）。/29 配 8 台设备属于「启动不报错、第 7 台入网才
	// 地址耗尽」的矛盾配置，提前到启动期拒绝——与 KnownFields 同一立场：
	// 配置错误越早暴露越好。
	usableHosts := 1<<(32-config.Limits.MaxCIDRPrefix) - 2
	if config.Limits.MaxDevicesPerNetwork > usableHosts {
		return fmt.Errorf("max_devices_per_network %d exceeds %d usable host addresses in the smallest allowed prefix /%d",
			config.Limits.MaxDevicesPerNetwork, usableHosts, config.Limits.MaxCIDRPrefix)
	}
	switch config.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logging.level must be debug, info, warn or error")
	}
	return nil
}
