package tun

import (
	"net/netip"
	"testing"

	"gtun-lite/internal/common"
)

type fakeRouteTable struct {
	gateway    netip.Addr
	hasGateway bool
	locals     []netip.Addr
	existing   map[string]bool // 已存在的 /32
}

func (t *fakeRouteTable) DefaultGateway() (netip.Addr, bool, error) {
	return t.gateway, t.hasGateway, nil
}
func (t *fakeRouteTable) LocalAddresses() ([]netip.Addr, error) { return t.locals, nil }
func (t *fakeRouteTable) HasHostRoute(ip netip.Addr) (bool, error) {
	return t.existing[ip.String()], nil
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func basePreflight() PreflightInput {
	return PreflightInput{LocalIP: "10.0.0.1", Peers: []common.IPv4{"10.0.0.2"}, ServerIP: "203.0.113.10"}
}

func TestPreflightAcceptsValidConfig(t *testing.T) {
	err := Preflight(basePreflight(), &fakeRouteTable{})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestPreflightRejectsServerIP(t *testing.T) {
	input := basePreflight()
	input.Peers = []common.IPv4{"203.0.113.10"} // 对端 = 服务器 IP
	if err := Preflight(input, &fakeRouteTable{}); err == nil {
		t.Fatal("expected conflict with server IP")
	}
}

func TestPreflightRejectsLoopback(t *testing.T) {
	input := basePreflight()
	input.Peers = []common.IPv4{"127.0.0.1"}
	if err := Preflight(input, &fakeRouteTable{}); err == nil {
		t.Fatal("expected conflict with loopback")
	}
}

func TestPreflightRejectsLinkLocal(t *testing.T) {
	input := basePreflight()
	input.LocalIP = "169.254.1.1"
	if err := Preflight(input, &fakeRouteTable{}); err == nil {
		t.Fatal("expected conflict with link-local")
	}
}

func TestPreflightRejectsLocalAddress(t *testing.T) {
	table := &fakeRouteTable{locals: []netip.Addr{mustAddr("10.0.0.2")}}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("expected conflict with local address")
	}
}

// TestPreflightRejectsExistingHostRoute 任何已存在的 /32 一律冲突——不豁免
// GTun 自身路由：preflight 执行时旧栈已拆干净，残留即异常退出遗留，
// 如实报冲突交运维（重应用幂等由「拓扑未变不重建」保证）。
func TestPreflightRejectsExistingHostRoute(t *testing.T) {
	table := &fakeRouteTable{existing: map[string]bool{"10.0.0.2": true}}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("expected conflict with existing host route")
	}
}

func TestPreflightRejectsDefaultGateway(t *testing.T) {
	table := &fakeRouteTable{gateway: mustAddr("10.0.0.2"), hasGateway: true}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("expected conflict with default gateway")
	}
}
