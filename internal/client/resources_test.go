package client

import (
	"net"
	"testing"
)

// TestValidateHelperCountAcceptsTiers 三个合法档位都必须通过。
func TestValidateHelperCountAcceptsTiers(t *testing.T) {
	t.Parallel()
	for _, count := range []int{256, 512, 1024} {
		if err := ValidateHelperCount(count); err != nil {
			t.Errorf("ValidateHelperCount(%d) rejected a valid tier: %v", count, err)
		}
	}
}

// TestValidateHelperCountRejectsOthers 档位之外的值一律拒绝，包括看起来"合理"的
// 中间值和 0。校验发生在启动阶段，拒绝启动优于运行期降级——降档会让打洞成功率
// 在同一次运行中悄悄变化，故障排查时无法复现。
func TestValidateHelperCountRejectsOthers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		count int
	}{
		{"zero", 0},
		{"negative", -1},
		{"between tiers", 300},
		{"just below a tier", 255},
		{"just above a tier", 1025},
		{"above the highest tier", 2048},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateHelperCount(c.count); err == nil {
				t.Fatalf("ValidateHelperCount(%d) accepted a value outside the allowed tiers", c.count)
			}
		})
	}
}

// TestSelectLocalIPv4PicksRouteSource 源地址必须来自内核对目标的路由选择：
// 以环回目标为例，选出的源必须是本机环回地址，而不是通配地址。
// 多宿主机上这一选择决定了画像与打洞共用哪条出口。
func TestSelectLocalIPv4PicksRouteSource(t *testing.T) {
	t.Parallel()
	local, err := selectLocalIPv4(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatalf("selectLocalIPv4: %v", err)
	}
	if local == nil || !local.IsLoopback() {
		t.Fatalf("source toward loopback must be a loopback address, got %v", local)
	}
}
