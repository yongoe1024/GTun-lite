package client

import (
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// testLog 返回静默日志。
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testManagerConfig 构造指向死端口的客户端配置：探测必然超时失败，
// 让单元测试不依赖任何网络依赖。
func testManagerConfig() ClientConfig {
	config := ClientConfig{}
	config.Server.Addr = "127.0.0.1:1"
	config.Server.ProbeBasePort = 1
	config.Identity.Name = "test"
	config.Control.HeartbeatInterval = time.Hour
	config.Control.RegisterTimeout = time.Second
	config.Control.ConnectTimeout = time.Second
	config.Control.ReconnectInterval = time.Hour
	config.Control.WriteTimeout = time.Second
	config.TUN.Name = "gtun0"
	config.TUN.MTU = 1280
	config.Tunnel.OutboundQueuePackets = 64
	config.Tunnel.InboundQueuePackets = 64
	config.Probe.Timeout = 50 * time.Millisecond
	config.Probe.PerPortTimeout = 20 * time.Millisecond
	config.Probe.Retries = 0
	config.Punch.StableTimeout = time.Second
	config.Punch.VariableTimeout = time.Second
	config.Punch.HelperCount = 256
	return config
}

// networkWithPeer 构造含一个对端的有效配置。
func networkWithPeer(t *testing.T, peering common.PeeringID) *common.NetworkConfig {
	t.Helper()
	return &common.NetworkConfig{
		Type: common.MessageNetworkConfig,
		Network: &common.NetworkDefinition{
			ID: "1234abcd", Name: "net", CIDR: "10.200.0.0/24", IP: "10.200.0.1",
			Peers: []common.NetworkPeer{{
				DeviceID: common.GenerateDeviceID(), PeeringID: peering,
				Name: "peer", IP: "10.200.0.2", Online: true,
			}},
		},
	}
}

// waitForFinished 轮询等待 Worker 置终结标志。
func waitForFinished(t *testing.T, worker *linkWorker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if worker.finished.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker did not finish")
}

// TestApplyConfigPrunesWorkers 配置收缩后，不在配置里的配对其 Worker 被停止并移除。
func TestApplyConfigPrunesWorkers(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.ApplyConfig(networkWithPeer(t, peering))
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: common.GenerateLinkToken(),
		PeeringID: peering, Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
	})
	worker := manager.workers[peering]
	if worker == nil {
		t.Fatal("worker should exist after connect")
	}

	manager.ApplyConfig(&common.NetworkConfig{Type: common.MessageNetworkConfig})
	if _, exists := manager.workers[peering]; exists {
		t.Fatal("worker must be pruned when peering leaves config")
	}
	waitForFinished(t, worker)
}

// TestDisconnectStopsWorker DISCONNECT 停止并移除 Worker。
func TestDisconnectStopsWorker(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.ApplyConfig(networkWithPeer(t, peering))
	token := common.GenerateLinkToken()
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: token, PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
	})
	worker := manager.workers[peering]

	manager.HandleDisconnect(&common.Disconnect{Type: common.MessageDisconnect, Token: token, PeeringID: peering})
	if _, exists := manager.workers[peering]; exists {
		t.Fatal("worker must be removed on disconnect")
	}
	waitForFinished(t, worker)
}

// TestReconnectReplacesWorker 同一配对再次 CONNECT 即重建：旧 Worker 停止、新 token 生效。
func TestReconnectReplacesWorker(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.ApplyConfig(networkWithPeer(t, peering))
	first := common.GenerateLinkToken()
	second := common.GenerateLinkToken()
	connect := &common.Connect{
		Type: common.MessageConnect, PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
	}
	connect.Token = first
	manager.HandleConnect(connect)
	original := manager.workers[peering]
	connect.Token = second
	manager.HandleConnect(connect)
	if manager.workers[peering].token != second {
		t.Fatalf("worker must carry the newest token, got %s", manager.workers[peering].token)
	}
	waitForFinished(t, original)
}

