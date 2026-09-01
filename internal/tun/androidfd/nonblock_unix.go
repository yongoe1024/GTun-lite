//go:build unix

package androidfd

import "golang.org/x/sys/unix"

// setNonblock 把宿主交付的 fd 置为非阻塞；失败时关闭 fd——此刻所有权
// 已随 detachFd 移交本包，不能泄漏。
func setNonblock(fd int) error {
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return err
	}
	return nil
}
