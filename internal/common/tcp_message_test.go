package common

import (
	"encoding/json"
	"strings"
	"testing"
)

// testNetworkDefinition 构造一份合法的单对端网络定义。
func testNetworkDefinition() NetworkDefinition {
	return NetworkDefinition{
		ID:   testNetwork,
		Name: "office",
		CIDR: "10.100.0.0/24",
		IP:   "10.100.0.2",
		Peers: []NetworkPeer{{
			DeviceID:  testDeviceB,
			PeeringID: testPeering,
			Name:      "laptop",
			IP:        "10.100.0.3",
			Online:    true,
		}},
	}
}

// TestNilPeersMarshalsAsEmptyArray nil slice 必须序列化成 []，不能是 null。
// "没有对端"是正常状态（设备刚入网、对端全被移除），线上不能表现成特殊值。
func TestNilPeersMarshalsAsEmptyArray(t *testing.T) {
	t.Parallel()
	network := testNetworkDefinition()
	network.Peers = nil
	data, err := json.Marshal(network)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"peers":null`) {
		t.Fatalf("nil peers serialized as null: %s", data)
	}
	if !strings.Contains(string(data), `"peers":[]`) {
		t.Fatalf("nil peers did not serialize as []: %s", data)
	}
	// 整条消息（Network 为指针）也必须如此。
	config := NetworkConfig{Type: MessageNetworkConfig, Network: &network}
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"peers":null`) {
		t.Fatalf("nil peers serialized as null inside the message: %s", data)
	}
}

// TestNetworkConfigAcceptsNullNetwork Network 为 nil 表示该设备不属于任何网络，
// 这是合法配置而非错误——不能因为"空"就拒绝。
func TestNetworkConfigAcceptsNullNetwork(t *testing.T) {
	t.Parallel()
	config := NetworkConfig{Type: MessageNetworkConfig}
	if err := config.Validate(); err != nil {
		t.Fatalf("an empty configuration was rejected: %v", err)
	}
}

// TestNetworkConfigRejectsWrongType 类型字段不符必须拒绝。
func TestNetworkConfigRejectsWrongType(t *testing.T) {
	t.Parallel()
	config := NetworkConfig{Type: "something_else"}
	if err := config.Validate(); err == nil {
		t.Fatal("a message with the wrong type was accepted")
	}
}

// TestNetworkDefinitionAcceptsValid 合法定义必须通过。
func TestNetworkDefinitionAcceptsValid(t *testing.T) {
	t.Parallel()
	if err := testNetworkDefinition().Validate(); err != nil {
		t.Fatalf("a valid network definition was rejected: %v", err)
	}
}

// TestNetworkDefinitionRejectsAddressErrors 地址类约束：本机与对端 IP 都必须是
// CIDR 内可用的主机地址，且不得重复。虚拟 IP 重复会让两条链路的路由互相覆盖。
func TestNetworkDefinitionRejectsAddressErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*NetworkDefinition)
	}{
		{"local IP outside CIDR", func(n *NetworkDefinition) { n.IP = "10.200.0.2" }},
		{"local IP is the network address", func(n *NetworkDefinition) { n.IP = "10.100.0.0" }},
		{"local IP is the broadcast address", func(n *NetworkDefinition) { n.IP = "10.100.0.255" }},
		{"peer IP outside CIDR", func(n *NetworkDefinition) { n.Peers[0].IP = "10.200.0.3" }},
		{"peer IP duplicates local IP", func(n *NetworkDefinition) { n.Peers[0].IP = n.IP }},
		{"non-canonical CIDR", func(n *NetworkDefinition) { n.CIDR = "10.100.0.5/24" }},
		{"public CIDR", func(n *NetworkDefinition) { n.CIDR = "8.8.8.0/24"; n.IP = "8.8.8.2"; n.Peers[0].IP = "8.8.8.3" }},
		{"invalid network id", func(n *NetworkDefinition) { n.ID = "xyz" }},
		{"invalid peer device id", func(n *NetworkDefinition) { n.Peers[0].DeviceID = "short" }},
		{"empty peer name", func(n *NetworkDefinition) { n.Peers[0].Name = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			network := testNetworkDefinition()
			c.mutate(&network)
			if err := network.Validate(); err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		})
	}
}

