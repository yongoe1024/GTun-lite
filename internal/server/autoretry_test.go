package server

import (
	"net/http"
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// waitForLinkState 轮询等待目标链路进入指定状态，返回该行视图。
func waitForLinkState(t *testing.T, server *testServer, deviceA, deviceB common.DeviceID, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		link := linksView(t, server, deviceA, deviceB)
		if link != nil && link["state"] == want {
			return link
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("link did not reach %s within deadline", want)
	return nil
}

// TestAutoRetrySweepReconnectsIdleLinks 全链路验证开关语义：手动建链后伪造
// 失败回到 IDLE，开启开关后循环在窗口内重发 CONNECT；关闭后失败不再被重试。
func TestAutoRetrySweepReconnectsIdleLinks(t *testing.T) {
	server := startTestServer(t)
	server.retry.Stop()
	server.retry.interval = 30 * time.Millisecond
	server.retry.Start()

	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	defer clientA.conn.Close()
	defer clientB.conn.Close()
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)

	// 手动建链一次让链路进入内存表，随即伪造失败收敛回 IDLE。
	status, body := adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusAccepted {
		t.Fatalf("manual connect: %d %v", status, body)
	}
	connectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)
	clientA.reportFailure(peering, connectA.Token, common.ReasonPunchTimeout)
	waitForLinkState(t, server, deviceA, deviceB, "IDLE")

	// 新鲜服务端开关默认关。
	_, linksBody := adminCall(t, server, http.MethodGet, "/api/links", nil)
	if linksBody["auto_retry"] != false {
		t.Fatalf("auto retry must default to off, got %v", linksBody["auto_retry"])
	}

	// 开启：循环应在窗口内向双方重发 CONNECT（新 token）。
	status, body = adminCall(t, server, http.MethodPost, "/api/links/auto-retry", map[string]bool{"enable": true})
	if status != http.StatusAccepted || body["status"] != "auto_retry_enabled" {
		t.Fatalf("enable: %d %v", status, body)
	}
	reconnectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientB.read(2*time.Second, common.MessageConnect)
	if reconnectA.Token == connectA.Token {
		t.Fatalf("auto retry must issue a fresh token")
	}
	link := waitForLinkState(t, server, deviceA, deviceB, "CONNECTING")
	if link["auto_retry"] != nil {
		t.Fatalf("auto_retry is a response-level key, not per-link: %v", link)
	}
	_, linksBody = adminCall(t, server, http.MethodGet, "/api/links", nil)
	if linksBody["auto_retry"] != true {
		t.Fatalf("auto retry must read back as on, got %v", linksBody["auto_retry"])
	}

	// 幂等：重复开启不产生第二个循环（race detector 把关），状态照常推进。
	status, _ = adminCall(t, server, http.MethodPost, "/api/links/auto-retry", map[string]bool{"enable": true})
	if status != http.StatusAccepted {
		t.Fatalf("re-enable must stay idempotent: %d", status)
	}

	// 关闭：失败收敛回 IDLE 后跨多个扫描周期保持不动。
	status, body = adminCall(t, server, http.MethodPost, "/api/links/auto-retry", map[string]bool{"enable": false})
	if status != http.StatusAccepted || body["status"] != "auto_retry_disabled" {
		t.Fatalf("disable: %d %v", status, body)
	}
	clientB.reportFailure(peering, reconnectA.Token, common.ReasonPunchTimeout)
	waitForLinkState(t, server, deviceA, deviceB, "IDLE")
	time.Sleep(10 * server.retry.interval)
	if link := linksView(t, server, deviceA, deviceB); link["state"] != "IDLE" {
		t.Fatalf("disabled loop must not reconnect, got %v", link["state"])
	}
	_, linksBody = adminCall(t, server, http.MethodGet, "/api/links", nil)
	if linksBody["auto_retry"] != false {
		t.Fatalf("auto retry must read back as off, got %v", linksBody["auto_retry"])
	}
}

// TestAutoRetrySweepOfflinePairSkipped 循环对单侧离线的链路只跳过不中断：
// 同一时刻库里另一对可建链的链路照常被扫描下发。
func TestAutoRetrySweepOfflinePairSkipped(t *testing.T) {
	server := startTestServer(t)
	server.retry.Stop()
	server.retry.interval = 30 * time.Millisecond
	server.retry.Start()

	// 可建链的一对：双端在线、手动建链失败后留在 IDLE。
	deviceA, deviceB := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientA := dial(t, server, deviceA)
	clientB := dial(t, server, deviceB)
	defer clientA.conn.Close()
	defer clientB.conn.Close()
	_, peering := fixtureNetwork(t, server, deviceA, deviceB)
	status, _ := adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceA), "device_b": string(deviceB)})
	if status != http.StatusAccepted {
		t.Fatalf("manual connect: %d", status)
	}
	connectA := clientA.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientA.reportFailure(peering, connectA.Token, common.ReasonPunchTimeout)
	waitForLinkState(t, server, deviceA, deviceB, "IDLE")

	// 单侧离线的另一对：建过链（有内存记录）、失败后同样停在 IDLE。
	// C、D 独立组成第二个网络（一台设备只属一个网络，B 不能复用）。
	deviceC, deviceD := common.GenerateDeviceID(), common.GenerateDeviceID()
	clientC := dial(t, server, deviceC)
	clientD := dial(t, server, deviceD)
	defer clientD.conn.Close()
	_, peeringCD := fixtureNetworkWithCIDR(t, server, "10.201.0.0/24", deviceC, deviceD)
	status, _ = adminCall(t, server, http.MethodPost, "/api/links/connect",
		map[string]string{"device_a": string(deviceC), "device_b": string(deviceD)})
	if status != http.StatusAccepted {
		t.Fatalf("manual connect cd: %d", status)
	}
	connectCD := clientC.read(2*time.Second, common.MessageConnect).(*common.Connect)
	clientD.read(2*time.Second, common.MessageConnect)
	clientC.reportFailure(peeringCD, connectCD.Token, common.ReasonPunchTimeout)
	waitForLinkState(t, server, deviceC, deviceD, "IDLE")
	_ = clientC.conn.Close()
	waitOffline(t, server, deviceC)

	adminCall(t, server, http.MethodPost, "/api/links/auto-retry", map[string]bool{"enable": true})
	// C 侧离线的对被跳过（Debug 日志），AB 对不受影响被重发。
	clientA.read(2*time.Second, common.MessageConnect)
	if link := waitForLinkState(t, server, deviceA, deviceB, "CONNECTING"); link == nil {
		t.Fatalf("online pair should be re-issued")
	}
}

// fixtureNetworkWithCIDR 与 fixtureNetwork 相同，但允许指定网段（同一测试
// 里建第二个网络时避免 CIDR 撞车）。
func fixtureNetworkWithCIDR(t *testing.T, server *testServer, cidr string, deviceA, deviceB common.DeviceID) (common.NetworkID, common.PeeringID) {
	t.Helper()
	status, body := adminCall(t, server, http.MethodPost, "/api/networks", map[string]string{"name": "testnet2", "cidr": cidr})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %v", status, body)
	}
	network := common.NetworkID(body["id"].(string))
	for _, device := range []common.DeviceID{deviceA, deviceB} {
		status, body = adminCall(t, server, http.MethodPost, "/api/devices/"+string(device)+"/approve", nil)
		if status != http.StatusOK {
			t.Fatalf("approve device %s: %d %v", device, status, body)
		}
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
