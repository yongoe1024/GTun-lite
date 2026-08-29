#!/bin/sh
# 真机局域网验收（本机做服务端 + 本机/Debian 双客户端，同网段无 NAT）。
#
# 用法：real-lan.sh <本机LAN_IP> <debian_ssh目标>
#   例：./real-lan.sh 192.168.31.157 root@192.168.31.105
# 前提：本机免密 sudo；debian_ssh 免密登录且为 root；两机均无占用 10000-10004。
#
# 校验项（任一失败即非零退出）：
#   1. 双侧 punch connected——link1 侧也必须建成（曾因 OK 不重发在真实
#      网络必死，局域网时序紧凑测不出，故显式查双侧日志）
#   2. 双向 ping 零丢包
#   3. 双向 8MB 随机数据传输 MD5 一致（发送端用 python3，勿用 nc：
#      mac 端 nc 的 stdin 一到 EOF 就发 FIN，对端把它当结束——传输会
#      假性卡在 8192 字节，是工具陷阱不是隧道缺陷）
#   4. 链路保活 70 秒后仍 CONNECTED
set -e

LAN_IP=${1:?usage: real-lan.sh <lan-ip> <ssh-target>}
SSH_TARGET=${2:?usage: real-lan.sh <lan-ip> <ssh-target>}
SSH="ssh -o ConnectTimeout=5 $SSH_TARGET"
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN=/tmp/gtun-accept-lan
CIDR=10.206.0.0/24
VIP_A=10.206.0.1   # 本机（先入网，拿第一个地址）
VIP_B=10.206.0.2   # Debian

echo "== 构建与部署 =="
make -C "$ROOT" build-darwin-arm64 build-linux >/dev/null
$SSH 'mkdir -p /root/gtun-accept'
scp -q "$ROOT/bin/linux/client/gtun-client" "$SSH_TARGET:/root/gtun-accept/"

echo "== 配置（yaml 全量生成，避免 sed 改注释行的脆弱性）=="
rm -rf "$RUN"; mkdir -p "$RUN/server" "$RUN/a"
cat > "$RUN/server/server.yaml" <<EOF
control:
  bind: "0.0.0.0:10000"
  register_timeout: 10s
  heartbeat_timeout: 60s
  write_timeout: 5s
  max_connections: 1000
probe:
  bind: "0.0.0.0"
  base_port: 10000
admin:
  bind: "127.0.0.1:9090"
database:
  path: "$RUN/server/gtun.db"
limits:
  max_devices_per_network: 8
  min_cidr_prefix: 24
  max_cidr_prefix: 28
logging:
  level: "info"
EOF
cat > "$RUN/a/client.yaml" <<EOF
server:
  addr: "$LAN_IP:10000"
  probe_base_port: 10000
identity:
  path: "$RUN/a/gtun-device-id"
  name: "mac-a"
tun:
  name: "gtun0"
  mtu: 1456
tunnel:
  outbound_queue: 4096
  inbound_queue: 4096
control:
  heartbeat_interval: 20s
  register_timeout: 10s
  connect_timeout: 10s
  reconnect_interval: 5s
  write_timeout: 5s
probe:
  timeout: 30s
  per_port_timeout: 1s
  retries: 3
punch:
  stable_timeout: 2s
  variable_timeout: 15s
  helper_count: 256
logging:
  level: "debug"
EOF
$SSH "cat > /root/gtun-accept/client.yaml <<EOF
server:
  addr: \"$LAN_IP:10000\"
  probe_base_port: 10000
identity:
  path: \"/root/gtun-accept/gtun-device-id\"
  name: \"debian-b\"
tun:
  name: \"gtun0\"
  mtu: 1456
tunnel:
  outbound_queue: 4096
  inbound_queue: 4096
control:
  heartbeat_interval: 20s
  register_timeout: 10s
  connect_timeout: 10s
  reconnect_interval: 5s
  write_timeout: 5s
probe:
  timeout: 30s
  per_port_timeout: 1s
  retries: 3
punch:
  stable_timeout: 2s
  variable_timeout: 15s
  helper_count: 256
logging:
  level: \"debug\"
EOF"

echo "== 启动：服务器 → Debian B → 本机 A =="
# 幂等预清理：上次失败运行可能残留进程占住端口（管理 API 打到旧库会 409）。
sudo pkill -TERM -x gtun-client 2>/dev/null || true
pkill -TERM -x gtun-server 2>/dev/null || true
$SSH 'pkill -x -TERM gtun-client 2>/dev/null' || true
sleep 1
(cd "$RUN/server" && nohup "$ROOT/bin/darwin-arm64/server/gtun-server" -config server.yaml > server.log 2>&1 < /dev/null & echo $! > "$RUN/server.pid")
sleep 1
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:9090/ready | grep -q 200 || { echo "FAIL: 服务器未就绪"; tail -3 "$RUN/server/server.log"; exit 1; }
$SSH 'cd /root/gtun-accept && (setsid nohup ./gtun-client -config client.yaml > client.log 2>&1 < /dev/null &)'
sudo sh -c "cd $RUN/a && (nohup $ROOT/bin/darwin-arm64/client/gtun-client -config client.yaml > client.log 2>&1 < /dev/null & echo \$! > $RUN/a/client.pid)"
sleep 3

