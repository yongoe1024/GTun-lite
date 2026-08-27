#!/bin/sh
# 真机公网验收（公网服务器做服务端 + 本机客户端穿真实 NAT 打到公网机上的
# 第二客户端）。局域网另一台设备与本机同一出口、覆盖面相同，不参与本脚本。
#
# 用法：real-wan.sh <公网ssh目标> [端口基值] [本机身份种子文件]
#   例：./real-wan.sh root@<公网机IP> 11000
#   例：./real-wan.sh root@<公网机IP> 11000 /tmp/seed-link1   # 强制角色
# 身份种子（可选）：启动本机客户端前复制为 gtun-device-id，用于强制 link
#   角色——UUID 小于对端身份=link0、大于=link1（方法论见 设计文档/09-测试清单.md
#   开头；本脚本每轮重建随机身份，不预写则角色随机）。
# 前提：公网 ssh 免密 root 且有 /dev/net/tun；本机免密 sudo；
#   安全组放行 端口基值(TCP) 与 端口基值..+4(UDP)。
#
# 环境须知（真机实测结论，见 设计文档/10-真机验收记录.md）：
#   - 家庭宽带可能按流轮换出口 IP：PROBE_IP_CHANGED 属环境如实拒绝，
#     脚本对 CONNECT 做最多 N 次重试，轮到一致出口即通。
#   - 两客户端在同一家庭路由器后时互打需 hairpin（多不支持，如实失败），
#     因此本脚本验证的是 本机↔公网机 的跨 NAT 链路。
# 校验项：双侧 punch connected、双向 ping 零丢、2MB MD5 一致。
set -e

