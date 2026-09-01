//go:build windows

package androidfd

import "errors"

// setNonblock 在 windows 上不可达：本包的 Opener 只经 opener_android.go
// （//go:build android）接线，此分支仅为全平台编译通过而存在。
func setNonblock(fd int) error {
	return errors.New("androidfd requires an android host")
}
