package tun

import (
	"net/netip"
	"testing"
)

// windowsResidueRows 取自中文 Win11 真机 `route print -4` 的崩溃残留
// （2026-08-28 对照实验）：适配器被 taskkill /F 摧毁后，对端 /32 仍在表里，
// Gateway 列为本地化的「在链路上」（On-link），Interface 列退化为「默认」
// ——不再是可解析的接口地址。这正是悬空判定的主战场。
var windowsResidueRows = [][]string{
	{"0.0.0.0", "0.0.0.0", "192.168.31.1", "192.168.31.245", "35"},
	{"127.0.0.0", "255.0.0.0", "在链路上", "127.0.0.1", "331"},
	{"192.168.31.245", "255.255.255.255", "在链路上", "192.168.31.245", "281"},
	{"10.206.0.1", "255.255.255.255", "在链路上", "默认", "1"},
}

// TestWindowsDanglingLocalizedResidue 真实残留形态：Interface 列是本地化
// 占位词「默认」，必须判为悬空（首次实现要求该列可解析为 IP，此形态漏判，
// 真机验证抓回）。
func TestWindowsDanglingLocalizedResidue(t *testing.T) {
	ip := netip.MustParseAddr("10.206.0.1")
	if !windowsRowsHaveHostRoute(windowsResidueRows, ip) {
		t.Fatal("residue /32 must be visible as host route")
	}
	if !windowsRowsDangling(windowsResidueRows, ip) {
		t.Fatal("localized-Default interface column must be dangling")
	}
}

// TestWindowsDanglingLiveAddressInterface 正常存活路由：Interface 列是本机
// 在用地址（127.0.0.1 任何测试机都有），不得判悬空。
func TestWindowsDanglingLiveAddressInterface(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	if windowsRowsDangling(windowsResidueRows, ip) {
		t.Fatal("loopback-bound route must not be dangling")
	}
}

// TestWindowsDanglingUnknownAddressInterface Interface 列可解析为 IP 但
// 没有任何现存接口持有（适配器拆除的另一形态）：判悬空。
func TestWindowsDanglingUnknownAddressInterface(t *testing.T) {
	rows := [][]string{
		{"10.206.0.1", "255.255.255.255", "On-link", "10.206.0.2", "1"},
	}
	if !windowsRowsDangling(rows, netip.MustParseAddr("10.206.0.1")) {
		t.Fatal("address owned by no interface must be dangling")
	}
}

// TestWindowsDanglingAbsentRoute 无该 /32 时自然非悬空。
func TestWindowsDanglingAbsentRoute(t *testing.T) {
	if windowsRowsDangling(windowsResidueRows, netip.MustParseAddr("10.9.9.9")) {
		t.Fatal("absent route must not be dangling")
	}
}
