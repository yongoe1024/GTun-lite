package client_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gtun-lite/internal/client"
	"gtun-lite/internal/common"
	"gtun-lite/internal/server"
	"gtun-lite/internal/tun"
)

// ---- 测试用假 TUN（外部包自带一份，实现 tun 的导出接口） ----

type fakeDevice struct {
	mu       sync.Mutex
	injected chan []byte
	written  [][]byte
	closed   chan struct{}
	once     sync.Once
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{injected: make(chan []byte, 64), closed: make(chan struct{})}
}

func (device *fakeDevice) Read(buffer []byte) (int, error) {
	select {
	case packet := <-device.injected:
		copy(buffer, packet)
		return len(packet), nil
	case <-device.closed:
		return 0, net.ErrClosed
	}
}

func (device *fakeDevice) Write(packet []byte) (int, error) {
	device.mu.Lock()
	defer device.mu.Unlock()
	select {
	case <-device.closed:
		return 0, net.ErrClosed
	default:
	}
	device.written = append(device.written, append([]byte(nil), packet...))
	return len(packet), nil
}

func (device *fakeDevice) Name() string { return "fake0" }
func (device *fakeDevice) MTU() int     { return 1500 }

func (device *fakeDevice) Close() error {
	device.once.Do(func() { close(device.closed) })
	return nil
}

func (device *fakeDevice) inject(packet []byte) {
	device.injected <- append([]byte(nil), packet...)
}

func (device *fakeDevice) takeWritten() [][]byte {
	device.mu.Lock()
	defer device.mu.Unlock()
	out := make([][]byte, len(device.written))
	copy(out, device.written)
	return out
}

// isClosed 加锁读关闭状态（设备指针跨协程传递，读取须与写方同步）。
func (device *fakeDevice) isClosed() bool {
	device.mu.Lock()
	defer device.mu.Unlock()
	select {
	case <-device.closed:
		return true
	default:
		return false
	}
}

// fakeOpener 记录打开的设备，测试经它拿到两端的假 TUN。
// 设备列表被客户端读循环 goroutine 写、测试 goroutine 读，须加锁。
type fakeOpener struct {
	mu      sync.Mutex
	devices []*fakeDevice
}

func (opener *fakeOpener) Open(_ context.Context, _ string, _ int, _ common.IPv4, _ []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	device := newFakeDevice()
	opener.mu.Lock()
	opener.devices = append(opener.devices, device)
	opener.mu.Unlock()
	return device, tun.NewRouteCleanup("fake0", nil, func() error { return nil }), nil
}

// lastDevice 返回当前（最后打开的）设备。
func (opener *fakeOpener) lastDevice() *fakeDevice {
	opener.mu.Lock()
	defer opener.mu.Unlock()
	if len(opener.devices) == 0 {
		return nil
	}
	return opener.devices[len(opener.devices)-1]
}

// takeDevices 加锁读设备列表长度/内容。
func (opener *fakeOpener) takeDevices() []*fakeDevice {
	opener.mu.Lock()
	defer opener.mu.Unlock()
	out := make([]*fakeDevice, len(opener.devices))
	copy(out, opener.devices)
	return out
}

// fakeRouteTable 报告一张空路由表。
type fakeRouteTable struct{}

func (fakeRouteTable) DefaultGateway() (netip.Addr, bool, error)  { return netip.Addr{}, false, nil }
func (fakeRouteTable) LocalAddresses() ([]netip.Addr, error)      { return nil, nil }
func (fakeRouteTable) HasHostRoute(netip.Addr) (bool, error)      { return false, nil }
func (fakeRouteTable) HostRouteDangling(netip.Addr) (bool, error) { return false, nil }
func (fakeRouteTable) DeleteHostRoute(netip.Addr) error           { return nil }

// ipv4Packet 构造带正确校验和的 20 字节头 IPv4 包（数据面出站校验要求）。
func ipv4Packet(src, dst string, payload []byte) []byte {
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(20+len(payload)))
	packet[8] = 64 // TTL
	packet[9] = 1  // ICMP
	srcIP := net.ParseIP(src).To4()
	dstIP := net.ParseIP(dst).To4()
	copy(packet[12:16], srcIP)
	copy(packet[16:20], dstIP)
	copy(packet[20:], payload)
	// 一补和校验：按 16 位字累加取反。
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
	return packet
}

