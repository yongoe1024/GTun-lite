//go:build windows

package win

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

const (
	// wintunDLLName 是 Wintun 适配器 DLL 的文件名，从程序运行目录加载。
	wintunDLLName = "wintun.dll"
	// wintunRingCapacity 是 Wintun 接收环容量（Wintun 要求在 0x20000..0x4000000）。
	wintunRingCapacity = 0x400000
	// errorNoMoreItems 是 WintunReceivePacket 空闲时的 GetLastError 值（ERROR_NO_MORE_ITEMS）。
	errorNoMoreItems = 259
	// waitInfinite 是 WaitForSingleObject 的无限等待超时值（INFINITE）。
	waitInfinite = 0xFFFFFFFF
	// waitFailed 是 WaitForSingleObject 的失败返回值（WAIT_FAILED，句柄失效等）。
	waitFailed = 0xFFFFFFFF
)

// opener 从程序目录加载 wintun.dll 创建 Wintun 适配器。
type opener struct{}

// New 返回 Windows 平台的 tun.Opener。DLL 不存在时 Open 返回明确错误。
func New() tun.Opener { return opener{} }

var (
	dllHandle *syscall.DLL
	dllErr    error
	dllOnce   sync.Once
)

// loadDLL 从程序可执行文件所在目录加载 wintun.dll。
func loadDLL() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	dllPath := filepath.Join(filepath.Dir(exe), wintunDLLName)
	handle, err := syscall.LoadDLL(dllPath)
	if err != nil {
		return fmt.Errorf("load %s from %s (DLL must be placed alongside the executable): %w", wintunDLLName, dllPath, err)
	}
	dllHandle = handle
	return nil
}

// proc 返回一个已加载的 Wintun 过程。
func proc(name string) (*syscall.Proc, error) {
	if dllHandle == nil {
		return nil, errors.New("wintun.dll not loaded")
	}
	return dllHandle.FindProc(name)
}

// errno 提取 Call 返回的错误值中的 syscall.Errno；Call 的 err 恒非 nil，errno==0 表示成功。
func errno(err error) syscall.Errno {
	if code, ok := err.(syscall.Errno); ok {
		return code
	}
	return syscall.Errno(1)
}

// Open 创建 Wintun 适配器并配置 /32 地址与路由。
func (opener) Open(_ context.Context, name string, mtu int, localIP common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	if err := localIP.Validate(); err != nil {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid local IP: %w", err)
	}
	if mtu < 20 || mtu > common.MaxTUNMTU {
		return nil, tun.RouteCleanup{}, fmt.Errorf("invalid MTU %d", mtu)
	}
	var initErr error
	dllOnce.Do(func() { initErr = loadDLL() })
	if initErr != nil {
		return nil, tun.RouteCleanup{}, initErr
	}
	createProc, err := proc("WintunCreateAdapter")
	if err != nil {
		return nil, tun.RouteCleanup{}, err
	}
	// WintunCreateAdapter(Name, TunnelType, RequestedGUID)。
	adapterName := utf16Ptr(name)
	tunnelType := utf16Ptr("GTun")
	handle, _, callErr := createProc.Call(uintptr(unsafe.Pointer(adapterName)), uintptr(unsafe.Pointer(tunnelType)), 0)
	if handle == 0 {
		return nil, tun.RouteCleanup{}, fmt.Errorf("WintunCreateAdapter: %w", callErr)
	}

	// 打开接收环会话并取其读等待事件句柄。
	startSession, err := proc("WintunStartSession")
	if err != nil {
		closeAdapter(handle)
		return nil, tun.RouteCleanup{}, err
	}
	session, _, callErr := startSession.Call(handle, uintptr(wintunRingCapacity))
	if session == 0 {
		closeAdapter(handle)
		return nil, tun.RouteCleanup{}, fmt.Errorf("WintunStartSession: %w", callErr)
	}
	getReadEvent, err := proc("WintunGetReadWaitEvent")
	if err != nil {
		endSession(session)
		closeAdapter(handle)
		return nil, tun.RouteCleanup{}, err
	}
	readEvent, _, _ := getReadEvent.Call(session)
	if readEvent == 0 {
		endSession(session)
		closeAdapter(handle)
		return nil, tun.RouteCleanup{}, errors.New("WintunGetReadWaitEvent returned null")
	}

	device := &device{handle: handle, session: session, readEvent: readEvent, name: name}
	if err := configureInterface(name, mtu, localIP); err != nil {
		device.Close()
		return nil, tun.RouteCleanup{}, fmt.Errorf("configure interface: %w", err)
	}
	routes := []tun.RouteEntry{}
	for _, peer := range peers {
		if err := addHostRoute(name, peer); err != nil {
			for _, r := range routes {
				_ = deleteHostRoute(r.Interface, r.Destination)
			}
			device.Close()
			return nil, tun.RouteCleanup{}, fmt.Errorf("add host route %s: %w", peer, err)
		}
		routes = append(routes, tun.RouteEntry{Destination: peer, Interface: name})
	}
	cleanup := tun.NewRouteCleanup(name, routes, func() error {
		var first error
		for _, route := range routes {
			if err := deleteHostRoute(route.Interface, route.Destination); err != nil && first == nil {
				first = err
			}
		}
		return first
	})
	return device, cleanup, nil
}

type device struct {
	handle    uintptr
	session   uintptr
	readEvent uintptr // 接收环读等待事件；空闲时 Read 阻塞在其上避免空转
	name      string
	// closed 与 inFlight 支撑关闭的排空协议，见 Close 的注释。
	closed   atomic.Bool
	inFlight sync.WaitGroup
}

