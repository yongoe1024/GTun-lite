# 03 TUN 设备层（internal/tun）

## 1. 包定位

`internal/tun` 是 TUN 设备与系统路由的**平台抽象层**：跨平台的接口、路由预检、数据面在包根；三个平台的实现在子包 `internal/tun/mac`、`internal/tun/linux`、`internal/tun/win`。硬性约束（见 `internal/tun/doc.go`）：

- 不持有业务状态、不创建业务 goroutine；
- TUN/路由的全部变更由客户端 manager 作为 owner 串行调用；
- **接口层只暴露裸 IPv4 包**——平台差异（如 macOS 的 4 字节地址族头）在各实现内部消化。

```
internal/tun/
├── tun.go                # 接口：Device / Opener / RouteEntry / RouteCleanup / RouteTable
├── dataplane.go          # 跨平台数据面：TUN 读写循环 + 按目的地址分发
├── route.go              # 路由预检 Preflight（六类冲突）
├── route_system.go       # systemRouteTable 跨平台部分（LocalAddresses）
├── route_system_*.go     # 三平台的只读路由查询（供 preflight）
├── mac/tun_darwin.go     # macOS utun（内核控制套接字）
├── linux/tun_linux.go    # Linux /dev/net/tun + route_linux.go（下发命令）
└── win/tun_windows.go    # Windows wintun.dll + route_windows.go（netsh）
```

## 2. 接口层（`tun.go`）

```go
type Device interface {
    Read(p []byte) (int, error)   // 读一个裸 IPv4 包（平台前缀已由实现剥离）
    Write(p []byte) (int, error)  // 写一个裸 IPv4 包（实现负责补平台前缀）
    Name() string                 // 实际接口名（macOS 是 utunN 而非配置名）
    Close() error                 // 幂等
}

type Opener interface {
    Open(ctx, name, mtu, localIP string, peers []string) (Device, RouteCleanup, error)
}
```

`Opener.Open` 一步完成「开设备 + 配本机地址 + 装对端 /32 路由」并返回回滚句柄：

- `RouteEntry`：一条对端 `/32` 主机路由（Destination + Interface）。
- `RouteCleanup`：Apply 成功后的资源清单与回滚闭包（`closeImpl` 由平台实现经 `NewRouteCleanup` 注入）。Open 过程中任一步失败即回滚已建资源；成功后清单交给调用方，拆栈时调用。
- `PreflightInput` + `RouteTable`：`DefaultGateway / LocalAddresses / HasHostRoute` 三个只读查询，供预检。

## 3. TUN 读写的帧格式（逐平台）

这是平台差异最大的一层。跨平台契约：**`Device` 层的 Read/Write 一律是裸 IPv4 包**。

### 3.1 macOS（utun）——带 4 字节地址族头

内核 utun 设备的用户态读写帧，在 IP 包前有一个 **4 字节地址族（address family）前缀**：

```
写方向（Device.Write 内部拼接）：
 0          4
 +----------+---------------------------+
 | 00 00 00 02 |   IPv4 包（N 字节）       |
 +----------+---------------------------+
   大端 uint32 = AF_INET (2)

读方向（Device.Read 内部剥离）：
  读到 buffer[0:n]，解析前 4 字节：
  - == 2（AF_INET）  → memmove 把 buffer[4:n] 前移，返回 n-4 字节裸 IPv4
  - != 2（如 30=AF_INET6）→ 静默丢弃，返回 (0, nil)
  - n < 4             → 返回 (0, nil)
```

要点：

- 前缀是**大端序（网络字节序）的 uint32**，实现里由 `binary.BigEndian.PutUint32(b, afInet)` 预生成（`afInet = 2`）。
- 每次 Write 分配 `4+len(packet)` 新缓冲拼接后写 fd。
- IPv6 帧在读设备层即丢弃——GTun 数据面只处理 IPv4。
- 因此数据面给读缓冲多留 4 字节：`make([]byte, TUNMTU+4)`。

### 3.2 Linux（/dev/net/tun）——无包头

