# 测试环境搭建（macOS / Linux / Windows / 公网机）

三平台真机测试的一次性环境设置清单。操作经验与工具坑见
[README.md](README.md)；本文只回答「一台新机器怎么弄到能测」。
文中地址均为占位：`<发起机LAN_IP>`、`root@<debian-ip>`、
`<win-user>@<win-ip>`、`root@<wan-ip>`，替换为实际机器后照做。

## macOS（本机，兼服务端/客户端测试机）

TUN 创建与 pf 故障注入都需要 root，脚本无法交互输密码——自动化测试前
先开免密 sudo（平时可删，属安全放宽面）：

```sh
echo 'yongoe ALL=(ALL) NOPASSWD: ALL' | sudo tee /etc/sudoers.d/999-gtun-test
# 收档清理：sudo rm /etc/sudoers.d/999-gtun-test
```

其余无需设置：系统自带 ssh 客户端（连另外两台）、pf（默认关闭，故障
注入时 `sudo pfctl -Ef -` 启用、用完 `sudo pfctl -d`）。

## Linux（Debian 测试机）

以 root 使用，两件事：

1. **ssh 免密**：把测试发起机的公钥追加到 `/root/.ssh/authorized_keys`
   （发起机 `cat ~/.ssh/id_ed25519.pub` 的内容）。
2. **确认 TUN 可用**：`ls /dev/net/tun` 存在即可（Debian 默认有）。

无需 sudoers（直接 root）；故障注入用 iptables，强杀 TCP 用
`ss -K dst <ip> dport = :<port>`（配方见 README 工具箱）。

## Windows 11（客户端测试机）

一次性设置（管理员 PowerShell，约 5 分钟）：

```powershell
# 1. OpenSSH 服务器（图形界面等价路径：设置→系统→可选功能）
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Start-Service sshd
Set-Service -Name sshd -StartupType Automatic

# 2. 放开令牌过滤：让管理员组用户经 ssh 拿到完整令牌（否则建不了网卡）
Set-ItemProperty -Path HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System -Name LocalAccountTokenFilterPolicy -Value 1

# 3. 公钥（注意是 ProgramData 路径，不是用户目录；权限不修则被静默忽略）
Add-Content -Path "C:\ProgramData\ssh\administrators_authorized_keys" -Value "<发起机公钥>"
icacls.exe "C:\ProgramData\ssh\administrators_authorized_keys" /inheritance:r /grant "SYSTEM:F" /grant "BUILTIN\Administrators:F"
```

要点（坑的细节见 README 的 Windows 节）：

- 账户须在管理员组（`net localgroup administrators` 核对）；
- **sshd 会话结束会杀子进程**：长驻进程必须用
  `schtasks /Create /TN x /TR <run.cmd> /SC ONCE /RL HIGHEST` + `/Run`
  分离（为何不能省见 README 的 Windows 节）；
- run.cmd 里显式重定向日志兜底（日志主路径是 logging 配置的文件）；
- 部署包：exe 与 wintun.dll 同目录（`make build-windows` 产出的整包）；
- 机器保持不睡眠。

## 公网服务器（跨 NAT / WAN 测试）

1. ssh 免密 root（同 Linux 节）；
2. `/dev/net/tun` 可用；
3. **端口规划**：部署前先查既有占用（Linux `ss -lntp`、Windows
   `netstat -ano`），与既有服务冲突就整段更换——real-wan.sh 默认用
   11000 段：控制 TCP 11000、探测 UDP 11000-11004、管理 127.0.0.1:19090
   （经 ssh 隧道访问）；
4. 云机安全组放行上述端口；
5. 云机上的第二客户端身份文件（gtun-device-id）跨轮持久——强制 link
   角色时以其 UUID 为比较基准（`GET /api/devices` 可查）。

## 网段与身份约定

- 测试建网固定 `10.206.x.0/24`（多网络往下排 10.206.1/2/3），避开
  物理 LAN 网段（网段重叠防线见 README 已知边界）；
- 设备身份文件随客户端持久，勿跨设备复制（同身份双开会互相顶替）；
- 判定出口 NAT 类型：手机热点=variable 源，家庭宽带=stable 但出口 IP
  可能按流轮换（不直接拒绝，见 [05-NAT穿透与打洞.md](../../设计文档/05-NAT穿透与打洞.md)）。
