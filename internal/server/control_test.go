package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// testServer 是一台装配完整的测试服务端：真实 TCP 控制面、真实 SQLite、
// 真实管理 HTTP。集成测试走完整协议栈，不 mock 任何一层——
// 硬约束第 4 条要求全链路验证。
type testServer struct {
	hub     *Hub
	control *ControlServer
	admin   *httptest.Server
	store   *Store
	retry   *AutoRetry
}

// startTestServer 启动一台测试服务端，端口由内核分配。
// adjust 可选地修改最终配置（如压低会话容量），在 applyDefaults 之后执行。
func startTestServer(t *testing.T, adjust ...func(*ServerConfig)) *testServer {
	t.Helper()
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	config := ServerConfig{}
	config.applyDefaults()
	config.Control.Bind = "127.0.0.1:0"
	// 心跳超时压到亚秒级、注册超时压到 1s，让失活与超时用例不必真等。
	// 这里直接构造配置对象，绕过 LoadServerConfig 的「至少 40s」校验——
	// 那条校验约束的是生产配置里心跳与超时的比例关系，不是机制本身。
	config.Control.HeartbeatTimeout = 500 * time.Millisecond
	config.Control.RegisterTimeout = 1 * time.Second
	for _, mutate := range adjust {
		mutate(&config)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	owner := NewHub(store, config, log)
	control := NewControlServer(owner, config, log)
	if err := control.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = control.Serve(ctx) }()

	adminAPI := NewAdminAPI(owner, store, config, log)
	admin := httptest.NewServer(adminAPI.Routes())
	t.Cleanup(func() {
		cancel()
		admin.Close()
		owner.Close()
		_ = store.Close()
	})
	return &testServer{hub: owner, control: control, admin: admin, store: store, retry: adminAPI.retry}
}

// fakeClient 是协议级测试客户端：裸 TCP + JSON Lines，按需手工编造
// 任意上行消息。它模拟的是「诚实但行为可控的客户端」。
type fakeClient struct {
	t      *testing.T
	conn   net.Conn
	reader *common.LineReader
	device common.DeviceID
}

// dial 注册一台测试设备并完成注册握手，返回就绪的客户端。
func dial(t *testing.T, server *testServer, device common.DeviceID) *fakeClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.control.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	reader, err := common.NewLineReader(conn, common.MaxControlMessageBytes)
	if err != nil {
		t.Fatalf("line reader: %v", err)
	}
	client := &fakeClient{t: t, conn: conn, reader: reader, device: device}
	client.send(&common.DeviceRegister{
		Type: common.MessageDeviceRegister, DeviceID: device,
		Name: "dev-" + string(device)[:8], Platform: "linux",
	})
	client.read(2*time.Second, common.MessageDeviceRegistered)
	return client
}

// send 序列化并写一条上行消息。
func (client *fakeClient) send(message common.Message) {
	client.t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		client.t.Fatalf("marshal: %v", err)
	}
	_ = client.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.conn.Write(append(data, '\n')); err != nil {
		client.t.Fatalf("write: %v", err)
	}
}

// read 等待一条指定类型的消息，超时致命。跳过心跳等其他消息类型
// 之外的一切干扰（本测试里服务器不会主动发别的）。
func (client *fakeClient) read(timeout time.Duration, wantType string) common.Message {
	client.t.Helper()
	_ = client.conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		line, err := client.reader.ReadLine()
		if err != nil {
			client.t.Fatalf("read %s: %v", wantType, err)
		}
		decoded, err := common.DecodeMessage(line)
		if err != nil {
			client.t.Fatalf("decode: %v (%q)", err, line)
		}
		if decoded.MessageType() == wantType {
			return decoded
		}
	}
}

// reportFailure 发一条即时失败转场上报。
func (client *fakeClient) reportFailure(peering common.PeeringID, token common.LinkToken, reason common.Reason) {
	client.t.Helper()
	client.send(&common.StateReport{
		Type: common.MessageStateReport, Full: false,
		Links: []common.LinkReport{{PeeringID: peering, State: common.StateIdle, Token: token, Reason: reason}},
	})
}

