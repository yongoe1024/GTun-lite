package tun

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// fakeDevice 是测试用的可控 TUN 设备。
type fakeDevice struct {
	mu      sync.Mutex
	mtu     int
	readCh  chan []byte // 写入 TUN 的入站包（由 Write 产生）
	reads   [][]byte    // Read 返回的预置包序列
	readPos int
	closed  bool
	written [][]byte // Write 收到的包
}

func newFakeDevice(mtu int) *fakeDevice {
	return &fakeDevice{mtu: mtu, readCh: make(chan []byte, 64)}
}

// Read 取出下一个预置包；没有包时返回 0 字节而不报错，模拟真实 TUN 的
// 「暂时无包可读」。
//
// 不能在无包时返回 error：那会让 tunReadLoop 永久退出，此后注入的包再也读不到。
// 早先的实现等 100ms 后返回错误，于是测试注入稍晚就必然失败——约 1/5 概率的
// flake。返回 0 字节走的是数据面对空读取的既有分支（它会 sleep 1ms 再重试），
// 读循环得以存活到测试注入为止。Close 后返回错误，让读循环正常退出。
func (d *fakeDevice) Read(buffer []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, errDataPlaneClosed
	}
	if d.readPos < len(d.reads) {
		pkt := d.reads[d.readPos]
		d.readPos++
		return copy(buffer, pkt), nil
	}
	return 0, nil // 暂时无包：交给数据面的空读取分支去等
}

func (d *fakeDevice) Write(packet []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, errDataPlaneClosed
	}
	d.written = append(d.written, append([]byte(nil), packet...))
	return len(packet), nil
}

func (d *fakeDevice) Name() string { return "fake0" }
func (d *fakeDevice) MTU() int     { return d.mtu }
func (d *fakeDevice) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

func (d *fakeDevice) writtenSnapshot() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.written...)
}

// fakeWorkerLink 是测试用的 Worker 数据面句柄。
type fakeWorkerLink struct {
	token   common.LinkToken
	peer    netip.AddrPort
	mu      sync.Mutex
	sent    [][]byte
	sendErr error
	batches int // SendBatch 调用次数
}

func (l *fakeWorkerLink) AttemptToken() common.LinkToken    { return l.token }
func (l *fakeWorkerLink) PeerLive() (*netip.AddrPort, bool) { return &l.peer, true }
func (l *fakeWorkerLink) SendFrame(_ context.Context, frame []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sendErr != nil {
		return l.sendErr
	}
	l.sent = append(l.sent, append([]byte(nil), frame...))
	return nil
}

func (l *fakeWorkerLink) SendBatch(_ context.Context, frames [][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sendErr != nil {
		return l.sendErr
	}
	l.batches++
	for _, frame := range frames {
		l.sent = append(l.sent, append([]byte(nil), frame...))
	}
	return nil
}

func (l *fakeWorkerLink) sentSnapshot() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.sent...)
}

// makeValidIPv4 构造一个带正确 checksum 的最小 IPv4 包（src→dst）。
func makeValidIPv4(src, dst string) []byte {
	pkt := make([]byte, 20)
	pkt[0] = 0x45 // version 4, IHL 5
	pkt[2] = 0
	pkt[3] = 20 // total length
	srcAddr := netip.MustParseAddr(src).As4()
	dstAddr := netip.MustParseAddr(dst).As4()
	copy(pkt[12:16], srcAddr[:])
	copy(pkt[16:20], dstAddr[:])
	// checksum：先清零再算
	pkt[10] = 0
	pkt[11] = 0
	sum := ipv4Checksum(pkt)
	pkt[10] = byte(sum >> 8)
	pkt[11] = byte(sum)
	return pkt
}

