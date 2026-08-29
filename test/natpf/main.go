//go:build linux

// natpf 是 pf 档实验用的用户态端口受限 NAT（仅 UDP）。与 conntrack 的
// 本质差异：按内网端点建映射（EIM，外部端口=内部端口）、入站按映射的
// 「发往过」名单过滤（端口受限/地址受限/全锥三档可配）、未匹配入站
// 静默丢弃且不留任何状态——负记录机制不存在。
//
// 数据路径：natgw 命名空间内，内核把 100.64/16 方向的 UDP 转发进 tun 设备，
// natpf 改写后用绑定在 (wan, 内部端口) 的本地 UDP socket 发出（无 NAT 分配
// 步骤，负记录无从下毒）；回程按目的端口反查映射、过滤后改写写回 tun。
// TCP/ICMP 不经本程序（natlab 里走 conntrack MASQUERADE，仅控制面）。
//
// 用法：natpf -tun tun0 -wan 100.64.2.100 -lan 10.0.2.0/24 -filter port
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type ifreq struct {
	name  [16]byte
	flags uint16
	_     [22]byte
}

// openTun 创建 L3 TUN 设备（IFF_TUN|IFF_NO_PI），非阻塞注册 poller。
func openTun(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	request := ifreq{flags: unix.IFF_TUN | unix.IFF_NO_PI}
	copy(request.name[:], name)
	if err := unix.IoctlSetInt(fd, unix.TUNSETIFF, int(uintptr(unsafe.Pointer(&request)))); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("set nonblock: %w", err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

// mapping 是一条 EIM 映射：内网端点 ↔ (wan, 外部端口)，外部端口保留自
// 内部端口。seen 是该端口的「发往过」名单，入站过滤的唯一依据。
type mapping struct {
	intIP   net.IP
	intPort uint16
	extPort uint16
	socket  *net.UDPConn
	seen    map[string]bool
	last    time.Time
}

type nat struct {
	tun    *os.File
	wan    net.IP
	lan    net.IPNet
	filter string // port | addr | full

	mu       sync.Mutex
	mappings map[string]*mapping // key: intIP:intPort
	drops    int
}

// seenKey 按过滤档位生成名单键。
func (n *nat) seenKey(ip net.IP, port uint16) string {
	if n.filter == "addr" {
		return ip.String()
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// outbound 处理内网→公网的 UDP 包：建映射、记名单、本地套接字发出。
func (n *nat) outbound(packet []byte) {
	srcIP, dstIP, srcPort, dstPort, payload, ok := parseUDP(packet)
	if !ok || !n.lan.Contains(srcIP) || n.lan.Contains(dstIP) {
		return // 非内网源/回环方向，不处理
	}
	key := fmt.Sprintf("%s:%d", srcIP, srcPort)
	n.mu.Lock()
	m, exists := n.mappings[key]
	if !exists {
		socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: n.wan, Port: int(srcPort)})
		if err != nil {
			n.mu.Unlock()
			log.Printf("bind %s:%d failed: %v", n.wan, srcPort, err)
			return
		}
		m = &mapping{
			intIP:   srcIP,
			intPort: srcPort,
			extPort: srcPort, // EIM：外部端口保留内部端口
			socket:  socket,
			seen:    map[string]bool{},
			last:    time.Now(),
		}
		n.mappings[key] = m
		go n.inboundLoop(m)
		log.Printf("mapping %s:%d -> %s:%d (%s)", srcIP, srcPort, n.wan, srcPort, n.filter)
	}
	m.seen[n.seenKey(dstIP, dstPort)] = true
	m.last = time.Now()
	socket := m.socket
	n.mu.Unlock()

	dst := &net.UDPAddr{IP: dstIP, Port: int(dstPort)}
	if _, err := socket.WriteToUDP(payload, dst); err != nil {
		log.Printf("send to %v: %v", dst, err)
	}
}

// inboundLoop 处理一条映射 socket 的回程：过滤（不在名单 → 静默丢）、
// 改写回内网地址、写 tun。
func (n *nat) inboundLoop(m *mapping) {
	buffer := make([]byte, 65535)
	for {
		length, src, err := m.socket.ReadFromUDP(buffer)
		if err != nil {
			return // 映射老化时 socket 被关闭
		}
		n.mu.Lock()
		m.last = time.Now()
		admit := n.filter == "full" || m.seen[n.seenKey(src.IP, uint16(src.Port))]
		if !admit {
			n.drops++
		}
		intIP, intPort := m.intIP, m.intPort
		n.mu.Unlock()
		if !admit {
			continue // 静默丢弃：与 conntrack 的本质区别——不留任何状态
		}
		packet := buildIPv4UDP(src.IP, uint16(src.Port), intIP, intPort, buffer[:length])
		if _, err := n.tun.Write(packet); err != nil {
			log.Printf("tun write: %v", err)
		}
	}
}

// ageLoop 周期清理不活跃映射（GTun 保活间隔远小于此值）。
func (n *nat) ageLoop() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		n.mu.Lock()
		var expired []string
		for key, m := range n.mappings {
			if time.Since(m.last) > 2*time.Minute {
				expired = append(expired, key)
			}
		}
		for _, key := range expired {
			n.mappings[key].socket.Close()
			delete(n.mappings, key)
			log.Printf("mapping %s expired (drops=%d)", key, n.drops)
		}
		n.mu.Unlock()
	}
}

