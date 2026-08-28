package client

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// responses 构造五份回显。
func responses(ip string, ports ...common.Port) []common.ProbeResponse {
	out := make([]common.ProbeResponse, len(ports))
	for i, port := range ports {
		out[i] = common.ProbeResponse{PublicIP: common.IPv4(ip), MappedPort: port}
	}
	return out
}

// TestClassifyProfileStable 五端口全等 → stable。
func TestClassifyProfileStable(t *testing.T) {
	profile, reason := classifyProfile(responses("203.0.113.7", 40000, 40000, 40000, 40000, 40000), nil)
	if reason != "" || profile.NAT != common.NATStable {
		t.Fatalf("expected stable, got %q reason %q", profile.NAT, reason)
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("profile invalid: %v", err)
	}
}

// TestClassifyProfileVariable 端口不全等 → variable。
func TestClassifyProfileVariable(t *testing.T) {
	profile, reason := classifyProfile(responses("203.0.113.7", 40000, 40001, 40002, 40003, 40004), nil)
	if reason != "" || profile.NAT != common.NATVariable {
		t.Fatalf("expected variable, got %q reason %q", profile.NAT, reason)
	}
}

// TestClassifyProfileIPChanged 公网 IP 不一致 → PROBE_IP_CHANGED。
// 家宽按流轮换出口 IP 时端口画像没有意义，必须拒绝而不是拿去打洞。
func TestClassifyProfileIPChanged(t *testing.T) {
	samples := responses("203.0.113.7", 40000, 40000, 40000, 40000, 40000)
	samples[4].PublicIP = "198.51.100.9"
	if _, reason := classifyProfile(samples, nil); reason != common.ReasonProbeIPChanged {
		t.Fatalf("expected PROBE_IP_CHANGED, got %q", reason)
	}
}

// TestPruneHelpersKeepsSelected 建成收缩：只保留 commit 选中的 helper，
// 其余关闭（被关 socket 再写必错），选中项仍可写。
func TestPruneHelpersKeepsSelected(t *testing.T) {
	sockets, err := CreateHelpers(3, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, socket := range sockets {
			_ = socket.Close()
		}
	}()
	worker := &linkWorker{log: testLog()}
	worker.helpers = []helperSocket{
		{socket: sockets[0], id: "a"}, {socket: sockets[1], id: "b"}, {socket: sockets[2], id: "c"},
	}
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

	worker.pruneHelpers(sockets[1])

	if len(worker.helpers) != 1 || worker.helpers[0].id != "b" {
		t.Fatalf("expected only the selected helper kept, got %v", worker.helpers)
	}
	if _, err := sockets[0].WriteToUDP([]byte("x"), target); err == nil {
		t.Fatal("dropped helper must be closed")
	}
	if _, err := sockets[2].WriteToUDP([]byte("x"), target); err == nil {
		t.Fatal("dropped helper must be closed")
	}
	if _, err := sockets[1].WriteToUDP([]byte("x"), target); err != nil {
		t.Fatalf("selected helper must stay writable: %v", err)
	}
}

// TestPunchSentScanMetadata 扫描候选的发送记录携带阶段元数据（命中溯源：
// 「哪一级、第几次猜中的」），普通 PUNCH（信标/直连/反向）stage 为零，
// 命中溯源据此区分「预测命中」与「回程握手」。
func TestPunchSentScanMetadata(t *testing.T) {
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	worker := &linkWorker{log: testLog(), punchSent: make(map[punchSentKey]punchSentRecord)}
	scanned := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 50000}
	plain := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 50001}

	worker.sendScanPunch(sender, "main", scanned, scanCandidate{Port: 50000, Stage: scanStageSweep, Ordinal: 342}, 363)
	worker.sendPunch(sender, "main", plain)

	record, ok := worker.lookupPunchSent("main", scanned)
	if !ok || record.stage != scanStageSweep || record.ordinal != 342 || record.global != 363 {
		t.Fatalf("scan record metadata wrong: %+v ok=%v", record, ok)
	}
	record, ok = worker.lookupPunchSent("main", plain)
	if !ok || record.stage != 0 {
		t.Fatalf("plain record must carry zero stage: %+v ok=%v", record, ok)
	}
	if !worker.punchSentFrom("main", scanned) || !worker.punchSentFrom("main", plain) {
		t.Fatal("both kinds of send must satisfy punchSentFrom")
	}
	// 命中溯源分支冒烟：扫描来源触发观察日志，非扫描来源静默忽略。
	worker.notePredictionHit("main", scanned, "ack")
	worker.notePredictionHit("main", plain, "ack")
}

