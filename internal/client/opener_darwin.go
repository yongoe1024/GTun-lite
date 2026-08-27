//go:build darwin

package client

import (
	"gtun-lite/internal/tun"
	"gtun-lite/internal/tun/mac"
)

// PlatformOpener 返回当前平台的 TUN Opener。
func PlatformOpener() tun.Opener { return mac.New() }
