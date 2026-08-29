//go:build android

package client

import (
	"gtun-lite/internal/tun"
	"gtun-lite/internal/tun/androidfd"
)

// PlatformOpener 返回安卓平台的 TUN Opener。fd 由壳层 VpnService 经
// androidfd 的交付协议送入（见 internal/tun/androidfd 包注释）——与桌面
// 三平台「Opener 自己开设备」不同，安卓的授权框与 establish 都在宿主侧。
func PlatformOpener() tun.Opener { return androidfd.Opener{} }