// parseUDP 解析 IPv4+UDP 头，返回五元组与载荷。
func parseUDP(packet []byte) (srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte, ok bool) {
	if len(packet) < 28 || packet[0]>>4 != 4 {
		return
	}
	ihl := int(packet[0]&0x0f) * 4
	if len(packet) < ihl+8 || packet[9] != 17 {
		return
	}
	srcIP, dstIP = net.IP(packet[12:16]), net.IP(packet[16:20])
	srcPort = binary.BigEndian.Uint16(packet[ihl : ihl+2])
	dstPort = binary.BigEndian.Uint16(packet[ihl+2 : ihl+4])
	udpLen := int(binary.BigEndian.Uint16(packet[ihl+4 : ihl+6]))
	if udpLen < 8 || len(packet) < ihl+udpLen {
		return
	}
	return srcIP, dstIP, srcPort, dstPort, packet[ihl+8 : ihl+udpLen], true
}

// buildIPv4UDP 构造内网方向的 IP+UDP 包。UDP 校验和置 0（IPv4 合法），
// IP 头校验和必须正确（对端内核协议栈会验）。
func buildIPv4UDP(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, payload []byte) []byte {
	udpLen := 8 + len(payload)
	total := 20 + udpLen
	buf := make([]byte, total)
	binary.BigEndian.PutUint16(buf[20:22], srcPort)
	binary.BigEndian.PutUint16(buf[22:24], dstPort)
	binary.BigEndian.PutUint16(buf[24:26], uint16(udpLen))
	copy(buf[28:], payload)
	buf[0] = 0x45
	binary.BigEndian.PutUint16(buf[2:4], uint16(total))
	buf[8] = 64
	buf[9] = 17
	copy(buf[12:16], srcIP.To4())
	copy(buf[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(buf[10:12], checksum(buf[:20]))
	return buf
}

func checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

func main() {
	tunName := flag.String("tun", "tun0", "TUN 设备名（须已存在或由此创建）")
	wan := flag.String("wan", "", "公网侧绑定地址（NAT 后的内网身份）")
	lan := flag.String("lan", "", "内网源网段，如 10.0.2.0/24")
	filter := flag.String("filter", "port", "入站过滤档位：port(端口受限)/addr(地址受限)/full(全锥)")
	flag.Parse()
	if *wan == "" || *lan == "" {
		log.Fatal("-wan 与 -lan 必填")
	}
	_, lanNet, err := net.ParseCIDR(*lan)
	if err != nil {
		log.Fatalf("解析 -lan: %v", err)
	}
	if *filter != "port" && *filter != "addr" && *filter != "full" {
		log.Fatalf("非法 -filter: %s", *filter)
	}
	wanIP := net.ParseIP(*wan)
	if wanIP == nil {
		log.Fatalf("解析 -wan: %s", *wan)
	}

	tun, err := openTun(*tunName)
	if err != nil {
		log.Fatalf("打开 TUN: %v", err)
	}
	n := &nat{tun: tun, wan: wanIP, lan: *lanNet, filter: *filter, mappings: map[string]*mapping{}}
	log.Printf("natpf 启动: tun=%s wan=%s lan=%s filter=%s", *tunName, *wan, *lan, *filter)
	go n.ageLoop()

	go func() {
		buffer := make([]byte, 65535)
		for {
			length, err := tun.Read(buffer)
			if err != nil {
				log.Fatalf("tun read: %v", err)
			}
			n.outbound(buffer[:length])
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
