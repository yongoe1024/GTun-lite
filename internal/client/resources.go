package client

import (
	"fmt"
	"net"
)

// helperTiers 是 helper socket 数量允许的三个档位。
//
// helper 数量直接决定 variable 侧 NAT 的端口覆盖率：每个 helper 占一个本地
// 端口参与打洞，数量越多命中对端映射端口的概率越高。它是给用户调的旋钮，
// 不是实现细节，所以由配置显式指定。
//
// 限定三档而非任意整数，是为了让不同部署的打洞成功率可以横向比较——
// 任意值会让「这台机器为什么比那台难打通」多一个无法排除的变量。
var helperTiers = map[int]bool{256: true, 512: true, 1024: true}

// ValidateHelperCount 校验档位，在配置加载阶段调用。
//
// 校验放在启动路径上、失败就拒绝启动，刻意不做两件事：
//   - 不探测系统 fd 上限再自动挑一档。那等于把配置错误静默改写成一个
//     「能跑起来」的值，用户以为自己配的是 1024，实际跑的是 256。
//   - 不在运行期因创建失败而降档。降档会让打洞成功率在同一次运行中
//     悄悄变化，故障排查时无法复现。
//
// fd 不够就是配置错误，启动时报出来由用户决定改配置还是调系统限制。
func ValidateHelperCount(count int) error {
	if !helperTiers[count] {
		return fmt.Errorf("helper_count must be 256, 512 or 1024, got %d", count)
	}
	return nil
}

// selectLocalIPv4 经一次 UDP connect 让内核选出「通往 target 的本机源地址」。
// UDP connect 不发任何包，只做路由选择。主 socket 与 helper 都绑定这个地址：
// 多宿主机（Wi-Fi+有线、VPN utun）上通配绑定会让画像与打洞各走各的出口，
// NAT 映射随出口变化，打洞必败且无从排查——画像测出哪条出口，打洞就用哪条。
func selectLocalIPv4(target *net.UDPAddr) (net.IP, error) {
	connection, err := net.DialUDP("udp4", nil, target)
	if err != nil {
		return nil, fmt.Errorf("select local address toward %s: %w", target, err)
	}
	defer connection.Close()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("route toward %s returned a non-UDP address", target)
	}
	return local.IP, nil
}

// CreateHelpers 批量创建 helper socket，绑定与主 socket 相同的本机地址
// （端口由内核分配）。任一失败即关闭全部已创建 socket 并返回错误——
// 不降档、不部分成功：档位决定 variable 侧的端口覆盖率，实际数量少于
// 配置会让打洞成功率悄然变化且无法复现。
func CreateHelpers(count int, localIP net.IP) ([]*net.UDPConn, error) {
	helpers := make([]*net.UDPConn, 0, count)
	for i := 0; i < count; i++ {
		socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
		if err != nil {
			for _, created := range helpers {
				_ = created.Close()
			}
			return nil, fmt.Errorf("create helper %d/%d: %w", i+1, count, err)
		}
		helpers = append(helpers, socket)
	}
	return helpers, nil
}