// reportFull 发一条全量快照上报（重连与 QUERY 响应用同一路径）。
func (client *fakeClient) reportFull(links ...common.LinkReport) {
	client.t.Helper()
	client.send(&common.StateReport{Type: common.MessageStateReport, Full: true, Links: links})
}

// adminCall 调管理 API，返回状态码与响应体。
func adminCall(t *testing.T, server *testServer, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal admin body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, server.admin.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded
}

// fixtureNetwork 建网、拉两台成员、配对，返回网络 ID 与配对 ID。
func fixtureNetwork(t *testing.T, server *testServer, deviceA, deviceB common.DeviceID) (common.NetworkID, common.PeeringID) {
	t.Helper()
	status, body := adminCall(t, server, http.MethodPost, "/api/networks", map[string]string{"name": "testnet", "cidr": "10.200.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := common.NetworkID(body["id"].(string))
	for _, device := range []common.DeviceID{deviceA, deviceB} {
		status, body = adminCall(t, server, http.MethodPost, "/api/networks/"+string(network)+"/members", map[string]string{"device_id": string(device)})
		if status != http.StatusCreated {
			t.Fatalf("add member %s: %d %v", device, status, body)
		}
	}
	status, body = adminCall(t, server, http.MethodPost, "/api/networks/"+string(network)+"/peerings",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusCreated {
		t.Fatalf("create peering: %d %v", status, body)
	}
	return network, common.PeeringID(body["peering_id"].(string))
}

// linksView 拉取链路视图并找到目标设备对。
func linksView(t *testing.T, server *testServer, deviceA, deviceB common.DeviceID) map[string]any {
	t.Helper()
	status, body := adminCall(t, server, http.MethodGet, "/api/links", nil)
	if status != http.StatusOK {
		t.Fatalf("list links: %d %v", status, body)
	}
	// 视图里的设备对按字典序规范化（与库内配对一致），比较前同样规范化。
	pair, err := common.NewLink(deviceA, deviceB)
	if err != nil {
		t.Fatalf("normalize pair: %v", err)
	}
	for _, entry := range body["links"].([]any) {
		link := entry.(map[string]any)
		if link["device_a"] == string(pair[0]) && link["device_b"] == string(pair[1]) {
			return link
		}
	}
	return nil
}

// deviceOnline 查询设备当前在线性。
func deviceOnline(t *testing.T, server *testServer, device common.DeviceID) bool {
	t.Helper()
	status, body := adminCall(t, server, http.MethodGet, "/api/devices", nil)
	if status != http.StatusOK {
		t.Fatalf("list devices: %d %v", status, body)
	}
	for _, entry := range body["devices"].([]any) {
		row := entry.(map[string]any)
		if row["device_id"] == string(device) {
			return row["online"].(bool)
		}
	}
	return false
}

// waitOffline 轮询等待设备离线。
func waitOffline(t *testing.T, server *testServer, device common.DeviceID) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !deviceOnline(t, server, device) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("device %s still online", device)
}

// TestRegisterPushesConfig 注册即推全量配置，管理面可见设备在线。
func TestRegisterPushesConfig(t *testing.T) {
	server := startTestServer(t)
	client := dial(t, server, common.GenerateDeviceID())
	defer client.conn.Close()

	config := client.read(2*time.Second, common.MessageNetworkConfig).(*common.NetworkConfig)
	if config.Network != nil {
		t.Fatalf("fresh device should have empty config, got %+v", config.Network)
	}
	if !deviceOnline(t, server, client.device) {
		t.Fatal("registered device should be online")
	}
}

// TestConfigChangeRepushes 配置变更后在线设备收到新的全量配置。
func TestConfigChangeRepushes(t *testing.T) {
	server := startTestServer(t)
	device := common.GenerateDeviceID()
	peer := common.GenerateDeviceID()
	client := dial(t, server, device)
	defer client.conn.Close()
	client.read(2*time.Second, common.MessageNetworkConfig) // 首份空配置
	peerClient := dial(t, server, peer)                     // 设备必须先注册才能入网（外键）
	defer peerClient.conn.Close()
	peerClient.read(2*time.Second, common.MessageNetworkConfig)

	fixtureNetwork(t, server, device, peer)
	updated := client.read(2*time.Second, common.MessageNetworkConfig).(*common.NetworkConfig)
	if updated.Network == nil {
		t.Fatal("expected network config after joining network")
	}
	if updated.Network.CIDR != "10.200.0.0/24" {
		t.Fatalf("unexpected cidr %s", updated.Network.CIDR)
	}
}

// TestInvariantOneTCPDropKeepsLinkState 不变量 1（全链路）：TCP 断开后
// 链路状态逐字段不变，且随后的 DISCONNECT 因单侧离线被拒（不变量 2）。
func TestInvariantOneTCPDropKeepsLinkState(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	status, body := adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusAccepted {
		t.Fatalf("connect: %d %v", status, body)
	}
	connectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	connectB := clientB.read(2*time.Second, common.MessageConnect).(*common.Connect)
	if connectA.Token != connectB.Token {
		t.Fatalf("both sides must receive the same token")
	}

	// 任一侧上报成功 → CONNECTED。
	clientA.reportFull(common.LinkReport{PeeringID: peering, State: common.StateConnected, Token: connectA.Token})
	deadline := time.Now().Add(2 * time.Second)
	var before map[string]any
	for time.Now().Before(deadline) {
		before = linksView(t, server, deviceA, deviceB)
		if before != nil && before["state"] == "CONNECTED" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if before == nil || before["state"] != "CONNECTED" {
		t.Fatalf("link should be CONNECTED, got %v", before)
	}

	// 不变量 1：A 的控制连接断开，链路状态逐字段保持。
	_ = clientA.conn.Close()
	waitOffline(t, server, deviceA)
	after := linksView(t, server, deviceA, deviceB)
	if after == nil || after["state"] != before["state"] || after["token"] != before["token"] || after["updated_at"] != before["updated_at"] {
		t.Fatalf("link state changed after TCP drop:\nbefore=%v\nafter=%v", before, after)
	}

	// 不变量 2：单侧离线时 DISCONNECT 被拒，状态不动。
	status, body = adminCall(t, server, http.MethodPost, "/api/links/disconnect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusConflict || body["code"] != "peer_offline" {
		t.Fatalf("disconnect must be rejected with PEER_OFFLINE, got %d %v", status, body)
	}
	rejected := linksView(t, server, deviceA, deviceB)
	if rejected["state"] != "CONNECTED" || rejected["updated_at"] != before["updated_at"] {
		t.Fatalf("rejected operation must not touch state:\nbefore=%v\nafter=%v", before, rejected)
	}
	_ = clientB.conn.Close()
}

// TestInvariantTwoConnectRejectedWhenOffline 不变量 2（连接建立前）：
// 单侧离线时 CONNECT 返回 PEER_OFFLINE 且链路保持 IDLE。
func TestInvariantTwoConnectRejectedWhenOffline(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	defer clientA.conn.Close()
	// B 注册入网后断开连接：配对存在、B 离线——这正是「单侧离线」的现场。
	clientB := dial(t, server, deviceB)
	clientB.read(2*time.Second, common.MessageNetworkConfig)
	_ = clientB.conn.Close()
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)
	waitOffline(t, server, deviceB)

	status, body := adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusConflict || body["code"] != "peer_offline" {
		t.Fatalf("connect must be rejected with PEER_OFFLINE, got %d %v", status, body)
	}
	link := linksView(t, server, deviceA, deviceB)
	if link == nil || link["state"] != "IDLE" {
		t.Fatalf("link must stay IDLE, got %v", link)
	}
	_ = peering
}

// TestInvariantThreeSingleFailureCollapses 不变量 3（全链路）：单侧即时
// 失败上报即把链路打回 IDLE，不等另一侧。
func TestInvariantThreeSingleFailureCollapses(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	connectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)

	// 只让 A 报失败；B 保持沉默。
	clientA.reportFailure(peering, connectA.Token, common.ReasonPunchTimeout)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if link := linksView(t, server, deviceA, deviceB); link != nil && link["state"] == "IDLE" && link["token"] == "" {
			_ = clientA.conn.Close()
			_ = clientB.conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("single-side failure must collapse the link to IDLE")
}

// TestStaleFailureReportIgnored 旧尝试的迟到失败上报不得打断新尝试：
// token 不匹配即忽略。
func TestStaleFailureReportIgnored(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	first := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)

	// 针对旧 token 的失败上报被忽略，链路保持 CONNECTING。
	stale := common.LinkToken(strings.Repeat("ff", 6))
	clientA.reportFailure(peering, stale, common.ReasonPunchTimeout)
	time.Sleep(100 * time.Millisecond)
	link := linksView(t, server, deviceA, deviceB)
	if link == nil || link["state"] != "CONNECTING" || link["token"] != string(first.Token) {
		t.Fatalf("stale failure must be ignored, got %v", link)
	}
	_ = clientA.conn.Close()
	_ = clientB.conn.Close()
}

// TestOutsiderStateReportIgnored 配对之外的设备（哪怕是同网络里诚实注册的
// 第三台）不得改写他人链路状态——全量快照也不例外，身份与配对关系的
// 绑定是 token 守卫之外的另一半防线。
func TestOutsiderStateReportIgnored(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	outsider := dial(t, server, common.GenerateDeviceID()) // 在线但不在网络里
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	connect := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)

	// 第三台设备拿着窃得的 token 声称链路已建成：全量与转场两条路都必须被拒。
	outsider.reportFull(common.LinkReport{PeeringID: peering, State: common.StateConnected, Token: connect.Token})
	outsider.send(&common.StateReport{
		Type:  common.MessageStateReport,
		Links: []common.LinkReport{{PeeringID: peering, State: common.StateConnected, Token: connect.Token}},
	})
	time.Sleep(150 * time.Millisecond)
	link := linksView(t, server, deviceA, deviceB)
	if link == nil || link["state"] != "CONNECTING" || link["token"] != string(connect.Token) {
		t.Fatalf("outsider report must not touch the link, got %v", link)
	}
	_ = clientA.conn.Close()
	_ = clientB.conn.Close()
}