WAN_SSH_TARGET=${1:?usage: real-wan.sh <wan-ssh> [base-port]}
BASE_PORT=${2:-11000}
WAN_SSH="ssh -o ConnectTimeout=8 $WAN_SSH_TARGET"
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN=/tmp/gtun-accept-wan
WAN_DIR=/root/gtun-accept-wan
CIDR=10.208.0.0/24
ATTEMPTS=8
# 服务器公网地址从 ssh 目标解析
WAN_HOST=${WAN_SSH_TARGET#*@}

echo "== 构建与部署 =="
make -C "$ROOT" build-darwin-arm64 build-linux >/dev/null
$WAN_SSH "mkdir -p $WAN_DIR"
scp -q "$ROOT/bin/linux/gtun-server" "$ROOT/bin/linux/gtun-client" "$WAN_SSH_TARGET:$WAN_DIR/"

echo "== 配置（端口基值 $BASE_PORT，避开服务器上既有服务的 10000/9090）=="
rm -rf "$RUN"; mkdir -p "$RUN/a"
$WAN_SSH "cat > $WAN_DIR/server.yaml <<EOF
control:
  bind: \"0.0.0.0:$BASE_PORT\"
probe:
  bind: \"0.0.0.0\"
  base_port: $BASE_PORT
admin:
  bind: \"127.0.0.1:19090\"
database:
  path: \"$WAN_DIR/gtun.db\"
EOF
cat > $WAN_DIR/client.yaml <<EOF
server:
  addr: \"$WAN_HOST:$BASE_PORT\"
  probe_base_port: $BASE_PORT
identity:
  path: \"$WAN_DIR/gtun-device-id\"
  name: \"wan-node\"
EOF"
cat > "$RUN/a/client.yaml" <<EOF
server:
  addr: "$WAN_HOST:$BASE_PORT"
  probe_base_port: $BASE_PORT
identity:
  path: "$RUN/a/gtun-device-id"
  name: "mac-a"
EOF
if [ -n "$3" ]; then
    sudo cp "$3" "$RUN/a/gtun-device-id"
    echo "== 已预写本机身份（强制角色）：$(sudo cat "$RUN/a/gtun-device-id") =="
fi

echo "== 启动：公网服务器（兼第二客户端）→ 本机 =="
# 幂等预清理：公网机上可能残留上次运行的 server/client（残留 server 会以旧库
# 应答管理 API、新 server 因端口冲突 fail-fast 退出，表现为莫名的 409/空响应）。
$WAN_SSH 'pkill -x -TERM gtun-server 2>/dev/null; pkill -x -TERM gtun-client 2>/dev/null' || true
sudo pkill -TERM -x gtun-client 2>/dev/null || true
sleep 1
$WAN_SSH "cd $WAN_DIR && rm -f gtun.db && (setsid nohup ./gtun-server -config server.yaml > server.log 2>&1 < /dev/null & echo \$! > server.pid)"
sleep 1
$WAN_SSH "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19090/ready" | grep -q 200 || { echo "FAIL: 公网服务器未就绪"; $WAN_SSH "tail -3 $WAN_DIR/server.log"; exit 1; }
$WAN_SSH "cd $WAN_DIR && (setsid nohup ./gtun-client -config client.yaml > node.log 2>&1 < /dev/null &)"
sudo sh -c "cd $RUN/a && (nohup $ROOT/bin/darwin-arm64/gtun-client -config client.yaml > client.log 2>&1 < /dev/null & echo \$! > $RUN/a/client.pid)"
sleep 3

cleanup() {
    sudo pkill -TERM -x gtun-client 2>/dev/null || true
    $WAN_SSH 'cd /root/gtun-accept-wan && (pkill -x -TERM gtun-client 2>/dev/null; kill $(cat server.pid) 2>/dev/null) || true' || true
}
trap cleanup EXIT

echo "== 入网 + 配对（本机 ↔ 公网机，跨 NAT）=="
# 管理 API 只绑公网机的 127.0.0.1，全部经 ssh 隧道调用。
api_get()  { $WAN_SSH "curl -s http://127.0.0.1:19090$1"; }
api_post() { $WAN_SSH "curl -s -X POST http://127.0.0.1:19090$1 -d '$2'"; }
A=$(api_get /api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="mac-a"][0])')
W=$(api_get /api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="wan-node"][0])')
NET=$(api_post /api/networks "{\"name\":\"wan\",\"cidr\":\"$CIDR\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
api_post /api/networks/$NET/members "{\"device_id\":\"$A\"}" >/dev/null
api_post /api/networks/$NET/members "{\"device_id\":\"$W\"}" >/dev/null
api_post /api/networks/$NET/peerings "{\"device_a\":\"$A\",\"device_b\":\"$W\"}" >/dev/null

echo "== CONNECT 重试（出口 IP 轮换环境下可能数次 PROBE_IP_CHANGED，属正常）=="
CONNECTED=0
i=1
while [ $i -le $ATTEMPTS ]; do
    api_post /api/links/connect "{\"device_a\":\"$A\",\"device_b\":\"$W\"}" >/dev/null
    sleep 6
    STATE=$(api_get /api/links | python3 -c "import json,sys;ls=[l for l in json.load(sys.stdin)['links'] if {l['name_a'],l['name_b']}=={'mac-a','wan-node'}];print(ls[0]['state'] if ls else 'none')")
    echo "  尝试 $i/$ATTEMPTS: $STATE"
    if [ "$STATE" = "CONNECTED" ]; then CONNECTED=1; break; fi
    i=$((i+1))
done
[ $CONNECTED -eq 1 ] || { echo "FAIL: $ATTEMPTS 次尝试未建成（若日志全是 PROBE_IP_CHANGED，为出口轮换环境所致，非代码缺陷）"; tail -3 "$RUN/a/client.log"; exit 1; }

echo "== 校验 1：双侧 punch connected =="
grep -q "punch connected" "$RUN/a/client.log" || { echo "FAIL: 本机侧未建成"; exit 1; }
# 对端建成可能晚于本机（variable 侧首毫秒建成、stable 侧扫描自
# helperInitWindow 后才起步），在其打洞预算（15s）内轮询等待。
CLOUD_OK=0
i=1
while [ $i -le 10 ]; do
    if $WAN_SSH "grep -q 'punch connected' $WAN_DIR/node.log"; then CLOUD_OK=1; break; fi
    sleep 2
    i=$((i+1))
done
if [ $CLOUD_OK -ne 1 ]; then
    echo "FAIL: 公网机（link1 路径）未建成"
    $WAN_SSH "tail -3 $WAN_DIR/node.log"
    exit 1
fi
echo "双侧建成 OK"

echo "== 校验 2：双向 ping 零丢包 =="
VIP_A=10.208.0.1; VIP_W=10.208.0.2
# 匹配必须锚定「, 0」开头：曾经的 "0(\.0)?% packet loss" 会命中
# "100.0% packet loss" 的子串，100% 丢包被误判为通过（真机抓出）。
ping -c 4 -i 0.3 $VIP_W | tail -2 | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: 本机→公网机 丢包"; exit 1; }
$WAN_SSH "ping -c 4 -i 0.3 $VIP_A | tail -2" | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: 公网机→本机 丢包"; exit 1; }
echo "双向 ping OK"

echo "== 校验 3：2MB 传输 MD5（python 发送端，避开 nc 半关闭陷阱）=="
$WAN_SSH "dd if=/dev/urandom of=$WAN_DIR/x.bin bs=1M count=2 2>/dev/null && md5sum $WAN_DIR/x.bin | cut -d' ' -f1 > $WAN_DIR/x.md5 && (setsid nohup python3 -c \"
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1)
s.bind(('$VIP_W',19092)); s.listen(1)
c,_=s.accept()
with open('$WAN_DIR/x.bin','rb') as f:
    while True:
        d=f.read(65536)
        if not d: break
        c.sendall(d)
c.close(); s.close()
\" > /dev/null 2>&1 &)"
sleep 1
nc -w 60 $VIP_W 19092 > $RUN/recv.bin
GOT=$(md5 -q $RUN/recv.bin)
WANT=$($WAN_SSH "cat $WAN_DIR/x.md5")
[ "$GOT" = "$WANT" ] || { echo "FAIL: MD5 不一致 ($GOT != $WANT)"; exit 1; }
echo "2MB MD5 OK"

echo "== 全部通过（WAN）=="
