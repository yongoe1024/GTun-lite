package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig 写一个最小可用的客户端配置，覆盖指定字段。
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	content := "server:\n  addr: \"127.0.0.1:10000\"\n" + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadClientConfigDefaults 省略 probe/punch 段时默认值齐全且可用。
func TestLoadClientConfigDefaults(t *testing.T) {
	config, err := LoadClientConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Probe.Timeout != 30*time.Second || config.Probe.PerPortTimeout != time.Second || config.Probe.Retries != 3 {
		t.Fatalf("unexpected probe defaults: %+v", config.Probe)
	}
	if config.Punch.StableTimeout != 5*time.Second || config.Punch.VariableTimeout != 15*time.Second || config.Punch.HelperCount != 256 {
		t.Fatalf("unexpected punch defaults: %+v", config.Punch)
	}
	if config.Server.ProbeBasePort != 10000 {
		t.Fatalf("unexpected probe base port: %d", config.Server.ProbeBasePort)
	}
}

// TestLoadClientConfigRejectsBadTier 非法 helper 档位在配置加载阶段即拒绝，
// 错误信息以 CONFIG_INVALID 开头（启动阶段拒绝优于运行期降档）。
func TestLoadClientConfigRejectsBadTier(t *testing.T) {
	_, err := LoadClientConfig(writeConfig(t, "punch:\n  helper_count: 300\n"))
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("expected CONFIG_INVALID rejection, got %v", err)
	}
}

// TestLoadClientConfigAcceptsTiers 三个合法档位全部放行。
func TestLoadClientConfigAcceptsTiers(t *testing.T) {
	for _, tier := range []int{256, 512, 1024} {
		path := writeConfig(t, "punch:\n  helper_count: "+itoa(tier)+"\n")
		if _, err := LoadClientConfig(path); err != nil {
			t.Fatalf("tier %d rejected: %v", tier, err)
		}
	}
}

// TestLoadClientConfigRejectsUnknownKeys 配置是本机文件：拼错键名必须报错。
func TestLoadClientConfigRejectsUnknownKeys(t *testing.T) {
	if _, err := LoadClientConfig(writeConfig(t, "prob:\n  timeout: 1s\n")); err == nil {
		t.Fatal("unknown key must be rejected")
	}
}

// itoa 避免仅为测试引入 strconv 依赖的误导——直接用 fmt 反而常见。
func itoa(value int) string {
	digits := ""
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// TestEnsureHelperFDHeadroom 正常档位要么已够、要么自举抬高后必够；
// 荒谬档位（超过任何合理硬上限）必须拒绝且不产生副作用。
// 测试进程的软上限只会被抬高、不会被调低，不影响其他用例。
func TestEnsureHelperFDHeadroom(t *testing.T) {
	if err := EnsureHelperFDHeadroom(1024); err != nil {
		t.Fatalf("highest tier must fit after bootstrap: %v", err)
	}
	if err := EnsureHelperFDHeadroom(1 << 28); err == nil {
		t.Fatal("absurd helper count must be rejected")
	}
}

// TestLoadClientConfigMTUBounds MTU 按 44 字节外层开销校验：20..1456。
func TestLoadClientConfigMTUBounds(t *testing.T) {
	if _, err := LoadClientConfig(writeConfig(t, "tun:\n  mtu: 1457\n")); err == nil {
		t.Fatal("mtu above 1456 (1500 - 44) must be rejected")
	}
	if _, err := LoadClientConfig(writeConfig(t, "tun:\n  mtu: 19\n")); err == nil {
		t.Fatal("mtu below 20 must be rejected")
	}
	if _, err := LoadClientConfig(writeConfig(t, "tun:\n  mtu: 1280\n")); err != nil {
		t.Fatalf("default mtu rejected: %v", err)
	}
}