// TestStateReportShape 全量上报按配置枚举：Worker 存活报 CONNECTING 带 token，
// 无 Worker 报 IDLE；终结后的 Worker 同样报 IDLE。
func TestStateReportShape(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.ApplyConfig(networkWithPeer(t, peering))

	report := manager.StateReport()
	if !report.Full || len(report.Links) != 1 || report.Links[0].State != common.StateIdle || report.Links[0].Token != "" {
		t.Fatalf("unexpected report for idle link: %+v", report)
	}

	token := common.GenerateLinkToken()
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: token, PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
	})
	report = manager.StateReport()
	if report.Links[0].State != common.StateConnecting || report.Links[0].Token != token {
		t.Fatalf("unexpected report for connecting link: %+v", report.Links[0])
	}

	// Worker 终结后（失败但事件未被消费）：上报必须已经回到 IDLE——
	// 状态读自 Worker 原子标志，不依赖事件被处理。
	manager.workers[peering].finish()
	report = manager.StateReport()
	if report.Links[0].State != common.StateIdle || report.Links[0].Token != "" {
		t.Fatalf("finished worker must report IDLE: %+v", report.Links[0])
	}
}

// TestStateReportWithoutConfig 没收到过配置时上报为空快照，不是 nil。
func TestStateReportWithoutConfig(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	report := manager.StateReport()
	if !report.Full || report.Links == nil || len(report.Links) != 0 {
		t.Fatalf("expected empty full snapshot, got %+v", report)
	}
}

// TestPeerProfileDeliveredAndFiltered 对端画像投递给匹配的 Worker，
// token 不匹配的丢弃。
func TestPeerProfileDeliveredAndFiltered(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.ApplyConfig(networkWithPeer(t, peering))
	token := common.GenerateLinkToken()
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: token, PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
	})
	worker := manager.workers[peering]

	profile := common.NATProfile{NAT: common.NATStable, PublicIP: "1.2.3.4", Ports: fivePorts(40000)}
	manager.HandlePeerProfile(&common.PeerProfile{Type: common.MessagePeerProfile, Token: "000000000000", PeeringID: peering, Profile: profile})
	select {
	case <-worker.incoming:
		t.Fatal("stale-token profile must be dropped")
	default:
	}
	manager.HandlePeerProfile(&common.PeerProfile{Type: common.MessagePeerProfile, Token: token, PeeringID: peering, Profile: profile})
	select {
	case <-worker.incoming:
	default:
		t.Fatal("matching profile must be delivered")
	}
	worker.Stop()
}

// fivePorts 构造五个相同端口（stable 画像）。
func fivePorts(port common.Port) []common.Port {
	return []common.Port{port, port, port, port, port}
}

// TestIdentityCreateAndReload 身份文件首读生成、再读一致、坏内容报错。
func TestIdentityCreateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-id")

	first, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if !first.Valid() {
		t.Fatalf("generated identity invalid: %s", first)
	}
	second, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	if first != second {
		t.Fatalf("identity must persist, got %s then %s", first, second)
	}

	bad := filepath.Join(t.TempDir(), "bad-id")
	if err := os.WriteFile(bad, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(bad); err == nil {
		t.Fatal("invalid identity file must be rejected")
	}
}

