package tun

import (
	"errors"
	"net/netip"
	"testing"

	"gtun-lite/internal/common"
)

type fakeRouteTable struct {
	gateway    netip.Addr
	hasGateway bool
	locals     []netip.Addr
	existing   map[string]bool // 已存在的 /32
	dangling   map[string]bool // 悬空 /32（指向已拆接口的残留）
	deleted    []string        // 记录 DeleteHostRoute 收到的目标
	deleteErr  error
}

func (t *fakeRouteTable) DefaultGateway() (netip.Addr, bool, error) {
	return t.gateway, t.hasGateway, nil
}
func (t *fakeRouteTable) LocalAddresses() ([]netip.Addr, error) { return t.locals, nil }
func (t *fakeRouteTable) HasHostRoute(ip netip.Addr) (bool, error) {
	return t.existing[ip.String()], nil
}
func (t *fakeRouteTable) HostRouteDangling(ip netip.Addr) (bool, error) {
	return t.dangling[ip.String()], nil
}
func (t *fakeRouteTable) DeleteHostRoute(ip netip.Addr) error {
	t.deleted = append(t.deleted, ip.String())
	return t.deleteErr
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
// GTun 自身路由：preflight 执行时旧栈已拆干净，此刻仍指向活跃接口的 /32
// 是他人路由或运维未清的残留，如实报冲突（悬空残留由
// CleanupDanglingHostRoutes 在 preflight 之前处理，见下）。
func TestPreflightRejectsExistingHostRoute(t *testing.T) {
	table := &fakeRouteTable{existing: map[string]bool{"10.0.0.2": true}}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("expected conflict with existing host route")
	}
}

// TestCleanupDanglingHostRoutesDeletesResidue 悬空 /32（指向已拆接口）被自动
// 清理，随后 preflight 可通过——异常退出残留不再需要人工 route delete。
func TestCleanupDanglingHostRoutesDeletesResidue(t *testing.T) {
	table := &fakeRouteTable{
		existing: map[string]bool{"10.0.0.2": true},
		dangling: map[string]bool{"10.0.0.2": true},
	}
	if n := CleanupDanglingHostRoutes(basePreflight(), table, nil); n != 1 {
		t.Fatalf("expected 1 cleaned, got %d", n)
	}
	if len(table.deleted) != 1 || table.deleted[0] != "10.0.0.2" {
		t.Fatalf("unexpected deletions: %v", table.deleted)
	}
}

// TestCleanupSkipsLiveRoutes 指向活跃接口的 /32 不是悬空残留：不删除，
// 留给 preflight 如实报冲突。
func TestCleanupSkipsLiveRoutes(t *testing.T) {
	table := &fakeRouteTable{
		existing: map[string]bool{"10.0.0.2": true},
		dangling: map[string]bool{},
	}
	if n := CleanupDanglingHostRoutes(basePreflight(), table, nil); n != 0 {
		t.Fatalf("live route must not be cleaned, got %d", n)
	}
	if len(table.deleted) != 0 {
		t.Fatalf("live route must not be deleted: %v", table.deleted)
	}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("live host route must still conflict in preflight")
	}
}

// TestCleanupFailureLeavesConflict 删除失败不吞错误也不阻塞：残留留着，
// preflight 仍如实报冲突。
func TestCleanupFailureLeavesConflict(t *testing.T) {
	table := &fakeRouteTable{
		existing:  map[string]bool{"10.0.0.2": true},
		dangling:  map[string]bool{"10.0.0.2": true},
		deleteErr: errors.New("simulated delete failure"),
	}
	if n := CleanupDanglingHostRoutes(basePreflight(), table, nil); n != 0 {
		t.Fatalf("failed delete must not count, got %d", n)
	}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("undeleted residue must still conflict")
	}
}

func TestPreflightRejectsDefaultGateway(t *testing.T) {
	table := &fakeRouteTable{gateway: mustAddr("10.0.0.2"), hasGateway: true}
	if err := Preflight(basePreflight(), table); err == nil {
		t.Fatal("expected conflict with default gateway")
	}
}
