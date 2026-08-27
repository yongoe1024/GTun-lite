//go:build linux

package client

import (
	"gtun-lite/internal/tun"
	"gtun-lite/internal/tun/linux"
)

// PlatformOpener 返回当前平台的 TUN Opener。
func PlatformOpener() tun.Opener { return linux.New() }