cleanup() {
    sudo pkill -TERM -x gtun-client 2>/dev/null || true
    $SSH 'pkill -x -TERM gtun-client 2>/dev/null' || true
    pkill -TERM -x gtun-server 2>/dev/null || true
}
trap cleanup EXIT

echo "== 入网 + 配对 + CONNECT =="
API=http://127.0.0.1:9090
A=$(curl -s $API/api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="mac-a"][0])')
B=$(curl -s $API/api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="debian-b" and d["online"]][0])')
NET=$(curl -s -X POST $API/api/networks -d "{\"name\":\"lan\",\"cidr\":\"$CIDR\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
curl -s -X POST $API/api/networks/$NET/members -d "{\"device_id\":\"$A\"}" >/dev/null
curl -s -X POST $API/api/networks/$NET/members -d "{\"device_id\":\"$B\"}" >/dev/null
curl -s -X POST $API/api/networks/$NET/peerings -d "{\"device_a\":\"$A\",\"device_b\":\"$B\"}" >/dev/null
curl -s -X POST $API/api/links/connect -d "{\"device_a\":\"$A\",\"device_b\":\"$B\"}" >/dev/null
sleep 3

STATE=$(curl -s $API/api/links | python3 -c 'import json,sys;print(json.load(sys.stdin)["links"][0]["state"])')
echo "link state: $STATE"
[ "$STATE" = "CONNECTED" ] || { echo "FAIL: 链路未建成"; grep -E "punch|failed" "$RUN/a/client.log" | tail -5; exit 1; }

echo "== 校验 1：双侧 punch connected =="
grep -q "punch connected" "$RUN/a/client.log" || { echo "FAIL: 本机侧无 punch connected 日志"; exit 1; }
$SSH 'grep -q "punch connected" /root/gtun-accept/client.log' || { echo "FAIL: Debian 侧（link1 路径）无 punch connected 日志"; exit 1; }
echo "双侧建成 OK"

echo "== 校验 2：双向 ping 零丢包 =="
ping -c 4 -i 0.3 $VIP_B | tail -2 | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: 本机→Debian 丢包"; exit 1; }
$SSH "ping -c 4 -i 0.3 $VIP_A | tail -2" | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: Debian→本机 丢包"; exit 1; }
echo "双向 ping OK"

echo "== 校验 3：双向 8MB 传输 MD5 =="
dd if=/dev/urandom of=$RUN/x.bin bs=1m count=8 2>/dev/null
WANT=$(md5 -q $RUN/x.bin)
# 本机 → Debian：本机 python 发送端绑本机 VIP，Debian nc 连过来接收
python3 - "$VIP_A" "$RUN/x.bin" <<'PYEOF' &
import socket, sys
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((sys.argv[1], 19090)); s.listen(1)
c, _ = s.accept()
with open(sys.argv[2], "rb") as f:
    while True:
        d = f.read(65536)
        if not d: break
        c.sendall(d)
c.close(); s.close()
PYEOF
sleep 1
GOT=$($SSH "nc -w 20 $VIP_A 19090 | md5sum | cut -d' ' -f1")
[ "$GOT" = "$WANT" ] || { echo "FAIL: 本机→Debian MD5 不一致 ($GOT != $WANT)"; exit 1; }
# Debian → 本机：Debian 生成数据并起 python 发送端，本机 nc 接收校验
$SSH 'dd if=/dev/urandom of=/root/gtun-accept/x.bin bs=1M count=8 2>/dev/null' >/dev/null
$SSH '(setsid nohup python3 -c "
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1)
s.bind((\"$VIP_B\",19091)); s.listen(1)
c,_=s.accept()
with open(\"/root/gtun-accept/x.bin\",\"rb\") as f:
    while True:
        d=f.read(65536)
        if not d: break
        c.sendall(d)
c.close(); s.close()
" > /dev/null 2>&1 &)'
sleep 1
nc -w 20 $VIP_B 19091 > $RUN/recv.bin
GOT=$(md5 -q $RUN/recv.bin)
WANT2=$($SSH 'md5sum /root/gtun-accept/x.bin | cut -d" " -f1')
[ "$GOT" = "$WANT2" ] || { echo "FAIL: Debian→本机 MD5 不一致"; exit 1; }
echo "双向 8MB MD5 OK"

echo "== 校验 4：保活 70 秒 =="
sleep 70
STATE=$(curl -s $API/api/links | python3 -c 'import json,sys;print(json.load(sys.stdin)["links"][0]["state"])')
[ "$STATE" = "CONNECTED" ] || { echo "FAIL: 70 秒后链路掉到 $STATE"; exit 1; }
echo "保活 OK"

echo "== 全部通过（LAN）=="