以 `IFF_TUN | IFF_NO_PI` 打开：TUN 模式（L3）且**去掉 4 字节包信息头**（`tun_pi`）。Read/Write 直接是裸 IPv4 包，无需任何前后缀处理。

### 3.3 Windows（wintun）——无包头

`WintunReceivePacket` 返回的就是「layer 3 IPv4 or IPv6 packet」裸包（环内存中 copy 出 `size` 字节）；发送用 `WintunAllocateSendPacket(len)` → copy → `WintunSendPacket`。无任何前缀。

## 4. 三平台打开实现

### 4.1 macOS：内核控制套接字（`mac/tun_darwin.go`）

1. `socket(AF_SYSTEM, SOCK_DGRAM, SYSPROTO_CONTROL)` 创建内核控制套接字。
2. `CTLIOCGINFO` ioctl 按控制名 `com.apple.net.utunControl` 查 provider ID。
3. `connect` 一个 `sockaddr_ctl` 指定 scUnit；**scUnit=N 对应接口 utun(N−1)**。
4. **unit 选择策略**：`ifconfig -l` 枚举现有 utun，从「最大编号 + 2」开始尝试。原因：configd 残留的「壳接口」若被直接连上，会复活壳 ifnet，数据面全静默。
5. fd 先 `SetNonblock(true)` 再 `os.NewFile`（见 §4.4 教训）。
6. 配置本机地址与 MTU（ifconfig 点对点形式，本机 IP 同时作 dst）。
7. 装对端 /32 主机路由。

清理特例：**先显式移除本机地址再关 fd**——内核拆接口是异步的，先关 fd 立即跑 preflight 会看到将死接口的地址，误报冲突。

### 4.2 Linux：/dev/net/tun（`linux/tun_linux.go`）

1. `unix.Open("/dev/net/tun", O_RDWR)` + `TUNSETIFF` ioctl 传 `ifreq{name[16], flags}`，flags = `IFF_TUN|IFF_NO_PI`。
2. 请求名被占用时内核另行分配，从 ifreq 读回实际名（`trimName`）。
3. 同样先非阻塞再 `os.NewFile`。
4. 配置（`linux/route_linux.go`）：本机地址**刻意按 /32 装**——避免内核自动生成整网段 connected route，把不属于本网络的路由也吸进隧道。

### 4.3 Windows：wintun.dll（`win/tun_windows.go`）

1. `sync.Once` 从**可执行文件同目录**加载 `wintun.dll`（`LoadDLL` + `FindProc`），DLL 随程序包分发（仓库 `wintun/bin/` 内含各架构预编译版）。
2. `WintunCreateAdapter(name, "GTun", NULL)` → `WintunStartSession(handle, 0x400000)`（4MB 接收环，合法范围 0x20000..0x4000000）→ `WintunGetReadWaitEvent(session)` 取读等待事件句柄。
3. 读：`WintunReceivePacket` 取环内指针与长度 → copy → `WintunReleaseReceivePacket`；`ERROR_NO_MORE_ITEMS`(259) 时 `WaitForSingleObject(readEvent, INFINITE)`。
4. 写：`WintunAllocateSendPacket(len)` → copy → `WintunSendPacket`；环满（`ERROR_BUFFER_OVERFLOW`）静默丢包。
5. 配置（`win/route_windows.go`）：netsh 配静态地址与 MTU、装对端 /32 路由。
6. **关闭四步排空协议**（真机 0xc0000005 崩溃教训，git 5b26371）：① `closed` CAS 置位 → ② `WintunEndSession` 唤醒等待 → ③ `inFlight.Wait()` 等在途 Read/Write 排空（每次调用全程 Add/Done）→ ④ 才 `WintunCloseAdapter` 释放内存。DLL 调用用 `syscall.SyscallN` 直调函数地址（vet 的 unsafe 白名单覆盖 SyscallN 而不覆盖 LazyProc.Call）。

### 4.4 两平台共同教训：fd 必须先非阻塞再 NewFile