// Read 从接收环取一个包；空闲时（ERROR_NO_MORE_ITEMS）阻塞在读事件上等待新包。
// 会话结束（Close 已调用 EndSession）时等待与取包都以错误返回，读循环退出。
//
// Windows 的会话/适配器是 DLL 管的内存而非内核 fd：CloseAdapter 释放后，
// 任何仍在途的 Wintun 调用都是访问已释放内存（真机崩溃实证：读循环活跃
// 处理噪声包时整栈重建，ReceivePacket 写已死会话 → 0xc0000005 写 0x58）。
// 因此 Read/Write 全程登记 inFlight，Close 排空后才允许 CloseAdapter。
func (d *device) Read(buffer []byte) (int, error) {
	if d.closed.Load() {
		return 0, errDeviceClosed
	}
	d.inFlight.Add(1)
	defer d.inFlight.Done()
	recv, err := procAddr("WintunReceivePacket")
	if err != nil {
		return 0, err
	}
	release, err := procAddr("WintunReleaseReceivePacket")
	if err != nil {
		return 0, err
	}
	for {
		if d.closed.Load() {
			return 0, errDeviceClosed
		}
		var size uint32
		// WintunReceivePacket(Session, *PacketSize) 返回包数据指针；空闲返回 NULL + ERROR_NO_MORE_ITEMS。
		// 经 syscall.SyscallN 调用：指针参数与返回值的 unsafe 转换在 vet 白名单内，
		// DLL 调用期间 GC 不会移动 size 与包内存（LazyProc.Call 无此保证）。
		packetPtr, _, callErr := syscall.SyscallN(recv, d.session, uintptr(unsafe.Pointer(&size)))
		if packetPtr != 0 {
			if int(size) > len(buffer) {
				syscall.SyscallN(release, d.session, packetPtr)
				return 0, errors.New("packet larger than buffer")
			}
			// packetPtr 指向 wintun 环内存（DLL 所有，不受 Go GC 管理），
			// uintptr→Pointer 转换恒安全；vet 的 unsafeptr 告警对此为误报。
			copy(buffer, unsafe.Slice((*byte)(unsafe.Pointer(packetPtr)), size))
			syscall.SyscallN(release, d.session, packetPtr)
			return int(size), nil
		}
		if errno(callErr) != errorNoMoreItems {
			return 0, callErr
		}
		// 空闲：等读事件（新包到达或会话结束唤醒）后重试。
		event, waitErr := syscall.WaitForSingleObject(syscall.Handle(d.readEvent), waitInfinite)
		if waitErr != nil {
			return 0, fmt.Errorf("wait for read event: %w", waitErr)
		}
		if event == waitFailed { // WAIT_FAILED：句柄已随会话结束失效
			return 0, errors.New("read wait event failed")
		}
	}
}

// Write 把包送入发送环：WintunAllocateSendPacket 分配后拷贝再 WintunSendPacket 提交。
// 与 Read 同受 inFlight 排空协议保护（见 Read 注释）。
func (d *device) Write(packet []byte) (int, error) {
	if d.closed.Load() {
		return 0, errDeviceClosed
	}
	d.inFlight.Add(1)
	defer d.inFlight.Done()
	alloc, err := procAddr("WintunAllocateSendPacket")
	if err != nil {
		return 0, err
	}
	ptr, _, callErr := syscall.SyscallN(alloc, d.session, uintptr(len(packet)))
	if ptr == 0 {
		return 0, callErr
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(packet)), packet) // 环内存同上，vet 误报
	send, err := procAddr("WintunSendPacket")
	if err != nil {
		return 0, err
	}
	syscall.SyscallN(send, d.session, ptr)
	return len(packet), nil
}

// Name 返回 Wintun 适配器名。
func (d *device) Name() string { return d.name }

// errDeviceClosed 在会话结束后由 Read/Write 返回，驱动各自循环退出。
var errDeviceClosed = errors.New("wintun device closed")

// Close 关闭设备，幂等。顺序是排空协议的关键，不能调整：
//  1. 置 closed 标志——新的与循环中的 Read/Write 尽快退出；
//  2. EndSession——唤醒阻塞在读事件上的等待（wintun 语义：结束后收发
//     以错误返回，内存仍然有效）；
//  3. 等 inFlight 排空——全部在途 DLL 调用离开；
//  4. CloseAdapter——此刻才释放会话与适配器内存。
//
// 曾经的顺序是 EndSession 后立即 CloseAdapter 并清零 session 字段：读循环
// 无同步地并发使用 session，活跃时撞上已释放内存或 NULL 句柄 → 访问违例
// （真机实证：整栈重建时 0xc0000005 写 0x58）。句柄字段此后不再清零。
func (d *device) Close() error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	endSession(d.session)
	d.inFlight.Wait()
	closeAdapter(d.handle)
	return nil
}

// endSession 结束接收环会话。
func endSession(session uintptr) {
	if end, err := proc("WintunEndSession"); err == nil {
		end.Call(session)
	}
}

// closeAdapter 关闭 Wintun 适配器句柄。
func closeAdapter(handle uintptr) {
	if closeProc, err := proc("WintunCloseAdapter"); err == nil {
		closeProc.Call(handle)
	}
}

// utf16Ptr 把 Go 字符串转成 Windows API 需要的 UTF-16 指针（忽略转换
// 失败：调用方只传 ASCII 设备名，失败时返回 nil 由 API 层报错）。
func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// procAddr 返回已解析的函数地址，供 syscall.SyscallN 直调
// （SyscallN 在 vet 的 unsafe 转换白名单内，LazyProc.Call 不在）。
func procAddr(name string) (uintptr, error) {
	p, err := proc(name)
	if err != nil {
		return 0, err
	}
	return p.Addr(), nil
}
