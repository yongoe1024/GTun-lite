package client

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// repoPath 从本测试文件位置推仓库内路径。客户端包的 TestMain 会把工作
// 目录切到临时目录（scanlog 落盘用），相对路径在测试里不可用。
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", rel)
}

// clientTemplate 读仓库内的客户端配置模板（构建产物随包分发的同一文件）。
func clientTemplate(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, filepath.Join("cmd", "client", "client.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// writeConfig 写全量客户端配置：DefaultConfig 打底、extra 覆盖（yaml
// 合并语义，段内只写要改的键），序列化落盘。
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	config := DefaultConfig()
	config.Server.Addr = "127.0.0.1:10000"
	if extra != "" {
		if err := yaml.Unmarshal([]byte(extra), &config); err != nil {
			t.Fatal(err)
		}
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClientTemplateMatchesDefaultConfig 模板与代码默认值的一致性锁：
// 两者漂移（改了一处忘另一处）在此处失败。Identity.Name 派生自主机名、
// Logging File/ErrorFile 是部署参数，不在默认值内，比较前清零。
func TestClientTemplateMatchesDefaultConfig(t *testing.T) {
	config, err := LoadClientConfig(repoPath(t, filepath.Join("cmd", "client", "client.yaml")))
	if err != nil {
		t.Fatalf("template must load: %v", err)
	}
	config.Identity.Name = ""
	config.Server.Addr = "" // 部署参数：模板必须写，默认值里没有
	config.Logging.File = ""
	config.Logging.ErrorFile = ""
	if !reflect.DeepEqual(config, DefaultConfig()) {
		t.Fatalf("template drifted from DefaultConfig():\n got %+v\nwant %+v", config, DefaultConfig())
	}
}

// TestLoadClientConfigRejectsMissingKey 全量显式：缺任何必填键拒绝启动，
// 错误信息点名缺的键。
func TestLoadClientConfigRejectsMissingKey(t *testing.T) {
	content := strings.Replace(clientTemplate(t), "  stable_timeout: 2s", "", 1)
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadClientConfig(path)
	if err == nil || !strings.Contains(err.Error(), "punch.stable_timeout is required") {
		t.Fatalf("expected missing-key rejection, got %v", err)
	}
}

// TestLoadClientConfigDefaults 模板默认值齐全且可用。
func TestLoadClientConfigDefaults(t *testing.T) {
	config, err := LoadClientConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Probe.Timeout != 30*time.Second || config.Probe.PerPortTimeout != time.Second || config.Probe.Retries != 3 {
		t.Fatalf("unexpected probe defaults: %+v", config.Probe)
	}
	if config.Punch.StableTimeout != 2*time.Second || config.Punch.VariableTimeout != 15*time.Second || config.Punch.HelperCount != 256 {
		t.Fatalf("unexpected punch defaults: %+v", config.Punch)
	}
	if config.Server.ProbeBasePort != 10000 {
		t.Fatalf("unexpected probe base port: %d", config.Server.ProbeBasePort)
	}
	if config.TUN.MTU != 1456 {
		t.Fatalf("unexpected default mtu: %d", config.TUN.MTU)
	}
	if config.Tunnel.OutboundQueuePackets != 4096 || config.Tunnel.InboundQueuePackets != 4096 {
		t.Fatalf("unexpected queue defaults: %d/%d", config.Tunnel.OutboundQueuePackets, config.Tunnel.InboundQueuePackets)
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
	path := filepath.Join(t.TempDir(), "client.yaml")
	content := clientTemplate(t) + "\nprob:\n  timeout: 1s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClientConfig(path); err == nil {
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

// TestLoadClientConfigLoggingPaths 日志双文件路径校验：相同路径拒绝
// （两个流写同一文件会交错，语义不清）。
func TestLoadClientConfigLoggingPaths(t *testing.T) {
	if _, err := LoadClientConfig(writeConfig(t, "logging:\n  file: a.log\n  error_file: a.log\n")); err == nil {
		t.Fatal("identical log paths must be rejected")
	}
	if _, err := LoadClientConfig(writeConfig(t, "logging:\n  file: a.log\n  error_file: b.err\n")); err != nil {
		t.Fatalf("distinct log paths must be accepted: %v", err)
	}
}
