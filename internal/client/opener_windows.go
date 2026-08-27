//go:build windows

package client

import (
	"gtun-lite/internal/tun"
	"gtun-lite/internal/tun/win"
)

// PlatformOpener 返回当前平台的 TUN Opener。
func PlatformOpener() tun.Opener { return win.New() }
