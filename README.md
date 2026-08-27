# GTun-Lite

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-blue)
![License](https://img.shields.io/badge/License-MIT-green)

轻量级 P2P 虚拟局域网。设备经服务器协调完成 UDP NAT 穿透后**点对点直连**，
共享一个虚拟网段（如 `10.206.0.0/24`）；隧道数据不经服务器，建成后服务器
重启、宕机均不影响已建立的链路。

## 特性

- **纯 Go 实现**：无 CGO 静态编译，服务端 SQLite schema 与管理页均
  `go:embed` 内嵌，单二进制即完整部署
- **三平台数据面**：Linux（tun）/ macOS（utun）/ Windows（wintun）
- **按 NAT 画像选路的穿透策略**：探测五端口画像 → 直连 / helper 信标 /
  Range 邻域扫描，不可行组合预判拒绝，不做无谓尝试
- **零恢复流程**：链路状态为纯内存缓存，服务器重启后由客户端重连全量
  上报自动重建
- **Web 管理台**：建网、成员、配对、链路操作全图形化，失败原因中文
  直读，无需登服务器查日志

![管理页](设计文档/images/admin-console.png)

## 架构

```
┌────────┐   TCP 控制面 :10000（注册 / 配置推送 / 链路指令）  ┌────────┐
│ 客户端 A │◄────────────────────────────────────────────►│        │
└───┬────┘   UDP 探测 :10000-10004（NAT 画像回显）          │ 服务器  │
    │                                                    │        │
    │        TCP / UDP 同上                               └────────┘
┌───┴────┐                                                 ▲
│ 客户端 B │◄────────────────────────────────────────────────┘
└───┬────┘
    │
    └────────────► UDP 直连隧道（打洞成功后，数据不经服务器）
```

服务器承载控制面、探测反射器与管理 API，三者均为轻负载；管理 API 默认
只绑 `127.0.0.1:9090`。

## 快速开始

要求：一台具有公网 IP 的服务器；客户端对 TUN 设备有权限（macOS/Linux
需 root/sudo，Windows 需管理员）。

### 0. 获取程序

```bash
git clone https://github.com/yongoe1024/GTun-lite.git
cd GTun-lite
make build-all        # 需 Go 1.22+；产物：bin/{linux,darwin,darwin-arm64,windows}/
```

每个程序包内含二进制与默认配置（`server.yaml` / `client.yaml`），
服务端分发包到服务器、客户端分发包到各设备即可。

### 1. 服务器

放行防火墙/安全组：TCP `10000`、UDP `10000-10004`。

```bash
./gtun-server -config server.yaml        # 默认配置即可运行，空库自动建表
```

### 2. 客户端

`client.yaml` 中唯一必填项为 `server.addr`：

```yaml
server:
  addr: "your.server.example:10000"
```

```bash
sudo ./gtun-client -config client.yaml   # macOS / Linux
```

macOS 也可直接双击程序包内的 `gtun-client.command`（终端内提权，
日志实时显示）。

Windows 双击 `gtun-client.exe` 并确认 UAC 提权即可（exe 已内嵌管理员
manifest）；`wintun.dll` 须与 exe 同目录。
首次启动生成设备身份文件 `gtun-device-id`：务必备份，**不要跨设备复制**
（同一身份两处运行会互相顶替）。

### 3. 管理台建网

浏览器访问 `http://127.0.0.1:9090`（跨机访问走 SSH 隧道）：

1. 「网络」视图建网，网段建议选 `10.206.x.0/24` 等冷门段，避开物理 LAN
2. 网络详情中将设备加入成员，虚拟 IP 自动分配
3. 选择两台成员建立配对
4. 「链路」视图下发建链，状态 `CONNECTED` 即通

### 4. 验证

```bash
ping <对端虚拟IP>
```

> **Windows 注意**：Windows 防火墙默认拦截入站 ICMP，虚拟 IP 同样受此
> 规则约束——隧道正常但 ping 不通属预期。放行方式（管理员）：
>
> ```powershell
> netsh advfirewall firewall add rule name="Allow ICMPv4-In" protocol=icmpv4:8,any dir=in action=allow
> ```
>
> 或改用 TCP 端口访问（如 `Test-NetConnection <虚拟IP> -Port <端口>`）验证连通性。

## NAT 穿透矩阵

| 两侧 NAT 组合 | 穿透路径 | 预算 |
|---|---|---|
| stable × stable | 直连映射端点 | 5s |
| stable × variable | helper 信标 + 反向握手 + OK 补发为主路径；Range 邻域扫描备援 | 15s |
| variable × variable | 预判拒绝（`NAT_UNSUPPORTED`） | — |
| 出口 IP 按流轮换（部分家宽） | 探测即拒（`PROBE_IP_CHANGED`） | — |
| 同一路由器后互打 | 依赖路由器 hairpin/回环支持，家用路由器多数不支持 | — |

真机验收记录见 [设计文档/10-真机验收记录.md](设计文档/10-真机验收记录.md)。

## 安全边界

使用前请确认以下定位与你的场景匹配：

- **数据面不加密**：隧道为明文 UDP，适用于受信网络环境下的设备互联，
  不提供机密性与认证，不要将其暴露于不受信链路
- **控制面无鉴权**：管理 API 默认只绑 `127.0.0.1`，跨机管理应走 SSH
  隧道，不要改绑公网地址
- **设备身份即凭证**：`gtun-device-id` 文件等价于设备身份，须妥善保管

## 故障排查

| 现象 | 原因与处置 |
|---|---|
| Windows 虚拟 IP ping 不通，其余方向正常 | Windows 防火墙默认拦截入站 ICMP，见「快速开始 · 验证」的放行命令 |
| 链路页红字「出口 IP 轮换」 | 家宽按流轮换出口 IP，属环境特性；多重试几次或换网络（如蜂窝热点） |
| 「双方 NAT 均不可预测」 | 双 variable 组合无可用路径，环境上限 |
| 「打洞超时」 | NAT 严格或路径黑洞；可重试，或上调 `punch.helper_count` 档位（256/512/1024） |
| 设备显示离线 | 客户端进程退出或网络中断；恢复后 5s 内自动重连 |
| 链路停在 CONNECTING | 下发后双侧连接皆死时的已知边界，对该设备点「查询」或等其重连上报即收敛 |

完整边界清单见 [设计文档/08-构建配置与部署.md](设计文档/08-构建配置与部署.md)。

## 构建

```bash
make build-all    # 产物：bin/{linux,darwin,darwin-arm64,windows}/
make check        # go vet + go test -race
```

## 文档

| 文档 | 内容 |
|---|---|
| [设计文档](设计文档/README.md) | 按模块拆分的技术设计文档集（架构总览、协议层、TUN、客户端、NAT 穿透、服务端、存储与管理面、构建部署） |
| [构建配置与部署](设计文档/08-构建配置与部署.md) | 配置全表、跨端检查、备份恢复、已知边界 |
| [存储与管理面](设计文档/07-存储与管理面.md) | SQLite schema、管理 API 全部端点（管理台背后即此 API）、Web 控制台 |
| [测试清单](设计文档/09-测试清单.md) | 覆盖矩阵 |
| [验收记录](设计文档/10-真机验收记录.md) | 真机测试档案 |

## 许可

[MIT](LICENSE)