// TestOutsiderProfileReportIgnored 配对之外的画像不得顶占收集侧位：
// 没有这一守卫，A+外来者的两份「到齐」会把全零画像下发给真实对端。
func TestOutsiderProfileReportIgnored(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	outsider := dial(t, server, common.GenerateDeviceID())
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	connect := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)

	profile := common.NATProfile{NAT: common.NATStable, PublicIP: "203.0.113.9", Ports: []common.Port{40000, 40000, 40000, 40000, 40000}}
	clientA.send(&common.ProfileReport{Type: common.MessageProfileReport, PeeringID: peering, Token: connect.Token, Profile: profile})
	outsider.send(&common.ProfileReport{Type: common.MessageProfileReport, PeeringID: peering, Token: connect.Token, Profile: profile})
	// 外来画像若被收纳，两侧「到齐」会立刻下发 peer_profile；守卫生效时
	// 这 300ms 内 B 什么也收不到。
	_ = clientB.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := clientB.reader.ReadLine(); err == nil {
		t.Fatal("outsider profile must not complete the pairing")
	}

	// 真实对端的画像到达后配对才完成，双方各拿到对方的画像。
	clientB.send(&common.ProfileReport{Type: common.MessageProfileReport, PeeringID: peering, Token: connect.Token, Profile: profile})
	received := clientA.read(2*time.Second, common.MessagePeerProfile).(*common.PeerProfile)
	if received.Profile.NAT != common.NATStable || received.Profile.PublicIP != "203.0.113.9" {
		t.Fatalf("peer profile not delivered after the real counterpart reported, got %+v", received.Profile)
	}
	_ = clientA.conn.Close()
	_ = clientB.conn.Close()
}

