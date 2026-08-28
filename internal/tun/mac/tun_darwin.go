//go:build darwin

package mac

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// opener 用内核 utun 控制套接字创建 utunN 设备。
type opener struct{}

// New 返回 macOS 平台的 tun.Opener。
func New() tun.Opener { return opener{} }

const (
	sysProtoControl = 2          // SYSPROTO_CONTROL
	ctlIoctlInfo    = 0xC0644E03 // CTLIOCGINFO: _IOWR('N', 3, struct ctl_info)
	afInet          = uint32(2)  // AF_INET，utun 数据包前缀
)

// utunPrefix 是写入 utun 时的 4 字节地址族前缀。
var utunPrefix = func() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, afInet)
	return b
}()

// Open 创建 utun 设备并配置点对点/主机路由。
func (opener) Open(_ context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	if err := localIP.Validate(); err != nil {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid local IP: %w", err)
	}
	if mtu < 20 || mtu > common.MaxTUNMTU {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid MTU %d", mtu)
	}
	fd, unit, err := openUtun()
	if err != nil {
		return nil, tun.RouteCleanup{}, err
	}
	actualName := fmt.Sprintf("utun%d", unit-1) // scUnit=N 对应接口 utun(N-1)（探针实证：scUnit=9 → utun8）
	// 必须先置非阻塞再 os.NewFile：只有非阻塞 fd 才被 runtime poller 接管，
	// Close 才能唤醒阻塞的读循环、优雅退出不卡死（与 Linux 同源，Go 文档化
	// 行为）。「非阻塞进 poller 后 kqueue 不投递事件、数据面全死」的旧项目
	// 观察已复验推翻（2026-08-27，实验档案见 test/e2e/真机验收记录.md）。
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, tun.RouteCleanup{}, fmt.Errorf("set utun nonblocking: %w", err)
	}
	file := os.NewFile(uintptr(fd), actualName)
	if err := configureInterface(actualName, mtu, localIP); err != nil {
		_ = file.Close()
		return nil, tun.RouteCleanup{}, fmt.Errorf("configure interface: %w", err)
	}
	routes := []tun.RouteEntry{}
	for _, peer := range peers {
		if err := addHostRoute(actualName, peer); err != nil {
			for _, r := range routes {
				_ = deleteHostRoute(r.Interface, r.Destination)
			}
			_ = file.Close()
			return nil, tun.RouteCleanup{}, fmt.Errorf("add host route %s: %w", peer, err)
		}
		routes = append(routes, tun.RouteEntry{Destination: peer, Interface: actualName})
	}
	cleanup := tun.NewRouteCleanup(actualName, routes, func() error {
		var first error
		// 显式移除接口地址：fd 关闭时接口随之消亡、地址自然消失，但内核
		// 拆除是异步的（存在亚秒级窗口，2026-08-27 实测）——拆栈后立即
		// preflight/重建的路径不能看到将死接口的地址，必须先同步拆干净。
		if err := removeInterfaceAddress(actualName, localIP); err != nil && first == nil {
			first = err
		}
		for _, route := range routes {
			if err := deleteHostRoute(route.Interface, route.Destination); err != nil && first == nil {
				first = err
			}
		}
		return first
	})
	return &device{file: file, name: actualName}, cleanup, nil
}

// ctlInfo 对应 macOS 内核 struct ctl_info。
type ctlInfo struct {
	ctlID   uint32
	ctlName [96]byte
}

// sockaddrCtl 对应 macOS struct sockaddr_ctl（ss_sysaddr 是 u_int16_t，sc_id 是 u_int32_t）。
type sockaddrCtl struct {
	scLen      uint8
	scFamily   uint8
	ssSysaddr  uint16
	scID       uint32
	scUnit     uint32
	scReserved [5]int32
}

// openUtun 创建 utun 控制套接字。macOS utun 需要指定具体 unit，connect 成功即表示可用。
// 每次尝试需要新 socket（connect 失败后 socket 状态不可恢复）。
// 从系统已存在 utun 编号之后分配：connect 到 configd 残留的编号会"复活"壳接口，
// 内核把路由包送到壳 ifnet 却不进新 fd 的队列——数据面全静默（真实双机定位，
// 独立探针在同配置的全新 unit 上读写正常）。
func openUtun() (fd int, unit uint32, err error) {
	info, err := queryCtlInfo(utunControlName)
	if err != nil {
		return 0, 0, err
	}
	start := uint32(0)
	if max := maxExistingUtunUnit(); max >= 0 {
		start = uint32(max) + 2 // scUnit=N 对应接口 utun(N-1)
	}
	for candidate := start; candidate < 256; candidate++ {
		socket, err := syscall.Socket(syscall.AF_SYSTEM, syscall.SOCK_DGRAM, sysProtoControl)
		if err != nil {
			return 0, 0, fmt.Errorf("system socket: %w", err)
		}
		addr := sockaddrCtl{scLen: uint8(unsafe.Sizeof(sockaddrCtl{})), scFamily: syscall.AF_SYSTEM, ssSysaddr: sysProtoControl, scID: info.ctlID, scUnit: candidate}
		if _, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(socket), uintptr(unsafe.Pointer(&addr)), uintptr(unsafe.Sizeof(addr))); errno == 0 {
			return socket, candidate, nil
		}
		syscall.Close(socket)
	}
	return 0, 0, errors.New("no free utun unit")
}