阻塞 fd 交给 `os.NewFile` 不会进入 Go runtime poller，`Close()` 无法唤醒阻塞中的读循环——优雅退出永久卡死在 `wg.Wait()`（Go 文档化行为，golang/go#22939；Linux 阻塞 fd × Close 对照实验 ≥2s 不归）。macOS 曾因此用「阻塞读 + 弃置 goroutine」绕路，2026-08-27 对照实验推翻「非阻塞进 poller 后 kqueue 不投递事件」的旧观察后已统一切换为非阻塞 + poller，弃置机制（readerAbandoner 全套）删除。实验档案见 [10-真机验收记录.md](10-真机验收记录.md)。

## 5. 路由体系

### 5.1 设计：没有进程内路由表

这是容易误解的一点——本项目**没有**最长前缀匹配（LPM）的路由表实现：

- 每个对端虚拟 IP 只装一条 **/32 主机路由**进操作系统，LPM 完全交给内核；
- 进程内数据面按 dst IP 在 `map[netip.Addr]*workerQueue` 上**精确匹配**（`dataplane.go`）；
- 网络的 CIDR 只是配置标识字段（拓扑比对用），不用于下发路由。

### 5.2 路由预检（`route.go` 的 Preflight）

对 localIP + 全部 peer IP 逐个检查，任一命中即返回哨兵 `ErrRouteConflict`（`fmt.Errorf("%w: ...")` 携带具体地址）：

| # | 冲突类别 | 判定 |
|---|---|---|
| 1 | 保留地址 | 回环 / 链路本地 / 多播 / 未指定 |
| 2 | 等于服务器（或探测面）地址 | 防虚拟 IP 与控制/探测路径重叠 |
| 3 | 已存在的 /32 | 系统里已有指向该地址的主机路由 |
| 4 | 与默认网关冲突 | 虚拟 IP 等于网关地址 |
| 5 | 与本机已有地址冲突 | `LocalAddresses` 已含该地址 |
| 6 | 结构非法 | 非 IPv4 / 非单播 |

**残留路由分两层处理**（开栈前由 `CleanupDanglingHostRoutes` 先行清理）：

- **悬空残留（自动清理）**：异常退出（崩溃/强杀/断电）会留下指向**已拆接口**的 /32——绑定接口已消失，归属零歧义，开栈前自动删除，不再需要人工 `route delete`；
- **活跃冲突（如实报错）**：指向现存接口的 /32 一律报 ErrRouteConflict——可能是其他 VPN 的真实路由，清理责任仍交运维，不静默接管。判定失败（解析不出绑定接口）同样保守按冲突处理。

幂等的重应用不靠豁免，仍由 manager 的「拓扑未变不重建 + 重建前先拆干净」保证（见 [04-客户端.md](04-客户端.md)）。

### 5.3 平台只读查询（route_system*.go，供 preflight）

| 查询 | macOS | Linux | Windows |
|---|---|---|---|
| 默认网关 | `route -n get default` 解析 `gateway:` 行 | `ip route show default` 取 `via` | `route print -4` 中目的/掩码均 0.0.0.0 行 |
| /32 存在性 | `netstat -rn -f inet` 全表解析（裸 `<ip>` 或 `<ip>/32` 两种形态）。不能用 `route get`——它做 LPM，任何目的地都命中默认路由 | `ip route show <ip>/32` 输出非空即存在 | `route print -4` 中目的 == ip 且掩码 255.255.255.255 |
| 悬空判定 | netstat 行 Netif 列（按表头动态定位）的接口已不存在 | 路由行 `dev <name>` 的接口已不存在 | 路由 Interface 列无任何现存接口持有该地址，或该列不可解析为 IP（适配器已拆时的本地化占位词，中文系统为「默认」） |
| 悬空清理 | `route -q delete -host <ip>` | `ip route del <ip>/32` | `route delete <ip>` |
| 本机地址 | `net.Interfaces()` 过滤回环/down，收非回环非链路本地 IPv4（跨平台，`route_system.go`） | 同左 | 同左 |

Windows 解析做语言无关处理：前两列均可解析为 IPv4 即算数据行（应对本地化输出）。

### 5.4 下发与回滚命令汇总