// TestDisconnectIdleLinkStillDeliverable 对 IDLE 链路（从未建链，内存无
// token）下发拆链：发出的 Disconnect 必须通过协议校验。曾经把空 token
// 直接写上线，客户端 DecodeMessage 拒帧，两条控制会话双双断连。
func TestDisconnectIdleLinkStillDeliverable(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	fixtureNetwork(t, server, deviceA, deviceB)

	status, body := adminCall(t, server, http.MethodPost, "/api/links/disconnect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusAccepted {
		t.Fatalf("disconnect on idle link: %d %v", status, body)
	}
	// read 在解码失败时 fatal——消息不合法本测试就过不去。
	disconnectA := clientA.read(2*time.Second, common.MessageDisconnect).(*common.Disconnect)
	disconnectB := clientB.read(2*time.Second, common.MessageDisconnect).(*common.Disconnect)
	if !disconnectA.Token.Valid() || !disconnectB.Token.Valid() {
		t.Fatalf("placeholder tokens must be wire-valid, got %q / %q", disconnectA.Token, disconnectB.Token)
	}
	if disconnectA.Token != disconnectB.Token {
		t.Fatal("both sides must receive the same token")
	}
	link := linksView(t, server, deviceA, deviceB)
	if link == nil || link["state"] != "IDLE" {
		t.Fatalf("link must be IDLE after disconnect, got %v", link)
	}
	_ = clientA.conn.Close()
	_ = clientB.conn.Close()
}

