package client

import (
	"context"
	"errors"
	"net/netip"
	"sync"

	"gtun-lite/internal/common"
	"gtun-lite/internal/tun"
)

// fakeDevice 是测试用 TUN 设备：注入包经 Read 送给数据面，
// 数据面写入的包被记录供断言。
type fakeDevice struct {
	mu       sync.Mutex
	injected chan []byte
	written  [][]byte
	closed   chan struct{}
	once     sync.Once
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{injected: make(chan []byte, 64), closed: make(chan struct{})}
}

func (device *fakeDevice) Read(buffer []byte) (int, error) {
	select {
	case packet := <-device.injected:
		copy(buffer, packet)
		return len(packet), nil
	case <-device.closed:
		return 0, errors.New("device closed")
	}
}

func (device *fakeDevice) Write(packet []byte) (int, error) {
	device.mu.Lock()
	defer device.mu.Unlock()
	select {
	case <-device.closed:
		return 0, errors.New("device closed")
	default:
	}
	device.written = append(device.written, append([]byte(nil), packet...))
	return len(packet), nil
}

func (device *fakeDevice) Name() string { return "fake0" }
func (device *fakeDevice) MTU() int     { return 1500 }

func (device *fakeDevice) Close() error {
	device.once.Do(func() { close(device.closed) })
	return nil
}

// inject 把一个包送进设备（等价于内核交给 TUN 一个出站包）。
func (device *fakeDevice) inject(packet []byte) {
	device.injected <- append([]byte(nil), packet...)
}

// takeWritten 取数据面写入的全部包（等价于内核从 TUN 收到入站包）。
func (device *fakeDevice) takeWritten() [][]byte {
	device.mu.Lock()
	defer device.mu.Unlock()
	out := make([][]byte, len(device.written))
	copy(out, device.written)
	return out
}

// fakeOpener 记录每次打开的设备供测试断言。
type fakeOpener struct {
	devices []*fakeDevice
}

func (opener *fakeOpener) Open(_ context.Context, _ string, _ int, _ common.IPv4, peers []common.IPv4) (tun.Device, tun.RouteCleanup, error) {
	device := newFakeDevice()
	opener.devices = append(opener.devices, device)
	cleanup := tun.NewRouteCleanup("fake0", nil, func() error { return nil })
	return device, cleanup, nil
}

// fakeRouteTable 报告一张空路由表：无默认网关、无 /32、无本机地址。
type fakeRouteTable struct{}

func (fakeRouteTable) DefaultGateway() (netip.Addr, bool, error) { return netip.Addr{}, false, nil }
func (fakeRouteTable) LocalAddresses() ([]netip.Addr, error)     { return nil, nil }
func (fakeRouteTable) HasHostRoute(netip.Addr) (bool, error)     { return false, nil }