| 平台 | 本机地址 | 对端 /32 | 回滚 |
|---|---|---|---|
| macOS | `ifconfig utunN inet <ip> <ip> mtu <m> up` | `route -q add -host <peer> -interface <iface>` | 先 `ifconfig ... inet <ip> remove`，再逐条 `route -q delete -host` |
| Linux | `ip addr add <ip>/32 dev` + `ip link set ... mtu ... up` | `ip route add <peer>/32 dev <iface>` | `ip route del <peer>/32 dev`（地址随接口拆除消失，无需单独清理） |
| Windows | `netsh interface ipv4 set address <name> static <ip> 255.255.255.255` + `set subinterface mtu=<m>` | `netsh interface ipv4 add route <peer>/32 <name>` | `netsh interface ipv4 delete route <peer>/32 <name>` |

## 6. 数据面（`dataplane.go`）

### 6.1 goroutine 拓扑

```
                 RegisterLink(peering, worker)
                        │ 启动
                        ▼
┌──────────┐  outbound chan  ┌─────────────────┐   GTUN 帧   ┌────────────┐
│tunReadLoop│ ─────────────▶ │ outboundSender  │ ──────────▶ │ Worker     │
│  (1 个)   │   (每链路)      │  (每链路 1 个)    │  WriteToUDP │ (UDP socket)│
└──────────┘                └─────────────────┘             └────────────┘
      ▲ DeliverInbound                                             │
      │                                                            ▼
┌─────────────── writeCh ─────────────────┐              Worker 接收 goroutine
│           tunWriteLoop (1 个)            │◀──────────── (deliverFrame, 见 04 篇)
└──────────────────────────────────────────┘
```

- **1 个 TUN 读循环** `tunReadLoop`；
- **每条已注册链路 1 个出站发送者** `outboundSender`（RegisterLink 时启动）；
- **1 个全局 TUN 写循环** `tunWriteLoop`。

队列：每链路出站 `outbound chan []byte`（容量 = `tunnel.outbound_queue`，默认 1024 包）；全局入站 `writeCh`（容量 = `tunnel.inbound_queue`，默认 1024 包）。溢出一律**丢新包**（不阻塞投递方、不挤掉旧包），逐包日志按秒节流（CAS 实现的每位置每秒至多一条）。

### 6.2 读循环

`device.Read` → `n < 20` 视为空读取，sleep 1ms 防 busy-spin → 复制包 → `common.ValidateIPv4Packet`（版本/IHL/总长/头校验和）→ 提取 src/dst（偏移 12–15 / 16–19）→ **src 必须等于本机虚拟 IP**（防伪造源）→ 持 `dp.mu` 按 dst 精确查 `links` map → 非阻塞入队或丢弃（未知目的丢弃，也是 Windows mDNS 多播渗入的防线）。

### 6.3 写循环与关闭顺序

写循环从 `writeCh` 取包直接 `device.Write`；写错误即退出（Error 级日志——这是「单向失灵」故障的唯一线索）。

关闭顺序是刻意安排的：置 closed → cancel ctx（停 outboundSender）→ **先 `device.Close` 再 `wg.Wait()`**。tunReadLoop 阻塞在 `device.Read` 时无法感知 ctx 取消，只有关设备（经 runtime poller）才能唤醒。

### 6.4 入站入口

`DeliverInbound(packet)`：无锁 try-send 进 `writeCh`，满则丢 + 节流 Warn。调用链来自 Worker 的四级验证之后（见 [04-客户端.md](04-客户端.md)），因此这里不再重复校验。

## 7. MTU 预算链

```
物理链路 MTU（典型 1500）
  − 20  外层 IPv4 头
  − 8   外层 UDP 头
  − 16  GTUN 帧头
  = 1456  → tun.mtu 上限，也是出厂默认（差路径手动下调，见 08）
TUN MTU + 4           → macOS 读缓冲（多留地址族头）
GTUNHeaderBytes + MTU → UDP 接收缓冲下限（见 05 篇）
MaxTUNMTU = 65535 − 44 = 65491（协议层理论最大）
```

超上限的 MTU 会让满尺寸包在物理链路被分片或丢弃，配置校验直接拒绝。