// TestSameTUNTopology 拓扑比较：展示性字段（名称、在线标志）变化不触发重建，
// 拓扑字段（本机 IP、CIDR、对端集合）变化触发。
func TestSameTUNTopology(t *testing.T) {
	base := func() *common.NetworkConfig {
		return &common.NetworkConfig{
			Type: common.MessageNetworkConfig,
			Network: &common.NetworkDefinition{
				ID: "1234abcd", Name: "net", CIDR: "10.200.0.0/24", IP: "10.200.0.1",
				Peers: []common.NetworkPeer{{DeviceID: "11111111-1111-4111-8111-111111111111", PeeringID: "33333333333333333333333333333333", Name: "p", IP: "10.200.0.2", Online: true}},
			},
		}
	}
	if !sameTUNTopology(nil, nil) {
		t.Fatal("nil pair must be equal")
	}
	if sameTUNTopology(nil, base()) {
		t.Fatal("nil vs network must differ")
	}
	if !sameTUNTopology(base(), base()) {
		t.Fatal("identical configs must be equal")
	}
	// 在线标志与名称是展示性字段。
	toggled := base()
	toggled.Network.Peers[0].Online = false
	toggled.Network.Name = "renamed"
	if !sameTUNTopology(base(), toggled) {
		t.Fatal("cosmetic changes must not rebuild the stack")
	}
	// 本机 IP / 对端 IP / 对端集合变化是拓扑变化。
	moved := base()
	moved.Network.IP = "10.200.0.9"
	if sameTUNTopology(base(), moved) {
		t.Fatal("local IP change must rebuild")
	}
	peerGone := base()
	peerGone.Network.Peers = nil
	if sameTUNTopology(base(), peerGone) {
		t.Fatal("peer set change must rebuild")
	}
	peerMoved := base()
	peerMoved.Network.Peers[0].IP = "10.200.0.3"
	if sameTUNTopology(base(), peerMoved) {
		t.Fatal("peer IP change must rebuild")
	}
}

// TestDataStackOpenClose 假件上完整走一遍开栈/关栈。
func TestDataStackOpenClose(t *testing.T) {
	config := testManagerConfig()
	network := networkWithPeer(t, common.GeneratePeeringID()).Network
	opener := &fakeOpener{}
	stack, reason, err := openDataStack(opener, fakeRouteTable{}, config, network, "192.0.2.1", testLog())
	if err != nil || reason != "" {
		t.Fatalf("open stack: reason=%q err=%v", reason, err)
	}
	if len(opener.devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(opener.devices))
	}
	stack.deliverInbound([]byte("x")) // 不 panic 即可
	stack.close(testLog())
	select {
	case <-opener.devices[0].closed:
	default:
		t.Fatal("device must be closed with the stack")
	}
}

// TestConnectRejectedWhenStackNil 数据面未就绪时收到 CONNECT：不开 Worker，
// 立即回报失败；从未打开过栈时原因默认 TUN_CREATE_FAILED。
func TestConnectRejectedWhenStackNil(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, fakeRouteTable{}, testLog())
	peering := common.GeneratePeeringID()
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: common.GenerateLinkToken(), PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "p", IP: "10.200.0.2"},
	})
	select {
	case event := <-manager.Events():
		if event.Kind != WorkerFailed || event.PeeringID != peering || event.Reason != common.ReasonTUNCreateFailed {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("failure event missing")
	}
	if len(manager.workers) != 0 {
		t.Fatal("no worker should be started without a data stack")
	}
}

// conflictingRouteTable 是永远报本机地址冲突的路由表：驱动 preflight 失败路径。
type conflictingRouteTable struct {
	fakeRouteTable
}

func (conflictingRouteTable) LocalAddresses() ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("10.200.0.1")}, nil
}

// TestPreflightFailureReason preflight 冲突归因为 ROUTE_CONFLICT，
// 并在后续 CONNECT 回报里携带该真实原因。
func TestPreflightFailureReason(t *testing.T) {
	manager := NewManager(testManagerConfig(), common.GenerateDeviceID(), &fakeOpener{}, conflictingRouteTable{}, testLog())
	manager.ApplyConfig(networkWithPeer(t, common.GeneratePeeringID()))
	if manager.stack != nil || manager.stackFailureReason != common.ReasonRouteConflict {
		t.Fatalf("expected nil stack with ROUTE_CONFLICT, got stack=%v reason=%q", manager.stack != nil, manager.stackFailureReason)
	}
	peering := common.GeneratePeeringID()
	manager.HandleConnect(&common.Connect{
		Type: common.MessageConnect, Token: common.GenerateLinkToken(), PeeringID: peering,
		Peer: common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "p", IP: "10.200.0.2"},
	})
	select {
	case event := <-manager.Events():
		if event.Reason != common.ReasonRouteConflict {
			t.Fatalf("expected ROUTE_CONFLICT in report, got %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("failure event missing")
	}
}
