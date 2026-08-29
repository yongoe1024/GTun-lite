# 真机测试

本项目**不使用 Docker**：TUN 设备、路由表与 NAT 行为是验收对象本身，
容器会引入另一层网络栈与权限边界，真机直测才是被测事实。单元与协议
级测试在 `internal/` 各包内（`make check`），这里只放需要真机的端到端脚本。

新机器的一次性环境设置（ssh、免密、提权、端口规划）见
[ENVIRONMENT.md](ENVIRONMENT.md)；本文是操作经验与工具坑。同目录的
[测试清单.md](测试清单.md) 是全环节×全分支覆盖矩阵（还有什么没测），
[真机验收记录.md](真机验收记录.md) 是验收档案与残留项活清单（何时验过什么）。

## 脚本

| 脚本 | 拓扑 | 校验项 |
|---|---|---|
| `real-lan.sh <本机LAN_IP> <debian_ssh>` | 本机做服务端 + 本机/Debian 双客户端（同网段，无 NAT） | 双侧 punch connected、双向 ping 零丢、双向 8MB MD5、保活 70s |
| `real-wan.sh <公网ssh> [端口基值] [身份种子]` | 公网服务器做服务端（兼第二客户端），本机穿真实 NAT 打过去 | 双侧 punch connected、双向 ping 零丢、2MB MD5（CONNECT 自动重试，容忍出口 IP 轮换） |
| `natlab.sh <本机LAN_IP> <debian_ssh> [A..E]` | Debian 单机 netns 五档 NAT 矩阵（conntrack 两档 + 用户态 pf 档两档 + 全 conntrack 对照），服务端跑本机 | 链路建成/如实拒绝、双侧 punch connected、双向 ping 零丢；gtun-scan.log 归档（打洞级观察） |

两脚本任一校验失败即非零退出；需要本机免密 sudo、两台 ssh 目标免密 root。

## 真机测试的既知环境事实（踩坑记录）

- **传输测试的发送端用 python3，不要用 nc**：macOS/BSD nc 的 stdin 一到
  EOF（脚本里通常是 /dev/null）就向对端发 FIN，Debian 的 nc 把它当结束
  信号——大文件传输会假性卡在 8192 字节。这是工具陷阱，不是隧道缺陷。
- **家庭宽带可能按流轮换出口 IP**（实测同一 socket 的五个探测包从两个
  公网 IP 交替出去）：客户端以 PROBE_IP_CHANGED 如实拒绝，打洞成功率
  随之波动，属环境特性；判定代码是否有问题看双侧 punch connected 日志。
- **同一家庭路由器后的两台设备互打需要 hairpin**，家用路由器多不支持——
  该场景如实 PUNCH_TIMEOUT，不代表打洞代码有缺陷，跨 NAT 侧验证为准。
- **手机热点是可靠的 variable NAT 源**（蜂窝出口为对称型）；link 角色
  每轮随身份随机，覆盖两个分支需多轮或预写身份强制角色（方法见
  `测试清单.md` 开头的方法论节）。
- ssh 远端后台进程用 `(setsid nohup ... > log 2>&1 < /dev/null &)`；
  远端清理用 `pkill -x` 精确进程名（`-f` 会匹配到 ssh 命令行自身）。
- macOS 本机（脚本侧）没有普通用户 `setsid`：普通用户命令会随会话结束被
  带走（症状是本机服务器"神秘 shutting down"）。用 sudo 起进程可借独立
  进程组存活（本仓库脚本的做法），或 python 双 fork + `os.setsid()`。

## 故障注入工具箱（复现 checklist 的 A3/B4/C6/D3/D4/E4 类手测）

- macOS 丢包/黑洞（pf）：`echo "block in proto udp to any port 10002" | sudo pfctl -Ef -`，
  结束 `sudo pfctl -d`。启用前先 `sudo pfctl -F all` 清旧规则，否则历史
  规则一并生效（真机踩过）。
- Linux 丢包（iptables）：`iptables -A INPUT -p udp --dport <port> -j DROP`，
  复原 `-D` 同规则。
- 强杀已建立的 TCP（Debian）：`ss -K dst <ip> dport = :22`——**过滤器必须
  带 dport**，裸 `dst <ip>` 会连带杀掉自己的 ssh 会话（真机踩过）。

## Windows 真机操作经验（2026-08-27 沉淀，Win11 build 26200）

- **sshd 会话结束会杀掉子进程**（`Start-Process` 也逃不掉）：长驻进程用
  计划任务分离——`schtasks /Create /TN x /TR <run.cmd> /SC ONCE /RL HIGHEST`
  再 `/Run`。**`/RL HIGHEST` 不能省**：省了进程无提权，WintunCreateAdapter
  报 Access denied（ssh 的提权靠事先设过 LocalAccountTokenFilterPolicy=1）。
- run.cmd 显式重定向日志（客户端日志走 stderr）：
  `gtun-client.exe -config <绝对路径> > client.log 2> client.err`。
- cmd 输出是 GBK（中文显示乱码）：判定用英文锚点，或改用
  PowerShell `Select-String`/`Get-Content`。
- 工具替代：nc/md5sum 没有——传输用系统自带 `curl.exe`（下载或 `-T` 上传），
  哈希用 `Get-FileHash -Algorithm MD5`；随机数据用 PowerShell
  `RandomNumberGenerator`。Windows ping 的零丢判定锚定 `(0% loss)`。
- 强杀（`taskkill /F`，模拟崩溃）后对端 /32 路由悬空残留：**2026-08-28
  起客户端开栈前自动清理**（含中文系统 Interface 列显示「默认」的形态，
  真机验证）；指向活跃接口的 /32 仍如实报 ROUTE_CONFLICT 交运维。
- Windows 自身 mDNS（224.0.0.251）会渗入 gtun，数据面按「未注册对端」
  丢弃（Debug 日志可见），属正常防护，无需处理。

## 大流量测量踩坑（2026-08-28 对照实验，均为未解决的既知事实）

- **家用 AP 大流量过载窗口**：持续 GB 级 UDP 打满后，AP 连接跟踪过载，
  出现 2~8 分钟「ICMP 通、新建 TCP 全被丢弃」的窗口——控制面拨号超时、
  隧道被保活拆链，期间任何建链尝试都会 PUNCH_TIMEOUT/重连失败。对策：
  大流量测量用 ≤256MB 短载荷；遇到窗口等它自愈再继续，不要误判为代码
  缺陷（token 守卫与全量上报会按设计收敛）。
- **Win 客户端一次无声消亡（未解）**：一次 schtasks 启动的客户端在运行
  4 分钟后消失——无应用崩溃事件、无日志（当时 run.cmd 是 LF 行尾，日志
  重定向失效）。原因未明；下次 Windows 长跑前先配 `logging.file` /
  `error_file`（已支持落盘），复现时才有证据。
- **测试脚本 shebang 一律 `#!/bin/bash`**：zsh 不做变量分词（本文档早有
  记录，2026-08-28 用 `#!/bin/zsh` 写测量脚本又踩一次——`$SSH` 整串被
  当作命令名，runner 与轮询全部静默失效，症状是「结果文件永远不出现」）。
- **判定测试成败不要用 `go test | grep` 管道**：退出码属于 grep，失败会
  溜过 `&&` 链（本次曾因此混入一次带病提交）。分开执行或显式检查
  `${PIPESTATUS}`。
- **换分支二进制时同步核对测试 yaml**：测试目录 yaml 的 `mtu: 8000`
  配 main 分支二进制（上限 1456）会 CONFIG_INVALID 拒启——fail-fast
  行为正确，但容易误当成环境问题排查半天。