// TestCreateHelpersCount 创建数量准确、绑定指定地址、可关闭。
func TestCreateHelpersCount(t *testing.T) {
	helpers, err := CreateHelpers(8, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatalf("create helpers: %v", err)
	}
	if len(helpers) != 8 {
		t.Fatalf("expected 8 helpers, got %d", len(helpers))
	}
	for _, socket := range helpers {
		if socket.LocalAddr() == nil {
			t.Fatal("helper must have a local address")
		}
		_ = socket.Close()
	}
}

// TestResendPunchOKSendsAllFour 首发立即 + 100ms×3 补发，共 4 笔必须全部上线。
// PUNCH_OK 是 link1 侧建成的唯一依据；曾经的缺陷是重发循环 select 到已
// 关闭的 connectedCh 立即退出，只发首发一笔——link1 在真实网络上永远
// 收不到 OK（公网真机抓包定位）。
func TestResendPunchOKSendsAllFour(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := &linkWorker{ctx: ctx, cancel: cancel, log: testLog()}
	worker.connectedCh = make(chan struct{})
	close(worker.connectedCh) // 复现真实时序：commit 已发生，connectedCh 已关闭

	target := receiver.LocalAddr().(*net.UDPAddr)
	ok := common.P2PControl{Type: common.P2PTypePunchOK, Token: "aabbccddeeff", SenderSocketID: "12345678", TargetSocketID: "87654321"}
	go worker.resendPunchOK(sender, ok, target)

	received := 0
	deadline := time.Now().Add(2 * time.Second)
	buffer := make([]byte, common.MaxP2PControlDatagram+1)
	for received < punchOKResendCount+1 && time.Now().Before(deadline) {
		_ = receiver.SetReadDeadline(deadline)
		if _, _, err := receiver.ReadFromUDP(buffer); err == nil {
			received++
		}
	}
	if received != punchOKResendCount+1 {
		t.Fatalf("expected %d PUNCH_OK datagrams (1 immediate + %d resends), got %d",
			punchOKResendCount+1, punchOKResendCount, received)
	}
}

// fakeReflector 是单端口的最小探测回显器：解析 PROBE、回 PORT。
type fakeReflector struct {
	socket *net.UDPConn
	port   int
	done   chan struct{}
}

func startFakeReflector(t *testing.T) *fakeReflector {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	reflector := &fakeReflector{socket: socket, port: socket.LocalAddr().(*net.UDPAddr).Port, done: make(chan struct{})}
	go func() {
		defer close(reflector.done)
		buffer := make([]byte, common.MaxProbeDatagram+1)
		for {
			length, remote, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			request, err := common.ParseProbeRequest(buffer[:length])
			if err != nil {
				continue
			}
			response, err := common.EncodeProbeResponse(common.ProbeResponse{
				Nonce: request.Nonce, ProbeID: request.ProbeID,
				PublicIP: common.IPv4(remote.IP.String()), MappedPort: common.Port(remote.Port),
			})
			if err != nil {
				continue
			}
			_, _ = socket.WriteToUDP(response, remote)
		}
	}()
	t.Cleanup(func() {
		_ = socket.Close()
		<-reflector.done
	})
	return reflector
}

// TestProbeOneMatchesNonceOnly probeOne 只接受来自目标端口、nonce 与
// probe_id 匹配的回显——真实回显器 + 真实 UDP 往返。
func TestProbeOneMatchesNonceOnly(t *testing.T) {
	reflector := startFakeReflector(t)
	config := testManagerConfig()
	config.Probe.PerPortTimeout = time.Second
	config.Probe.Retries = 1
	worker := &linkWorker{config: config, log: testLog()}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	worker.mainSocket = socket

	nonce := hex.EncodeToString([]byte{1, 2, 3, 4, 5, 6, 7, 8}) // 8 字节 → 16 个 hex 字符
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: reflector.port}
	response, err := worker.probeOne(context.Background(), socket, target, nonce, 1)
	if err != nil {
		t.Fatalf("probeOne: %v", err)
	}
	if response.Nonce != nonce || response.ProbeID != 1 {
		t.Fatalf("unexpected response %+v", response)
	}
	if response.PublicIP != "127.0.0.1" {
		t.Fatalf("expected observed ip 127.0.0.1, got %s", response.PublicIP)
	}
}