// TestNetworkDefinitionRejectsDuplicatePeers 同一快照内设备、配对、虚拟 IP 都不得重复。
func TestNetworkDefinitionRejectsDuplicatePeers(t *testing.T) {
	t.Parallel()
	base := testNetworkDefinition()
	second := NetworkPeer{DeviceID: testDeviceA, PeeringID: PeeringID(strings.Repeat("a", 32)), Name: "desktop", IP: "10.100.0.4"}

	t.Run("duplicate device", func(t *testing.T) {
		network := testNetworkDefinition()
		duplicate := second
		duplicate.DeviceID = base.Peers[0].DeviceID
		network.Peers = append(network.Peers, duplicate)
		if err := network.Validate(); err == nil {
			t.Fatal("a duplicate peer device_id was accepted")
		}
	})
	t.Run("duplicate peering", func(t *testing.T) {
		network := testNetworkDefinition()
		duplicate := second
		duplicate.PeeringID = base.Peers[0].PeeringID
		network.Peers = append(network.Peers, duplicate)
		if err := network.Validate(); err == nil {
			t.Fatal("a duplicate peering_id was accepted")
		}
	})
	t.Run("duplicate virtual IP", func(t *testing.T) {
		network := testNetworkDefinition()
		duplicate := second
		duplicate.IP = base.Peers[0].IP
		network.Peers = append(network.Peers, duplicate)
		if err := network.Validate(); err == nil {
			t.Fatal("a duplicate virtual IP was accepted")
		}
	})
}

// TestDecodeMessageAllowsUnknownFields 未知字段必须被忽略而非拒绝整条消息：
// 两端版本不必严格同步，旧端遇到新增字段应当继续工作。
func TestDecodeMessageAllowsUnknownFields(t *testing.T) {
	t.Parallel()
	raw := `{"type":"network_config","network":null,"future_field":{"nested":1}}`
	message, err := DecodeMessage([]byte(raw))
	if err != nil {
		t.Fatalf("an unknown field caused rejection: %v", err)
	}
	config, ok := message.(*NetworkConfig)
	if !ok || config.Type != MessageNetworkConfig {
		t.Fatalf("decoded message = %T, want *NetworkConfig", message)
	}
}

// TestDecodeMessageStillRejectsMalformed 宽容的只是"多出来的字段"，
// 畸形结构与业务约束违反仍必须拒绝。
func TestDecodeMessageStillRejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"broken json", `{"type":`},
		{"wrong type value", `{"type":"bogus"}`},
		{"wrong field kind", `{"type":"network_config","network":"not-an-object"}`},
		{"invalid nested network", `{"type":"network_config","network":{"id":"xyz","name":"n","cidr":"10.0.0.0/24","ip":"10.0.0.2","peers":[]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeMessage([]byte(c.raw)); err == nil {
				t.Fatalf("malformed input accepted: %s", c.raw)
			}
		})
	}
}

// TestEndpointValidate endpoint 必须是可路由单播地址加非零端口。
func TestEndpointValidate(t *testing.T) {
	t.Parallel()
	if err := (Endpoint{IP: "198.51.100.10", Port: 40000}).Validate(); err != nil {
		t.Fatalf("a valid endpoint was rejected: %v", err)
	}
	rejects := []Endpoint{
		{IP: "198.51.100.10", Port: 0},     // 零端口
		{IP: "0.0.0.0", Port: 40000},       // 未指定地址
		{IP: "224.0.0.1", Port: 40000},     // 组播
		{IP: "255.255.255.255", Port: 100}, // 广播
		{IP: "not-an-ip", Port: 40000},
	}
	for _, endpoint := range rejects {
		if err := endpoint.Validate(); err == nil {
			t.Errorf("invalid endpoint accepted: %+v", endpoint)
		}
	}
}