// startServer 启动一台真实服务端（控制面 + 管理面 + 探测反射器 + SQLite），
// 端口由内核分配。与 server 包自身的测试共用同一套生产代码路径。
func startServer(t *testing.T) (controlAddr string, adminURL string, probePort int) {
	t.Helper()
	store, err := server.OpenStore(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	config := server.ServerConfig{}
	config.Control.Bind = "127.0.0.1:0"
	config.Admin.Bind = "127.0.0.1:0"
	config.Control.RegisterTimeout = 5 * time.Second
	config.Control.HeartbeatTimeout = 60 * time.Second
	config.Control.WriteTimeout = 2 * time.Second
	config.Control.MaxConnections = 100
	config.Limits.MaxDevicesPerNetwork = 8
	config.Limits.MinCIDRPrefix = 24
	config.Limits.MaxCIDRPrefix = 28
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := server.NewHub(store, config, log)
	control := server.NewControlServer(owner, config, log)
	if err := control.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = control.Serve(ctx) }()
	// 探测反射器：base_port=0 让内核分配连续 5 端口的首端口。
	reflector, err := server.NewProbeReflector("127.0.0.1", 0, log)
	if err != nil {
		t.Fatalf("create reflector: %v", err)
	}
	go func() { _ = reflector.ListenAndServe(ctx) }()
	t.Cleanup(reflector.Close)
	deadline := time.Now().Add(2 * time.Second)
	for reflector.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if reflector.Addr() == nil {
		t.Fatal("probe reflector did not start")
	}

	admin := httptest.NewServer(server.NewAdminAPI(owner, store, config, log).Routes())
	t.Cleanup(func() {
		cancel()
		admin.Close()
		owner.Close()
		_ = store.Close()
	})
	return control.Addr().String(), admin.URL, reflector.Addr().Port
}

// clientConfig 构造测试客户端配置：心跳、重连与打洞时序都压到亚秒级。
func clientConfig(addr string, probePort int, identityPath string) client.ClientConfig {
	config := client.ClientConfig{}
	config.Server.Addr = addr
	config.Server.ProbeBasePort = probePort
	config.Identity.Path = identityPath
	config.Identity.Name = "test-device"
	config.Control.HeartbeatInterval = 100 * time.Millisecond
	config.Control.RegisterTimeout = 2 * time.Second
	config.Control.ConnectTimeout = 500 * time.Millisecond
	config.Control.ReconnectInterval = 100 * time.Millisecond
	config.Control.WriteTimeout = 2 * time.Second
	config.TUN.Name = "gtun0"
	config.TUN.MTU = 1280
	config.Tunnel.OutboundQueuePackets = 1024
	config.Tunnel.InboundQueuePackets = 1024
	config.Probe.Timeout = 2 * time.Second
	config.Probe.PerPortTimeout = 500 * time.Millisecond
	config.Probe.Retries = 1
	config.Punch.StableTimeout = 5 * time.Second
	config.Punch.VariableTimeout = 5 * time.Second
	config.Punch.HelperCount = 256
	config.Logging.Level = "info"
	return config
}

// runClient 在后台 goroutine 里跑一个真实客户端，TUN 走测试假件。
func runClient(t *testing.T, config client.ClientConfig, identity string, opener *fakeOpener) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := client.NewManager(config, common.DeviceID(identity), opener, fakeRouteTable{}, log, nil)
	control := client.NewControlClient(config, manager, common.DeviceID(identity), log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- control.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		manager.Close()
	})
	return done
}

// admin 调管理 API。
func admin(t *testing.T, method, url, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, url+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded
}