// TestConnectedLink0SupplementsOKOnSelectedPath 已建成的 link0 收到仍在打洞的
// 对端的 PUNCH：ACK 照回到 PUNCH 来源，但 OK 补发必须走本侧选定的路径
// （p2pSocket → peerLive，Target 为对端选定 socket 的 ID）。
// 不从 PUNCH 来源回 OK：link1 会在「收到 OK 的 socket」上建成，而它与
// link0 选定的 socket 可能是两个不同 helper——对称 NAT 下两条映射互不相通，
// 隧路从建成起就是双向黑洞（公网 variable 真机抓出：双方 60s 保活齐超时）。
func TestConnectedLink0SupplementsOKOnSelectedPath(t *testing.T) {
	token := common.GenerateLinkToken()
	ctx, cancel := context.WithCancel(context.Background())
	worker := &linkWorker{
		token: token, log: testLog(),
		events: make(chan WorkerEvent, 4), ctx: ctx, cancel: cancel,
		punchSent: make(map[punchSentKey]punchSentRecord), reverseSent: make(map[reverseKey]struct{}),
		connectedCh: make(chan struct{}),
	}

	// 选定路径：worker 的 p2pSocket ↔ selectedPeer。
	selected, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Close()
	selectedPeer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer selectedPeer.Close()
	worker.commit(selected, "aaaabbbb", "cccc1111", selectedPeer.LocalAddr().(*net.UDPAddr))

	// 迟到 PUNCH 到达另一个 socket，来自另一个来源。
	lateSocket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer lateSocket.Close()
	latePuncher, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer latePuncher.Close()
	punch := common.P2PControl{Type: common.P2PTypePunch, Token: token, SenderSocketID: "dddd2222"}
	encoded, err := common.MarshalP2PControl(punch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := latePuncher.WriteToUDP(encoded, lateSocket.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	worker.handlePunch(wireEvent{
		kind: common.P2PTypePunch, control: punch,
		source: latePuncher.LocalAddr().(*net.UDPAddr), socket: lateSocket, socketID: "eeee3333",
	}, true)

	readAll := func(socket *net.UDPConn) []common.P2PControl {
		buffer := make([]byte, common.MaxP2PControlDatagram+1)
		_ = socket.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var controls []common.P2PControl
		for {
			length, _, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return controls
			}
			if control, err := common.ParseP2PControl(buffer[:length]); err == nil {
				controls = append(controls, control)
			}
		}
	}

	// 迟到 PUNCH 的来源只收到 ACK，绝不能收到 OK。
	lateReplies := readAll(latePuncher)
	if len(lateReplies) != 1 || lateReplies[0].Type != common.P2PTypePunchACK {
		t.Fatalf("late puncher must receive exactly one ACK, got %+v", lateReplies)
	}
	if lateReplies[0].TargetSocketID != "dddd2222" || lateReplies[0].SenderSocketID != "eeee3333" {
		t.Fatalf("ACK fields must echo the punch: %+v", lateReplies[0])
	}
	// 选定路径的对端收到 OK，字段指向两侧选定的 socket。
	selectedReplies := readAll(selectedPeer)
	found := false
	for _, control := range selectedReplies {
		if control.Type == common.P2PTypePunchOK {
			found = true
			if control.TargetSocketID != "cccc1111" || control.SenderSocketID != "aaaabbbb" {
				t.Fatalf("OK must reference the selected sockets: %+v", control)
			}
		}
	}
	if !found {
		t.Fatalf("selected-path peer must receive a PUNCH_OK supplement, got %+v", selectedReplies)
	}
}

// TestLink1RepliesWithReversePunch 未建成的 link1 收到 PUNCH，除 ACK 外还须
// 回一发反向 PUNCH。link1 的握手来源只有对端 PUNCH 的来源地址——对端
// Range 扫描打不中随机分配的映射时（运营商 NAT 常态），这发反向 PUNCH
// 是对端能拿到 ACK（记关联）的唯一途径；同三元组去重只发一次。
func TestLink1RepliesWithReversePunch(t *testing.T) {
	token := common.GenerateLinkToken()
	ctx, cancel := context.WithCancel(context.Background())
	worker := &linkWorker{
		token: token, log: testLog(), ctx: ctx, cancel: cancel,
		punchSent:   make(map[punchSentKey]punchSentRecord),
		reverseSent: make(map[reverseKey]struct{}),
	}
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerAddr := peer.LocalAddr().(*net.UDPAddr)
	receiverID := common.GenerateSocketID()

	readControl := func(t *testing.T, wantCount int) []common.P2PControl {
		t.Helper()
		buffer := make([]byte, common.MaxP2PControlDatagram+1)
		_ = peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var controls []common.P2PControl
		for len(controls) < wantCount {
			length, _, err := peer.ReadFromUDP(buffer)
			if err != nil {
				break
			}
			if control, err := common.ParseP2PControl(buffer[:length]); err == nil {
				controls = append(controls, control)
			}
		}
		return controls
	}

	punch := common.P2PControl{Type: common.P2PTypePunch, Token: token, SenderSocketID: "11112222"}
	encoded, err := common.MarshalP2PControl(punch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteToUDP(encoded, receiver.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	event := wireEvent{kind: common.P2PTypePunch, control: punch, source: peerAddr, socket: receiver, socketID: receiverID}
	worker.handlePunch(event, false)

	controls := readControl(t, 2)
	if len(controls) < 2 || controls[0].Type != common.P2PTypePunchACK || controls[1].Type != common.P2PTypePunch {
		t.Fatalf("expected ACK then reverse PUNCH, got %+v", controls)
	}
	if controls[1].SenderSocketID != receiverID {
		t.Fatalf("reverse PUNCH must carry the receiving socket's ID %s, got %s", receiverID, controls[1].SenderSocketID)
	}

	// 同三元组重复 PUNCH：只回 ACK，不再重发反向 PUNCH。
	worker.handlePunch(event, false)
	controls = readControl(t, 2)
	if len(controls) != 1 || controls[0].Type != common.P2PTypePunchACK {
		t.Fatalf("duplicate triple must yield exactly one ACK, got %+v", controls)
	}
}

// fakeStablePeer 扮演 stable 对端：对收到的每个 PUNCH，从真实收包地址回
// ACK 与 PUNCH_OK（TargetSocketID 取 PUNCH 的 SenderSocketID 原样回指）。
// 同时记录每个源端口声明的 SenderSocketID 与累计发包数，供测试核对
// 配对正确性与信标持续性。
type fakeStablePeer struct {
	socket *net.UDPConn
	id     common.SocketID

	mu       sync.Mutex
	declared map[int]common.SocketID // PUNCH 源端口 → 声明的 SenderSocketID
	counts   map[int]int             // PUNCH 源端口 → 累计收到数
}

func (peer *fakeStablePeer) declarations() map[int]common.SocketID {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	snapshot := make(map[int]common.SocketID, len(peer.declared))
	for port, id := range peer.declared {
		snapshot[port] = id
	}
	return snapshot
}

// count 返回某源端口累计发出的 PUNCH 数（信标持续性断言用）。
func (peer *fakeStablePeer) count(port int) int {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return peer.counts[port]
}

func startFakeStablePeer(t *testing.T, token common.LinkToken) *fakeStablePeer {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	peer := &fakeStablePeer{socket: socket, id: common.GenerateSocketID(), declared: make(map[int]common.SocketID), counts: make(map[int]int)}
	t.Cleanup(func() { _ = socket.Close() })
	go func() {
		buffer := make([]byte, common.MaxP2PControlDatagram+1)
		for {
			length, source, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			control, err := common.ParseP2PControl(buffer[:length])
			if err != nil || control.Type != common.P2PTypePunch {
				continue
			}
			peer.mu.Lock()
			peer.declared[source.Port] = control.SenderSocketID
			peer.counts[source.Port]++
			peer.mu.Unlock()
			ack := common.P2PControl{Type: common.P2PTypePunchACK, Token: token, TargetSocketID: control.SenderSocketID, SenderSocketID: peer.id}
			if encoded, err := common.MarshalP2PControl(ack); err == nil {
				_, _ = socket.WriteToUDP(encoded, source)
			}
			ok := common.P2PControl{Type: common.P2PTypePunchOK, Token: token, TargetSocketID: control.SenderSocketID, SenderSocketID: peer.id}
			if encoded, err := common.MarshalP2PControl(ok); err == nil {
				_, _ = socket.WriteToUDP(encoded, source)
			}
		}
	}()
	return peer
}

// TestVariableLink1HelperHandshake 驱动 variable 侧（本端为 link1）的完整
// helper 打洞握手：fake 对端按 PUNCH 的 SenderSocketID 回 ACK 与 PUNCH_OK，
// Worker 必须建成，且每个 PUNCH 声明的 SenderSocketID 必须属于真正发包的
// socket。后者是 pollHelpers 配对的守护——曾经 ids 取自 map 迭代序、
// sockets 取自切片序，zip 后一轮 256 个 PUNCH 里 255 个声明了别的 socket
// 的 ID，回程 ACK 全被本侧校验拒绝，一轮轮询只剩随机对齐的个别 helper
// 能建成（实测 aligned=1/mismatched=255），helper 档位的端口覆盖率形同虚设。
// 残余盲区：若坏实现恰好只轮询到首个 helper 即建成，本测试无从分辨——
// 概率约 1/256，可接受。
func TestVariableLink1HelperHandshake(t *testing.T) {
	config := testManagerConfig()
	config.Punch.VariableTimeout = 5 * time.Second

	token := common.GenerateLinkToken()
	localDevice, err := common.ParseDeviceID("ffffffff-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	remoteDevice, err := common.ParseDeviceID("00000000-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &linkWorker{
		token: token, peering: common.GeneratePeeringID(),
		peer:   common.ConnectPeer{DeviceID: remoteDevice, Name: "peer", IP: "10.200.0.2"},
		device: localDevice, config: config, log: testLog(),
		events: make(chan WorkerEvent, 64), incoming: make(chan common.NATProfile, 1),
		ctx: ctx, cancel: cancel,
		localVirtualIP: "10.200.0.1", peerVirtualIP: "10.200.0.2",
		punchSent: make(map[punchSentKey]punchSentRecord), reverseSent: make(map[reverseKey]struct{}),
		ackAssoc: make(map[ackKey]ackAssociation), connectedCh: make(chan struct{}),
		eventsIn: make(chan wireEvent, 64),
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	worker.mainSocket = socket
	worker.mainSocketID = common.GenerateSocketID()
	worker.localAddr = net.IPv4(127, 0, 0, 1)

	peer := startFakeStablePeer(t, token)
	peerAddr := peer.socket.LocalAddr().(*net.UDPAddr)
	peerPort := common.Port(peerAddr.Port)
	local := common.NATProfile{NAT: common.NATVariable, PublicIP: "127.0.0.1", Ports: []common.Port{10001, 10002, 10003, 10004, 10005}}
	remote := common.NATProfile{NAT: common.NATStable, PublicIP: "127.0.0.1", Ports: []common.Port{peerPort, peerPort, peerPort, peerPort, peerPort}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.punch(local, remote)
	}()
	defer func() {
		worker.Stop()
		<-done
		worker.finish()
	}()

	// helpers 一出现就快照真值表：commit 会把 helper 池收缩到选中的那一个
	// （pruneHelpers），事后就查不到被收缩 socket 的 ID 了。helpers 切片在
	// punch() 里一次锁内填满，快照要么空要么全量，不会撞上收缩。
	truth := map[int]common.SocketID{
		socket.LocalAddr().(*net.UDPAddr).Port: worker.mainSocketID,
	}
	populated := time.Now().Add(4 * time.Second)
	for {
		worker.helperMu.Lock()
		for _, helper := range worker.helpers {
			truth[helper.socket.LocalAddr().(*net.UDPAddr).Port] = helper.id
		}
		worker.helperMu.Unlock()
		if len(truth) > 1 || time.Now().After(populated) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	deadline := time.Now().Add(4 * time.Second)
	for !worker.connected.Load() {
		if time.Now().After(deadline) {
			t.Fatal("variable link1 helper handshake did not complete: ACK validation on the helper sockets never passed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // 收齐在途 PUNCH 的声明记录

	declarations := peer.declarations()
	if len(declarations) == 0 {
		t.Fatal("fake peer observed no PUNCH datagrams")
	}
	for port, declaredID := range declarations {
		if truth[port] != declaredID {
			t.Fatalf("PUNCH from port %d declared socket ID %s, but that socket's real ID is %s",
				port, declaredID, truth[port])
		}
	}
	// 信标持续（收缩后语义）：建成后 helper 池收缩到选中的那一个，但选中
	// 路径的信标不得停发——晚建成的对端靠持续 PUNCH 获得握手来源，
	// sendSelectedPathOK 的触发也依赖它（variable×stable 公网真机：本侧
	// link0 59ms 建成即停发，对端 15s 预算内零握手来源，PUNCH_TIMEOUT）。
	worker.dataMu.Lock()
	selected := worker.p2pSocket
	worker.dataMu.Unlock()
	if selected == nil {
		t.Fatal("connected without a selected p2p socket")
	}
	selectedPort := selected.LocalAddr().(*net.UDPAddr).Port
	before := peer.count(selectedPort)
	time.Sleep(150 * time.Millisecond)
	if after := peer.count(selectedPort); after <= before {
		t.Fatalf("selected helper beacon stopped after commit: %d packets before, %d after", before, after)
	}
}

// startFakeProbeBase 在 127.0.0.1 上绑定 5 个连续 UDP 端口并各自回显 PROBE
// （run() 的画像阶段按 ProbeBasePort..+4 探测，需要真实连续端点），返回首端口号。
// 绑定冲突时换起点重试。
func startFakeProbeBase(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		base := 20000 + (attempt*7919)%20000
		sockets := make([]*net.UDPConn, 0, common.ProbePortCount)
		bound := true
		for index := 0; index < common.ProbePortCount; index++ {
			socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: base + index})
			if err != nil {
				bound = false
				break
			}
			sockets = append(sockets, socket)
		}
		if !bound {
			for _, socket := range sockets {
				_ = socket.Close()
			}
			continue
		}
		for _, socket := range sockets {
			echo := socket
			go func() {
				buffer := make([]byte, common.MaxProbeDatagram+1)
				for {
					length, remote, err := echo.ReadFromUDP(buffer)
					if err != nil {
						return
					}
					request, err := common.ParseProbeRequest(buffer[:length])
					if err != nil {
						continue
					}
					response, err := common.EncodeProbeResponse(common.ProbeResponse{
						Nonce: request.Nonce, ProbeID: request.ProbeID,
						PublicIP: common.IPv4(remote.IP.String()), MappedPort: common.Port(remote.Port),
					})
					if err != nil {
						continue
					}
					_, _ = echo.WriteToUDP(response, remote)
				}
			}()
			t.Cleanup(func() { _ = echo.Close() })
		}
		return base
	}
	t.Fatal("could not bind 5 consecutive UDP probe ports")
	return 0
}

// TestPeerProfileWaitTimesOut 等待对端画像必须有界收敛。画像上报随控制面
// 断开丢失（服务器永远凑不齐两侧）或对端探测先失败（其失败上报只改服务
// 器侧状态、不通知本端）时，没有上限的等待会让 Worker 永卡 CONNECTING：
// socket 与 goroutine 不释放，快照还把服务器的 IDLE 覆盖回 CONNECTING。
// 走完整 startLinkWorker 流程：探测对 5 连号真实回显端口成功，画像事件
// 之后永不投递对端画像，断言按 PUNCH_TIMEOUT 失败（上限=探测预算+较大
// 打洞预算=350ms，断言放宽到 3s 容忍调度抖动）。
func TestPeerProfileWaitTimesOut(t *testing.T) {
	base := startFakeProbeBase(t)
	config := testManagerConfig()
	config.Server.Addr = fmt.Sprintf("127.0.0.1:%d", base)
	config.Server.ProbeBasePort = base
	config.Probe.Timeout = 300 * time.Millisecond
	config.Probe.PerPortTimeout = 200 * time.Millisecond
	config.Probe.Retries = 0
	config.Punch.StableTimeout = 50 * time.Millisecond
	config.Punch.VariableTimeout = 50 * time.Millisecond

	events := make(chan WorkerEvent, 16)
	worker := startLinkWorker(config, common.GenerateDeviceID(), common.GenerateLinkToken(), common.GeneratePeeringID(),
		common.ConnectPeer{DeviceID: common.GenerateDeviceID(), Name: "peer", IP: "10.200.0.2"},
		"10.200.0.1", nil, events, testLog())
	defer worker.Stop()

	profileSeen := false
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case event := <-events:
			switch event.Kind {
			case WorkerProfile:
				profileSeen = true
			case WorkerFailed:
				if !profileSeen {
					t.Fatal("failure arrived before the probe completed; the wait stage was never reached")
				}
				if event.Reason != common.ReasonPunchTimeout {
					t.Fatalf("expected PUNCH_TIMEOUT, got %s", event.Reason)
				}
				return
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("worker stuck waiting for the peer profile: the wait has no bound")
		}
	}
}
