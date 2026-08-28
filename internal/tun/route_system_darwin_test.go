//go:build darwin

package tun

import (
	"net/netip"
	"testing"
)

// TestParseDarwinHostRoute 样例取自 macOS 真实 `netstat -rn -f inet` 输出，
// 验证目的字段两种形态的匹配：`route add -host` 安装的静态路由显示为 <ip>/32，
// 内核学习条目显示为裸 <ip>，两者都算存在主机路由；网络路由（短形态/带 /N）与
// default 不匹配。样例含 30.0.0.1/32（其他 VPN 经 utun 安装的静态 /32）。
// 同时验证 Netif 列提取（悬空判定的输入）。
func TestParseDarwinHostRoute(t *testing.T) {
	out := `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            192.168.31.1       UGScg                 en0
127                127.0.0.1          UCS                   lo0
192.168.31         link#14            UCS                   en0      !
192.168.31.1/32    link#14            UCS                   en0      !
192.168.31.157/32  link#14            UCS                   en0      !
30.0.0.1/32        10.251.1.1         UGSc                  utun0
192.168.31.1       4:67:61:95:b5:50   UHLWIir               en0   1177
224.0.0/4          link#14            UmCS                  en0      !
`
	// 静态 /32（GTun `route add -host` 与其他 VPN 的安装形态）必须命中，
	// 且 Netif 列提取正确。
	for ip, wantIface := range map[string]string{
		"192.168.31.157": "en0",
		"30.0.0.1":       "utun0",
		"192.168.31.1":   "en0",
	} {
		iface, found := parseDarwinHostRoute(out, netip.MustParseAddr(ip))
		if !found {
			t.Errorf("parseDarwinHostRoute(%s) not found, want found", ip)
			continue
		}
		if iface != wantIface {
			t.Errorf("parseDarwinHostRoute(%s) netif = %q, want %q", ip, iface, wantIface)
		}
	}
	// 网络路由 / default / 不存在的地址不得命中。
	for _, ip := range []string{"192.168.31.42", "224.0.0.5", "10.9.9.9", "127.0.0.2"} {
		if _, found := parseDarwinHostRoute(out, netip.MustParseAddr(ip)); found {
			t.Errorf("parseDarwinHostRoute(%s) = true, want false", ip)
		}
	}
}