// TestProbeReflectorBindFailureNoHang 绑定失败必须立刻返回错误，
// 且随后的 Close 不得挂死——done 在失败路径上同样要关闭。
func TestProbeReflectorBindFailureNoHang(t *testing.T) {
	occupied, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.LocalAddr().(*net.UDPAddr).Port

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reflector, err := NewProbeReflector("127.0.0.1", port, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- reflector.ListenAndServe(ctx) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("bind conflict must fail ListenAndServe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bind conflict was not reported")
	}
	closed := make(chan struct{})
	go func() { reflector.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung after a failed ListenAndServe")
	}
}

// TestMalformedLogRateLimit 每源 IP 窗口内只记一条；表满后新来源放弃日志
// 而不是撑大限流表。
func TestMalformedLogRateLimit(t *testing.T) {
	reflector := &ProbeReflector{malformed: make(map[string]time.Time)}
	if !reflector.shouldLogMalformed("192.0.2.1") {
		t.Fatal("the first datagram from a source must be logged")
	}
	if reflector.shouldLogMalformed("192.0.2.1") {
		t.Fatal("a repeat within the window must be suppressed")
	}
	if !reflector.shouldLogMalformed("192.0.2.2") {
		t.Fatal("a different source must not be suppressed by another source")
	}
	for i := 0; i < malformedLogCap; i++ {
		reflector.malformed[fmt.Sprintf("198.51.100.%d", i)] = time.Now()
	}
	if reflector.shouldLogMalformed("203.0.113.99") {
		t.Fatal("a new source must be dropped once the table is full")
	}
	// 已知来源的窗口过期后仍能继续被记录：限流表只挡新来源，不驱逐老来源。
	reflector.malformed["192.0.2.1"] = time.Now().Add(-2 * malformedLogWindow)
	if !reflector.shouldLogMalformed("192.0.2.1") {
		t.Fatal("a known source must remain tracked after the table fills")
	}
}