// maxExistingUtunUnit 返回系统当前 utun 接口的最大编号，无则 -1。
func maxExistingUtunUnit() int {
	out, err := exec.Command("ifconfig", "-l").Output()
	if err != nil {
		return -1
	}
	max := -1
	for _, field := range strings.Fields(string(out)) {
		if !strings.HasPrefix(field, "utun") {
			continue
		}
		if number, err := strconv.Atoi(field[len("utun"):]); err == nil && number > max {
			max = number
		}
	}
	return max
}

const utunControlName = "com.apple.net.utun_control"

// queryCtlInfo 通过 CTLIOCGINFO 获取 control provider ID。
func queryCtlInfo(name string) (ctlInfo, error) {
	var info ctlInfo
	copy(info.ctlName[:], name)
	fd, err := syscall.Socket(syscall.AF_SYSTEM, syscall.SOCK_DGRAM, sysProtoControl)
	if err != nil {
		return ctlInfo{}, err
	}
	defer syscall.Close(fd)
	// CTLIOCGINFO 是 _IOWR('N', 3, struct ctl_info)，直接对原始 fd ioctl。
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(ctlIoctlInfo), uintptr(unsafe.Pointer(&info))); errno != 0 {
		return ctlInfo{}, fmt.Errorf("CTLIOCGINFO: %w", errno)
	}
	return info, nil
}

type device struct {
	file *os.File
	name string
}

// Read 从 utun 读取并剥离 4 字节地址族前缀，返回原始 IPv4 包。
func (d *device) Read(buffer []byte) (int, error) {
	if len(buffer) < 4 {
		return 0, errors.New("buffer too small for utun header")
	}
	n, err := d.file.Read(buffer)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, nil
	}
	prefix := binary.BigEndian.Uint32(buffer[:4])
	if prefix != afInet {
		return 0, nil // 非 IPv4 静默丢弃
	}
	packet := buffer[4:n]
	copy(buffer, packet)
	return n - 4, nil
}

// Write 在原始 IPv4 包前加 4 字节 AF_INET 地址族前缀后写入 utun。
func (d *device) Write(packet []byte) (int, error) {
	if len(packet) == 0 {
		return 0, nil
	}
	buf := make([]byte, 4+len(packet))
	copy(buf[:4], utunPrefix)
	copy(buf[4:], packet)
	if _, err := d.file.Write(buf); err != nil {
		return 0, err
	}
	return len(packet), nil
}

// Name 返回内核分配的 utun 接口名（请求名仅作日志前缀）。
func (d *device) Name() string { return d.name }

// Close 关闭 utun fd：经 runtime poller 唤醒阻塞的读循环退出。地址与路由
// 由 RouteCleanup 在 fd 关闭之前拆除（内核对接口的拆除是异步的，不能指望
// fd 一关地址就立刻从系统消失）。
func (d *device) Close() error { return d.file.Close() }

// configureInterface 用 ifconfig 配置 utun 地址与 MTU。
func configureInterface(name string, mtu int, localIP common.IPv4) error {
	if err := run("ifconfig", name, "inet", string(localIP), string(localIP), "mtu", fmt.Sprintf("%d", mtu), "up"); err != nil {
		return fmt.Errorf("ifconfig addr: %w", err)
	}
	return nil
}

// addHostRoute 用 route 命令安装 /32 主机路由。
func addHostRoute(name string, peer common.IPv4) error {
	return run("route", "-q", "add", "-host", string(peer), "-interface", name)
}

// deleteHostRoute 删除 /32 主机路由（回滚）。
func deleteHostRoute(name string, peer common.IPv4) error {
	return run("route", "-q", "delete", "-host", string(peer), "-interface", name)
}

// removeInterfaceAddress 移除 utun 上的虚拟 IP，防止 configd 残留。
func removeInterfaceAddress(name string, localIP common.IPv4) error {
	return run("ifconfig", name, "inet", string(localIP), "remove")
}

// run 执行一条系统命令，失败时把 stdout/stderr 一并附进错误（排障需要
// ifconfig/route 的原文输出）。
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, string(output))
	}
	return nil
}