// deviceOnline 轮询管理面直到设备在线。
func waitOnline(t *testing.T, adminURL, device string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := admin(t, http.MethodGet, adminURL, "/api/devices", nil)
		for _, entry := range body["devices"].([]any) {
			row := entry.(map[string]any)
			if row["device_id"] == device && row["online"].(bool) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("device %s never came online", device)
}

// approveDevice 把设备审批落库（注册审批制：注册不落库，入网前必须批准）。
func approveDevice(t *testing.T, adminURL, device string) {
	t.Helper()
	status, body := admin(t, http.MethodPost, adminURL, "/api/devices/"+device+"/approve", nil)
	if status != http.StatusOK {
		t.Fatalf("approve device %s: %d %v", device, status, body)
	}
}

// linkState 轮询链路视图直到达到期望状态。
func waitLinkState(t *testing.T, adminURL, deviceA, deviceB, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := admin(t, http.MethodGet, adminURL, "/api/links", nil)
		for _, entry := range body["links"].([]any) {
			link := entry.(map[string]any)
			if (link["device_a"] == deviceA && link["device_b"] == deviceB) ||
				(link["device_a"] == deviceB && link["device_b"] == deviceA) {
				if link["state"] == want {
					return link
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("link %s/%s never reached %s", deviceA, deviceB, want)
	return nil
}

// TestEndToEndPunch 全链路真打洞：两台真实客户端注册入网配对，管理面
// 下发 CONNECT，双方经探测反射器完成 stable 画像，三步握手后链路 CONNECTED；
// 再下发 DISCONNECT 拆回 IDLE。这是 Phase 3 的验收用例。
func TestEndToEndPunch(t *testing.T) {
	controlAddr, adminURL, probePort := startServer(t)
	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	openerA := &fakeOpener{}
	openerB := &fakeOpener{}
	runClient(t, clientConfig(controlAddr, probePort, "a"), string(idA), openerA)
	runClient(t, clientConfig(controlAddr, probePort, "b"), string(idB), openerB)
	waitOnline(t, adminURL, string(idA))
	waitOnline(t, adminURL, string(idB))

	approveDevice(t, adminURL, string(idA))
	approveDevice(t, adminURL, string(idB))
	status, body := admin(t, http.MethodPost, adminURL, "/api/networks", map[string]string{"name": "e2e", "cidr": "10.201.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	for _, device := range []string{string(idA), string(idB)} {
		status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/members", map[string]string{"device_id": device})
		if status != http.StatusCreated {
			t.Fatalf("add member: %d %v", status, body)
		}
	}
	status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/peerings",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusCreated {
		t.Fatalf("create peering: %d %v", status, body)
	}

	status, body = admin(t, http.MethodPost, adminURL, "/api/links/connect",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusAccepted {
		t.Fatalf("connect: %d %v", status, body)
	}
	// localhost 无 NAT：双方画像都是 stable，直连打洞 + 三步握手，
	// 双方上报成功后链路应到 CONNECTED（CONNECTING 窗口可能只有几十毫秒，
	// 不对它做断言，避免时序抖动）。
	link := waitLinkState(t, adminURL, string(idA), string(idB), "CONNECTED")
	if link["token"] == "" {
		t.Fatal("connected link must carry a token")
	}

	// QUERY 拉取双方快照，事实仍是 CONNECTED。
	for _, device := range []string{string(idA), string(idB)} {
		status, body = admin(t, http.MethodPost, adminURL, "/api/devices/"+device+"/query", nil)
		if status != http.StatusAccepted {
			t.Fatalf("query %s: %d %v", device, status, body)
		}
	}
	waitLinkState(t, adminURL, string(idA), string(idB), "CONNECTED")

	// 拆链：双方在线，DISCONNECT 应被受理并回到 IDLE。
	status, body = admin(t, http.MethodPost, adminURL, "/api/links/disconnect",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusAccepted {
		t.Fatalf("disconnect: %d %v", status, body)
	}
	waitLinkState(t, adminURL, string(idA), string(idB), "IDLE")
}

// TestClientKeepsReconnecting 客户端连不上时按固定间隔持续重连，
// 不退出、不报错；随后指向活服务器即正常注册上线。
func TestClientKeepsReconnecting(t *testing.T) {
	deadAddr := "127.0.0.1:1" // 端口 1 无人监听，拨号立即被拒
	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	done := runClient(t, clientConfig(deadAddr, 1, "a"), string(idA), &fakeOpener{})

	// 数秒内客户端不得退出（重连循环是常态路径，不是错误）。
	select {
	case err := <-done:
		t.Fatalf("client must keep reconnecting, exited with %v", err)
	case <-time.After(1 * time.Second):
	}

	// 指向活服务器的客户端正常上线，验证恢复路径完整。
	liveAddr, liveAdmin, liveProbe := startServer(t)
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	runClient(t, clientConfig(liveAddr, liveProbe, "b"), string(idB), &fakeOpener{})
	waitOnline(t, liveAdmin, string(idB))
}

// waitForDevice 轮询等待客户端打开 TUN（配置下发后），返回当时最新设备。
// 注意：取到后客户端仍可能因配置推送重建栈；需要稳定设备时在 CONNECTED
// 之后用 currentDevice 取最终实例。
func waitForDevice(t *testing.T, opener *fakeOpener) *fakeDevice {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if device := opener.lastDevice(); device != nil {
			return device
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client did not open its tun device")
	return nil
}

// currentDevice 返回 opener 当前（最后打开的）设备；CONNECTED 之后配置
// 不再重建栈，此时取到的即数据面正在使用的设备。
func currentDevice(t *testing.T, opener *fakeOpener) *fakeDevice {
	t.Helper()
	if device := opener.lastDevice(); device != nil {
		return device
	}
	t.Fatal("no device opened")
	return nil
}

// waitWritten 轮询等待设备收到含指定载荷的包，返回完整包。
func waitWritten(t *testing.T, device *fakeDevice, marker string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, packet := range device.takeWritten() {
			if bytes.Contains(packet, []byte(marker)) {
				return packet
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("device never received a packet containing %q", marker)
	return nil
}

// TestEndToEndDataPath 全链路数据面验收：假 TUN 注入 → 出站校验 → GTUN 帧
// 编码 → 真实 UDP 直连 → 对端入站验证链 → 对端假 TUN 收到，双向各验一次。
// 隧道在 CONNECTED 后由管理面注册数据链路，此用例覆盖 Phase 4 全部环节。
func TestEndToEndDataPath(t *testing.T) {
	controlAddr, adminURL, probePort := startServer(t)
	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	openerA := &fakeOpener{}
	openerB := &fakeOpener{}
	runClient(t, clientConfig(controlAddr, probePort, "a"), string(idA), openerA)
	runClient(t, clientConfig(controlAddr, probePort, "b"), string(idB), openerB)
	waitOnline(t, adminURL, string(idA))
	waitOnline(t, adminURL, string(idB))

	approveDevice(t, adminURL, string(idA))
	approveDevice(t, adminURL, string(idB))
	status, body := admin(t, http.MethodPost, adminURL, "/api/networks", map[string]string{"name": "data", "cidr": "10.202.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	// 成员加入顺序决定虚拟 IP：A 先入网拿 .1，B 拿 .2。
	for _, device := range []string{string(idA), string(idB)} {
		status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/members", map[string]string{"device_id": device})
		if status != http.StatusCreated {
			t.Fatalf("add member: %d %v", status, body)
		}
	}
	status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/peerings",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusCreated {
		t.Fatalf("create peering: %d %v", status, body)
	}
	waitForDevice(t, openerA)
	waitForDevice(t, openerB)

	status, _ = admin(t, http.MethodPost, adminURL, "/api/links/connect",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusAccepted {
		t.Fatalf("connect: %d", status)
	}
	waitLinkState(t, adminURL, string(idA), string(idB), "CONNECTED")
	// CONNECTED 后配置拓扑不再变化，取最终设备做数据面断言。
	deviceA := currentDevice(t, openerA)
	deviceB := currentDevice(t, openerB)

	// A → B：向 A 的 TUN 注入 src=.1 dst=.2 的包，断言从 B 的 TUN 收到。
	forward := ipv4Packet("10.202.0.1", "10.202.0.2", []byte("payload-a-to-b"))
	deviceA.inject(forward)
	received := waitWritten(t, deviceB, "payload-a-to-b")
	if !bytes.Equal(received, forward) {
		t.Fatalf("packet mutated in transit:\nsent %x\ngot  %x", forward, received)
	}

	// B → A 反向。
	backward := ipv4Packet("10.202.0.2", "10.202.0.1", []byte("payload-b-to-a"))
	deviceB.inject(backward)
	received = waitWritten(t, deviceA, "payload-b-to-a")
	if !bytes.Equal(received, backward) {
		t.Fatalf("packet mutated in transit:\nsent %x\ngot  %x", backward, received)
	}
}

// TestConfigSwapRebuildsStack 配置拓扑变化（移出网络）时数据面栈关闭、
// Worker 全停；再入网时重新打开。
func TestConfigSwapRebuildsStack(t *testing.T) {
	controlAddr, adminURL, probePort := startServer(t)
	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	openerA := &fakeOpener{}
	runClient(t, clientConfig(controlAddr, probePort, "a"), string(idA), openerA)
	runClient(t, clientConfig(controlAddr, probePort, "b"), string(idB), &fakeOpener{})
	waitOnline(t, adminURL, string(idA))
	waitOnline(t, adminURL, string(idB))

	approveDevice(t, adminURL, string(idA))
	status, body := admin(t, http.MethodPost, adminURL, "/api/networks", map[string]string{"name": "swap", "cidr": "10.203.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	status, _ = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/members", map[string]string{"device_id": string(idA)})
	if status != http.StatusCreated {
		t.Fatalf("add member: %d", status)
	}
	first := waitForDevice(t, openerA)

	// 移出网络：客户端应收到空配置并关闭数据面栈（设备 Close）。
	status, _ = admin(t, http.MethodDelete, adminURL, "/api/networks/"+network+"/members/"+string(idA), nil)
	if status != http.StatusOK {
		t.Fatalf("remove member: %d", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if first.isClosed() {
			return // 栈已按预期关闭
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("data stack was not closed after leaving the network")
}

// TestTunnelSurvivesTopologyChange 三设备场景：A-B 隧道 CONNECTED 后，
// 新建 B-C 配对触发 B 的栈重建——A-B 隧道必须幸存（幸存者重挂接），
// 且数据面在重建后无需重连即可继续收发。
func TestTunnelSurvivesTopologyChange(t *testing.T) {
	controlAddr, adminURL, probePort := startServer(t)
	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	idC, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "c"))
	openerA := &fakeOpener{}
	openerB := &fakeOpener{}
	runClient(t, clientConfig(controlAddr, probePort, "a"), string(idA), openerA)
	runClient(t, clientConfig(controlAddr, probePort, "b"), string(idB), openerB)
	runClient(t, clientConfig(controlAddr, probePort, "c"), string(idC), &fakeOpener{})
	waitOnline(t, adminURL, string(idA))
	waitOnline(t, adminURL, string(idB))
	waitOnline(t, adminURL, string(idC))

	approveDevice(t, adminURL, string(idA))
	approveDevice(t, adminURL, string(idB))
	approveDevice(t, adminURL, string(idC))
	status, body := admin(t, http.MethodPost, adminURL, "/api/networks", map[string]string{"name": "tri", "cidr": "10.204.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	for _, device := range []string{string(idA), string(idB), string(idC)} {
		status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/members", map[string]string{"device_id": device})
		if status != http.StatusCreated {
			t.Fatalf("add member: %d %v", status, body)
		}
	}
	status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/peerings",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusCreated {
		t.Fatalf("create peering: %d %v", status, body)
	}

	status, _ = admin(t, http.MethodPost, adminURL, "/api/links/connect",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusAccepted {
		t.Fatalf("connect: %d", status)
	}
	waitLinkState(t, adminURL, string(idA), string(idB), "CONNECTED")
	rebuildsBefore := len(openerB.takeDevices())

	// 新建 B-C 配对：B 的配置新增对端 C，触发 B 整栈重建。
	status, body = admin(t, http.MethodPost, adminURL, "/api/networks/"+network+"/peerings",
		map[string]string{"device_a": string(idB), "device_b": string(idC)})
	if status != http.StatusCreated {
		t.Fatalf("create peering B-C: %d %v", status, body)
	}
	// 等 B 重建（设备多开一个）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(openerB.takeDevices()) > rebuildsBefore {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(openerB.takeDevices()) <= rebuildsBefore {
		t.Fatal("B did not rebuild its stack after new peering")
	}
	// 给重挂接一点时间，然后验证 A-B 隧道数据面仍然工作（无需重连）。
	time.Sleep(100 * time.Millisecond)
	deviceA := currentDevice(t, openerA)
	deviceB := currentDevice(t, openerB)
	forward := ipv4Packet("10.204.0.1", "10.204.0.2", []byte("survivor-a-to-b"))
	deviceA.inject(forward)
	received := waitWritten(t, deviceB, "survivor-a-to-b")
	if !bytes.Equal(received, forward) {
		t.Fatalf("packet mutated after rebuild:\nsent %x\ngot  %x", forward, received)
	}
	backward := ipv4Packet("10.204.0.2", "10.204.0.1", []byte("survivor-b-to-a"))
	deviceB.inject(backward)
	received = waitWritten(t, deviceA, "survivor-b-to-a")
	if !bytes.Equal(received, backward) {
		t.Fatalf("packet mutated after rebuild:\nsent %x\ngot  %x", backward, received)
	}
	// 链路状态保持 CONNECTED（没有因重建掉回 IDLE）。
	waitLinkState(t, adminURL, string(idA), string(idB), "CONNECTED")
}

// restartableServer 是可整体关闭再原端口重启的服务端（真机 P0 场景
// 「服务器重启」的进程内等价物：同一份 SQLite，新的 hub/内存状态）。
type restartableServer struct {
	store    *server.Store
	config   server.ServerConfig
	log      *slog.Logger
	cancel   context.CancelFunc
	addr     string
	adminURL string
	probeURL int
}

func startRestartableServer(t *testing.T, store *server.Store) *restartableServer {
	t.Helper()
	config := server.ServerConfig{}
	config.Control.Bind = "127.0.0.1:0"
	config.Admin.Bind = "127.0.0.1:1" // 0 会随机，改用 httptest 实例接管
	config.Control.RegisterTimeout = 2 * time.Second
	config.Control.HeartbeatTimeout = 60 * time.Second
	config.Control.WriteTimeout = 2 * time.Second
	config.Control.MaxConnections = 100
	config.Limits.MaxDevicesPerNetwork = 8
	config.Limits.MinCIDRPrefix = 24
	config.Limits.MaxCIDRPrefix = 28
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	instance := &restartableServer{store: store, config: config, log: log}
	instance.start(t)
	return instance
}

// start 启动一套全新的控制面/反射器/管理面（hub 内存状态为空）。
// 「重启」必须复用首次的端口：客户端按固定地址重连，换了端口等于换了服务器。
func (rs *restartableServer) start(t *testing.T) {
	t.Helper()
	if rs.addr != "" {
		rs.config.Control.Bind = rs.addr
	}
	probeBase := 0
	if rs.probeURL != 0 {
		probeBase = rs.probeURL
	}
	owner := server.NewHub(rs.store, rs.config, rs.log)
	control := server.NewControlServer(owner, rs.config, rs.log)
	if err := control.Listen(); err != nil {
		t.Fatalf("listen control: %v", err)
	}
	reflector, err := server.NewProbeReflector("127.0.0.1", probeBase, rs.log)
	if err != nil {
		t.Fatalf("create reflector: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = control.Serve(ctx) }()
	go func() { _ = reflector.ListenAndServe(ctx) }()
	admin := httptest.NewServer(server.NewAdminAPI(owner, store0(rs), rs.config, rs.log).Routes())
	deadline := time.Now().Add(2 * time.Second)
	for reflector.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if reflector.Addr() == nil {
		t.Fatal("reflector did not start")
	}
	rs.cancel = func() {
		cancel()
		reflector.Close()
		admin.Close()
		owner.Close()
	}
	rs.addr = control.Addr().String()
	rs.adminURL = admin.URL
	rs.probeURL = reflector.Addr().Port
}

// store0 占位：管理 API 需要同一 store 实例。
func store0(rs *restartableServer) *server.Store { return rs.store }

// shutdown 关停当前实例（监听端口随关闭释放，可原端口重启）。
func (rs *restartableServer) shutdown() { rs.cancel() }

// TestServerRestartTunnelSurvivesAndRebuilt 真机验收 P0 两项的进程内等价：
//  1. 杀掉服务器（控制面+反射器+管理面全部关闭）后，已建成隧道的数据面
//     必须继续双向收发——隧道是 P2P 的，不经过服务器。
//  2. 服务器「重启」（同一数据库、全新内存状态）后，客户端固定间隔重连、
//     全量上报，链路视图必须恢复为客户端实测的 CONNECTED。
func TestServerRestartTunnelSurvivesAndRebuilt(t *testing.T) {
	store, err := server.OpenStore(context.Background(), filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rs := startRestartableServer(t, store)

	idA, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "a"))
	idB, _ := client.LoadIdentity(filepath.Join(t.TempDir(), "b"))
	openerA := &fakeOpener{}
	openerB := &fakeOpener{}
	runClientAt(t, rs.addr, rs.probeURL, "a", string(idA), openerA)
	runClientAt(t, rs.addr, rs.probeURL, "b", string(idB), openerB)
	waitOnline(t, rs.adminURL, string(idA))
	waitOnline(t, rs.adminURL, string(idB))

	approveDevice(t, rs.adminURL, string(idA))
	approveDevice(t, rs.adminURL, string(idB))
	status, body := admin(t, http.MethodPost, rs.adminURL, "/api/networks", map[string]string{"name": "p0", "cidr": "10.205.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	for _, device := range []string{string(idA), string(idB)} {
		status, body = admin(t, http.MethodPost, rs.adminURL, "/api/networks/"+network+"/members", map[string]string{"device_id": device})
		if status != http.StatusCreated {
			t.Fatalf("add member: %d %v", status, body)
		}
	}
	status, _ = admin(t, http.MethodPost, rs.adminURL, "/api/networks/"+network+"/peerings",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusCreated {
		t.Fatalf("create peering: %d", status)
	}
	status, _ = admin(t, http.MethodPost, rs.adminURL, "/api/links/connect",
		map[string]string{"device_a": string(idA), "device_b": string(idB)})
	if status != http.StatusAccepted {
		t.Fatalf("connect: %d", status)
	}
	waitLinkState(t, rs.adminURL, string(idA), string(idB), "CONNECTED")

	// 数据面基线：双向通。
	deviceA := currentDevice(t, openerA)
	deviceB := currentDevice(t, openerB)
	deviceA.inject(ipv4Packet("10.205.0.1", "10.205.0.2", []byte("before-kill")))
	waitWritten(t, deviceB, "before-kill")

	// P0-1：杀服务器。隧道与两端客户端进程毫无感知地继续工作。
	rs.shutdown()
	time.Sleep(200 * time.Millisecond)
	deviceA.inject(ipv4Packet("10.205.0.1", "10.205.0.2", []byte("after-kill")))
	waitWritten(t, deviceB, "after-kill")
	deviceB.inject(ipv4Packet("10.205.0.2", "10.205.0.1", []byte("reverse-after-kill")))
	waitWritten(t, deviceA, "reverse-after-kill")

	// P0-2：服务器重启（同库新进程语义）。客户端重连后全量上报，
	// 新的内存链路状态按客户端实测恢复为 CONNECTED。
	rs.start(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, devices := admin(t, http.MethodGet, rs.adminURL, "/api/devices", nil)
		online := 0
		if devices != nil {
			for _, entry := range devices["devices"].([]any) {
				if row, ok := entry.(map[string]any); ok && row["online"].(bool) {
					online++
				}
			}
		}
		if online == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	link := waitLinkState(t, rs.adminURL, string(idA), string(idB), "CONNECTED")
	if link["token"] == "" {
		t.Fatal("rebuilt link must carry the client-reported token")
	}
	// 隧道数据面在整场重启中始终未断。
	deviceA = currentDevice(t, openerA)
	deviceB = currentDevice(t, openerB)
	deviceA.inject(ipv4Packet("10.205.0.1", "10.205.0.2", []byte("after-restart")))
	waitWritten(t, deviceB, "after-restart")
}

// runClientAt 在指定地址跑一个真实客户端（restartable 场景用）。
func runClientAt(t *testing.T, controlAddr string, probePort int, identityPath, identity string, opener *fakeOpener) <-chan error {
	t.Helper()
	config := clientConfig(controlAddr, probePort, identityPath)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := client.NewManager(config, common.DeviceID(identity), opener, fakeRouteTable{}, log, nil)
	control := client.NewControlClient(config, manager, common.DeviceID(identity), log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- control.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		manager.Close()
	})
	return done
}