// TestReconnectFullReportRebuildsState 客户端重连全量上报即采信：
// 服务器链路状态与客户端事实不一致时，快照直接覆盖。
func TestReconnectFullReportRebuildsState(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	// 建链到 CONNECTED，然后 A 断开（模拟断连期间隧道自行恢复/失败）。
	adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	connectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)
	clientA.reportFull(common.LinkReport{PeeringID: peering, State: common.StateConnected, Token: connectA.Token})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if link := linksView(t, server, deviceA, deviceB); link != nil && link["state"] == "CONNECTED" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = clientA.conn.Close()
	waitOffline(t, server, deviceA)

	// A 重连。服务器此刻的状态是 CONNECTED；客户端的新事实是 IDLE
	//（断连期间隧道死了，尝试已清）。快照必须直接覆盖服务器的记录。
	reconnected := dial(t, server, deviceA)
	reconnected.read(2*time.Second, common.MessageNetworkConfig)
	reconnected.reportFull(common.LinkReport{PeeringID: peering, State: common.StateIdle})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if link := linksView(t, server, deviceA, deviceB); link != nil && link["state"] == "IDLE" && link["token"] == "" {
			_ = reconnected.conn.Close()
			_ = clientB.conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("full report must overwrite server-side state")
}

// TestQueryPullsClientState QUERY 下发到客户端，全量响应刷新链路视图。
func TestQueryPullsClientState(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	status, body := adminCall(t, server, http.MethodPost, "/api/devices/"+string(deviceA)+"/query", nil)
	if status != http.StatusAccepted {
		t.Fatalf("query: %d %v", status, body)
	}
	if query := clientA.read(2*time.Second, common.MessageQuery); query == nil {
		t.Fatal("client should receive query")
	}
	// 客户端响应 CONNECTING 快照（尝试进行中）。
	token := common.LinkToken(strings.Repeat("ab", 6))
	clientA.reportFull(common.LinkReport{PeeringID: peering, State: common.StateConnecting, Token: token})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if link := linksView(t, server, deviceA, deviceB); link != nil && link["state"] == "CONNECTING" && link["token"] == string(token) {
			_ = clientA.conn.Close()
			_ = clientB.conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("query response should refresh link view")
}

// TestQueryOfflineDeviceRejected 离线设备不可查询，如实拒绝。
func TestQueryOfflineDeviceRejected(t *testing.T) {
	server := startTestServer(t)
	status, body := adminCall(t, server, http.MethodPost, "/api/devices/"+string(common.GenerateDeviceID())+"/query", nil)
	if status != http.StatusConflict || body["code"] != "peer_offline" {
		t.Fatalf("query on offline device must be rejected, got %d %v", status, body)
	}
}

// TestDuplicateLoginReplacesSession 同设备重连顶替旧会话：
// 旧连接收到 duplicate_login 并被关闭，新连接成为当前会话。
func TestDuplicateLoginReplacesSession(t *testing.T) {
	server := startTestServer(t)
	device := common.GenerateDeviceID()
	first := dial(t, server, device)
	first.read(2*time.Second, common.MessageNetworkConfig)

	second := dial(t, server, device)
	second.read(2*time.Second, common.MessageNetworkConfig)

	// 旧连接收到顶替通知后由服务器关闭，读侧很快报错。
	notice := first.read(2*time.Second, common.MessageDuplicateLogin)
	if notice == nil {
		t.Fatal("old session must receive duplicate_login")
	}
	_ = first.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := first.reader.ReadLine(); err == nil {
		t.Fatal("old connection must be closed after duplicate_login")
	}
	// 新连接心跳仍被受理（在线性指向新会话）。
	second.send(&common.DeviceHeartbeat{Type: common.MessageDeviceHeartbeat})
	if !deviceOnline(t, server, device) {
		t.Fatal("replacing session must keep device online")
	}
	_ = second.conn.Close()
}

// TestHeartbeatTimeoutMarksOffline 静默连接在心跳超时后被判定离线。
func TestHeartbeatTimeoutMarksOffline(t *testing.T) {
	server := startTestServer(t) // 测试配置里超时是 500ms
	device := common.GenerateDeviceID()
	client := dial(t, server, device)
	client.read(2*time.Second, common.MessageNetworkConfig)
	// 不发心跳，等读超时触发会话终结。
	waitOffline(t, server, device)
	_ = client.conn.Close()
}

// TestConnectWithoutPeering 无配对的设备对不能建链。
func TestConnectWithoutPeering(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	status, body := adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusNotFound {
		t.Fatalf("connect without peering must 404, got %d %v", status, body)
	}
	_ = clientA.conn.Close()
	_ = clientB.conn.Close()
}

// TestConnectionLimitRejected 容量满时第二台设备注册被拒：
// 收到 server_full（而非 internal_error）后连接关闭，既有会话不受影响。
func TestConnectionLimitRejected(t *testing.T) {
	server := startTestServer(t, func(config *ServerConfig) { config.Control.MaxConnections = 1 })
	first := dial(t, server, common.GenerateDeviceID())
	defer first.conn.Close()
	first.read(2*time.Second, common.MessageNetworkConfig)

	conn, err := net.DialTimeout("tcp", server.control.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	second := &fakeClient{t: t, conn: conn}
	var initErr error
	second.reader, initErr = common.NewLineReader(conn, common.MaxControlMessageBytes)
	if initErr != nil {
		t.Fatalf("line reader: %v", initErr)
	}
	second.device = common.GenerateDeviceID()
	second.send(&common.DeviceRegister{
		Type: common.MessageDeviceRegister, DeviceID: second.device,
		Name: "second", Platform: "linux",
	})
	rejection := second.read(2*time.Second, common.MessageError).(*common.ErrorMessage)
	if rejection.Code != common.ErrorServerFull {
		t.Fatalf("expected server_full, got %s (%s)", rejection.Code, rejection.Message)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.reader.ReadLine(); err == nil {
		t.Fatal("connection must be closed after rejection")
	}
	if !deviceOnline(t, server, first.device) {
		t.Fatal("existing session must be unaffected")
	}
}

// TestRegisterTimeout 沉默连接在注册超时后被断开并收到错误消息。
func TestRegisterTimeout(t *testing.T) {
	server := startTestServer(t)
	conn, err := net.DialTimeout("tcp", server.control.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// 只连不注册。测试服务端注册超时是 1s，读到关闭即通过。
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 128)
	for {
		if _, err := conn.Read(buffer); err != nil {
			if strings.Contains(fmt.Sprint(err), "EOF") {
				return // 服务器写了 error 后关闭
			}
			return // 读超时/连接关闭都证明连接有界
		}
	}
}

// TestProbeReflectorEchoes 反射器把观察到的来源地址回显给探测者。
func TestProbeReflectorEchoes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reflector, err := NewProbeReflector("127.0.0.1", 0, log)
	if err != nil {
		t.Fatalf("create reflector: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = reflector.ListenAndServe(ctx) }()
	t.Cleanup(func() { cancel(); reflector.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for reflector.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if reflector.Addr() == nil {
		t.Fatal("reflector did not start")
	}

	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	nonce := "0123456789abcdef"
	request, _ := common.EncodeProbeRequest(common.ProbeRequest{Nonce: nonce, ProbeID: 3})
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: reflector.Addr().Port + 2}
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := socket.WriteToUDP(request, target); err != nil {
			t.Fatal(err)
		}
		_ = socket.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buffer := make([]byte, common.MaxProbeDatagram+1)
		length, source, err := socket.ReadFromUDP(buffer)
		if err != nil {
			continue
		}
		response, err := common.ParseProbeResponse(buffer[:length])
		if err != nil || source == nil || source.Port != target.Port || !source.IP.Equal(target.IP) || response.Nonce != nonce || response.ProbeID != 3 {
			continue
		}
		if response.PublicIP != "127.0.0.1" || response.MappedPort != common.Port(socket.LocalAddr().(*net.UDPAddr).Port) {
			t.Fatalf("unexpected echo: %+v", response)
		}
		return
	}
	t.Fatal("reflector did not echo a valid PORT response")
}

// TestDeleteDevice 删设备的三条路径：在网拒绝（先移出网络）、在线拒绝
// （先停客户端，否则重连 upsert 复活）、离线且不在网 → 删除成功。
func TestDeleteDevice(t *testing.T) {
	server := startTestServer(t)
	device := common.GenerateDeviceID()
	client := dial(t, server, device)
	defer client.conn.Close()
	client.read(2*time.Second, common.MessageNetworkConfig)

	// 在线 → 409 device_online。
	status, body := adminCall(t, server, http.MethodDelete, "/api/devices/"+string(device), nil)
	if status != http.StatusConflict || body["code"] != "device_online" {
		t.Fatalf("online device must be rejected, got %d %v", status, body)
	}
	// 断开后删除 → 200，且设备从列表消失。
	_ = client.conn.Close()
	waitOffline(t, server, device)
	status, body = adminCall(t, server, http.MethodDelete, "/api/devices/"+string(device), nil)
	if status != http.StatusOK {
		t.Fatalf("offline device must be deletable, got %d %v", status, body)
	}
	status, body = adminCall(t, server, http.MethodGet, "/api/devices", nil)
	for _, entry := range body["devices"].([]any) {
		if entry.(map[string]any)["device_id"] == string(device) {
			t.Fatal("deleted device must disappear from the list")
		}
	}
	// 再删一次 → 404。
	status, _ = adminCall(t, server, http.MethodDelete, "/api/devices/"+string(device), nil)
	if status != http.StatusNotFound {
		t.Fatalf("repeat delete must 404, got %d", status)
	}
}

// TestDeleteMemberDeviceRejected 在网设备删除被拒（先移出网络）。
func TestDeleteMemberDeviceRejected(t *testing.T) {
	server := startTestServer(t)
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	defer func() { _ = clientA.conn.Close(); _ = clientB.conn.Close() }()
	clientB.read(2*time.Second, common.MessageNetworkConfig)
	_ = clientB.conn.Close()
	waitOffline(t, server, deviceB)
	fixtureNetwork(t, server, deviceA, deviceB) // B 已注册入网

	status, body := adminCall(t, server, http.MethodDelete, "/api/devices/"+string(deviceB), nil)
	if status != http.StatusConflict || body["code"] != "still_member" {
		t.Fatalf("member device must be rejected with still_member, got %d %v", status, body)
	}
}

// TestReadyEndpoint /ready 返回 ok（数据库可达）。
func TestReadyEndpoint(t *testing.T) {
	server := startTestServer(t)
	status, body := adminCall(t, server, http.MethodGet, "/ready", nil)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("expected 200 ok, got %d %v", status, body)
	}
}

// TestAddMemberResponseComplete 入网 201 响应包含完整的成员行
// （device_name 与 joined_at 非空——曾经写漏为空值）。
func TestAddMemberResponseComplete(t *testing.T) {
	server := startTestServer(t)
	device := common.GenerateDeviceID()
	client := dial(t, server, device)
	defer client.conn.Close()
	client.read(2*time.Second, common.MessageNetworkConfig)

	status, body := adminCall(t, server, http.MethodPost, "/api/networks",
		map[string]string{"name": "resp", "cidr": "10.207.0.0/24"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := body["id"].(string)
	status, body = adminCall(t, server, http.MethodPost, "/api/networks/"+network+"/members",
		map[string]string{"device_id": string(device)})
	if status != http.StatusCreated {
		t.Fatalf("add member: %d %v", status, body)
	}
	if body["device_name"] == "" || body["joined_at"] == "" {
		t.Fatalf("member response must be complete, got %v", body)
	}
}