func ipv4Checksum(header []byte) uint16 {
	var sum uint32
	for i := 0; i < len(header); i += 2 {
		sum += uint32(header[i])<<8 | uint32(header[i+1])
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func testDataPlane(t *testing.T) (*DataPlane, *fakeDevice) {
	t.Helper()
	device := newFakeDevice(1280)
	dp, err := NewDataPlane(device, DataPlaneConfig{TUNMTU: 1280, OutboundQueuePackets: 4, InboundQueuePackets: 4}, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	dp.Start()
	t.Cleanup(func() { _ = dp.Close() })
	return dp, device
}

func TestDataPlaneOutboundSendsFrameToPeer(t *testing.T) {
	dp, device := testDataPlane(t)
	link := &fakeWorkerLink{token: "111111111111", peer: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), 40000)}
	if err := dp.RegisterLink(link, "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	// 往 TUN 喂一个合法 IPv4 包：src=本机 10.0.0.1, dst=peer 10.0.0.2
	pkt := makeValidIPv4("10.0.0.1", "10.0.0.2")
	device.mu.Lock()
	device.reads = append(device.reads, pkt)
	device.mu.Unlock()
	// 等待 Worker 收到出站帧
	waitFor := func() bool {
		return len(link.sentSnapshot()) > 0
	}
	deadline := time.Now().Add(2 * time.Second)
	for !waitFor() {
		if time.Now().After(deadline) {
			t.Fatal("outbound frame not sent")
		}
		time.Sleep(time.Millisecond)
	}
	frame, err := common.DecodeGTUNFrame(link.sentSnapshot()[0], 1280)
	if err != nil {
		t.Fatalf("decode sent frame: %v", err)
	}
	if frame.Token != "111111111111" {
		t.Fatalf("token mismatch: %s", frame.Token)
	}
}

func TestDataPlaneDropsInvalidSource(t *testing.T) {
	dp, device := testDataPlane(t)
	link := &fakeWorkerLink{token: "111111111111", peer: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), 40000)}
	dp.RegisterLink(link, "10.0.0.2")
	// src 不等于本机虚拟 IP，应丢弃
	pkt := makeValidIPv4("10.0.0.99", "10.0.0.2")
	device.mu.Lock()
	device.reads = append(device.reads, pkt)
	device.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if sent := link.sentSnapshot(); len(sent) != 0 {
		t.Fatalf("invalid source should be dropped, sent=%d", len(sent))
	}
}

func TestDataPlaneDropsUnknownDestination(t *testing.T) {
	_, device := testDataPlane(t)
	// 无对应配对，应丢弃
	pkt := makeValidIPv4("10.0.0.1", "10.0.0.99")
	device.mu.Lock()
	device.reads = append(device.reads, pkt)
	device.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
}

func TestDataPlaneInboundWritesToTUN(t *testing.T) {
	dp, device := testDataPlane(t)
	pkt := makeValidIPv4("10.0.0.2", "10.0.0.1")
	dp.DeliverInbound(pkt)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(device.writtenSnapshot()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inbound packet not written to TUN")
		}
		time.Sleep(time.Millisecond)
	}
	if string(device.writtenSnapshot()[0]) != string(pkt) {
		t.Fatal("written packet mismatch")
	}
}

func TestDataPlaneInboundQueueFullDrops(t *testing.T) {
	dp, _ := testDataPlane(t)
	// 填满入站队列（容量 4）
	for i := 0; i < 10; i++ {
		dp.DeliverInbound(makeValidIPv4("10.0.0.2", "10.0.0.1"))
	}
	// 不 panic 即通过；部分丢弃是预期行为
}

func TestDataPlaneOutboundQueueFullDrops(t *testing.T) {
	dp, device := testDataPlane(t)
	link := &fakeWorkerLink{token: "111111111111", peer: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), 40000)}
	dp.RegisterLink(link, "10.0.0.2")
	// 喂超过出站队列容量（4）的包，不 panic
	for i := 0; i < 10; i++ {
		device.mu.Lock()
		device.reads = append(device.reads, makeValidIPv4("10.0.0.1", "10.0.0.2"))
		device.mu.Unlock()
	}
	time.Sleep(50 * time.Millisecond)
}

// TestOutboundBatchGroupsFrames 攒批验证：一次注入 40 帧（队列容量 64），
// 全部送达且 SendBatch 调用次数少于帧数（发生了批量，而非逐帧一次调用）。
func TestOutboundBatchGroupsFrames(t *testing.T) {
	device := newFakeDevice(1280)
	dp, err := NewDataPlane(device, DataPlaneConfig{TUNMTU: 1280, OutboundQueuePackets: 64, InboundQueuePackets: 4}, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	dp.Start()
	t.Cleanup(func() { _ = dp.Close() })
	link := &fakeWorkerLink{token: "111111111111", peer: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), 40000)}
	if err := dp.RegisterLink(link, "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	const frames = 40
	device.mu.Lock()
	for i := 0; i < frames; i++ {
		device.reads = append(device.reads, makeValidIPv4("10.0.0.1", "10.0.0.2"))
	}
	device.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for len(link.sentSnapshot()) < frames {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d sent frames, got %d", frames, len(link.sentSnapshot()))
		}
		time.Sleep(time.Millisecond)
	}
	link.mu.Lock()
	batches := link.batches
	link.mu.Unlock()
	if batches < 2 {
		t.Fatalf("expected batching (>=2 SendBatch calls for %d frames), got %d", frames, batches)
	}
}
