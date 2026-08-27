package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeServerConfig 写一个最小可用的服务端配置；limits 段可替换覆盖。
func writeServerConfig(t *testing.T, limits string) string {
	t.Helper()
	if limits == "" {
		limits = strings.Join([]string{
			"limits:",
			"  max_devices_per_network: 8",
			"  min_cidr_prefix: 24",
			"  max_cidr_prefix: 28",
		}, "\n")
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	content := strings.Join([]string{
		"control:",
		"  bind: \"127.0.0.1:10000\"",
		"  register_timeout: 10s",
		"  heartbeat_timeout: 60s",
		"  write_timeout: 5s",
		"  max_connections: 16",
		"probe:",
		"  bind: \"0.0.0.0\"",
		"  base_port: 10000",
		"admin:",
		"  bind: \"127.0.0.1:19090\"",
		"database:",
		"  path: \"test.db\"",
		limits,
		"logging:",
		"  level: \"info\"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadServerConfigDefaults 默认值齐全且通过交叉校验（/28 有 14 个可用
// 主机地址，装得下默认 8 台设备）。
func TestLoadServerConfigDefaults(t *testing.T) {
	config, err := LoadServerConfig(writeServerConfig(t, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Control.MaxConnections != 16 || config.Limits.MaxDevicesPerNetwork != 8 ||
		config.Limits.MinCIDRPrefix != 24 || config.Limits.MaxCIDRPrefix != 28 {
		t.Fatalf("unexpected config: %+v", config)
	}
	if config.Control.HeartbeatTimeout != 60*time.Second {
		t.Fatalf("unexpected heartbeat timeout: %s", config.Control.HeartbeatTimeout)
	}
}

// TestLoadServerConfigRejectsDeviceOverflow 设备上限超过最小网段可用主机数
// 时启动即拒绝：/29 只有 6 个可用地址，配 8 台设备是矛盾配置。
func TestLoadServerConfigRejectsDeviceOverflow(t *testing.T) {
	limits := strings.Join([]string{
		"limits:",
		"  max_devices_per_network: 8",
		"  min_cidr_prefix: 24",
		"  max_cidr_prefix: 29",
	}, "\n")
	if _, err := LoadServerConfig(writeServerConfig(t, limits)); err == nil || !strings.Contains(err.Error(), "usable host addresses") {
		t.Fatalf("expected usable-host cross-check failure, got %v", err)
	}
}

// TestLoadServerConfigAcceptsTightBoundary 恰好贴边界的组合通过：/29 的
// 6 个可用地址配 6 台设备。
func TestLoadServerConfigAcceptsTightBoundary(t *testing.T) {
	limits := strings.Join([]string{
		"limits:",
		"  max_devices_per_network: 6",
		"  min_cidr_prefix: 24",
		"  max_cidr_prefix: 29",
	}, "\n")
	if _, err := LoadServerConfig(writeServerConfig(t, limits)); err != nil {
		t.Fatalf("boundary combination must load: %v", err)
	}
}
