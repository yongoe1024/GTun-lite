#!/bin/sh
# netns NAT 实验室验收（Debian 单机双 NAT 网关 + 本机 macOS 服务端）。
#
# 用法：./natlab.sh <本机LAN_IP> <debian_ssh目标> [组合...]
#   例：./natlab.sh 192.168.31.157 root@192.168.31.105
#   组合参数为 nat 类型对（natgw1,natgw2），默认跑全部三种：
#     A=stable,stable      B=variable,stable      C=variable,variable
#   stable=MASQUERADE（端口保留 → 端口受限锥）
#   variable=MASQUERADE --random-fully（逐流随机 → 对称）
#
# 拓扑（全部在 Debian 内的 netns，宿主机只做转发）：
#   client1(10.0.1.100) - natgw1(100.64.1.100) - inet(公网路由器) - natgw2 - client2
#   服务端跑本机；本机需 route 100.64.0.0/16 → debian。
#
# 前提：debian ssh 免密 root、本机免密 sudo；两机端口 10000-10004 空闲。
# 每组合独立 server 实例（独立 db），客户端身份文件跨组合持久在 debian。
# 日志归档 /tmp/gtun-natlab/<组合>/。
set -u
LAN_IP=${1:?usage: natlab.sh <lan-ip> <ssh-target> [A B C]}
SSH_TARGET=${2:?usage: natlab.sh <lan-ip> <ssh-target> [A B C]}
shift 2
COMBOS=${*:-"A B C"}
SSH="ssh -o ConnectTimeout=5 $SSH_TARGET"
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN=/tmp/gtun-natlab
CIDR=10.206.0.0/24
API=http://127.0.0.1:9090
D=/root/gtun-natlab
mkdir -p "$RUN"

setup_lab() {
    $SSH 'bash -s' <<'REMOTE'
set -e
# 幂等：整组重建
for ns in client2 natgw2 client1 natgw1 inet; do ip netns del $ns 2>/dev/null || true; done
ip link del h0 2>/dev/null || true

ip netns add inet
ip netns add natgw1; ip netns add client1
ip netns add natgw2; ip netns add client2
ip link add h0 type veth peer name i9
ip link add w1 type veth peer name e1
ip link add l1 type veth peer name c1
ip link add w2 type veth peer name e2
ip link add l2 type veth peer name c2
ip link set i9 netns inet
ip link set w1 netns natgw1; ip link set e1 netns inet
ip link set l1 netns natgw1; ip link set c1 netns client1
ip link set w2 netns natgw2; ip link set e2 netns inet
ip link set l2 netns natgw2; ip link set c2 netns client2

# inet：模拟公网路由器
ip -n inet addr add 10.63.0.3/24 dev i9
ip -n inet addr add 100.64.1.1/24 dev e1
ip -n inet addr add 100.64.2.1/24 dev e2
ip -n inet link set lo up; ip -n inet link set i9 up
ip -n inet link set e1 up; ip -n inet link set e2 up
ip -n inet route add default via 10.63.0.254
ip netns exec inet sysctl -qw net.ipv4.ip_forward=1

# 宿主机：把"公网"接入局域网侧
ip addr add 10.63.0.254/24 dev h0
ip link set h0 up
ip route replace 100.64.1.0/24 via 10.63.0.3 dev h0
ip route replace 100.64.2.0/24 via 10.63.0.3 dev h0
sysctl -qw net.ipv4.ip_forward=1
iptables -C FORWARD -i h0 -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i h0 -j ACCEPT
iptables -C FORWARD -o h0 -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -o h0 -j ACCEPT

for i in 1 2; do
  ip -n natgw$i addr add 100.64.$i.100/24 dev w$i
  ip -n natgw$i addr add 10.0.$i.1/24    dev l$i
  ip -n natgw$i link set lo up
  ip -n natgw$i link set w$i up; ip -n natgw$i link set l$i up
  ip -n natgw$i route add default via 100.64.$i.1
  ip netns exec natgw$i sysctl -qw net.ipv4.ip_forward=1
  ip -n client$i addr add 10.0.$i.100/24 dev c$i
  ip -n client$i link set lo up; ip -n client$i link set c$i up
  ip -n client$i route add default via 10.0.$i.1
done
echo lab-ready
REMOTE
}

set_nat() {
    ns=$1 t=$2
    if [ "$t" = slirp ]; then
        # pf 档：slirp4netns 用户态 NAT 替代 conntrack——按内网端点保留
        # 外部端口（EIM）、未匹配入站静默丢且不留记录。需要 Debian 装
        # slirp4netns。拆掉 l/c veth（网关不再转发），tap+静态网关顶上；
        # 出向绑 100.64.$ns.1（Mac 的 100.64/16 回程路由覆盖）。
        $SSH "ip netns exec natgw$ns iptables -t nat -F 2>/dev/null
            ip -n natgw$ns link del l$ns 2>/dev/null
            ip -n client$ns link del tap9 2>/dev/null
            pkill -x slirp4netns 2>/dev/null
            sleep 0.5
            ip -n client$ns tuntap add mode tap tap9
            ip -n client$ns addr add 10.0.$ns.100/24 dev tap9
            ip -n client$ns link set tap9 up
            ip -n client$ns route add default via 10.0.$ns.2
            ip netns exec inet slirp4netns --outbound-addr=100.64.$ns.1 --netns-type=path /run/netns/client$ns tap9 >/tmp/slirp$ns.log 2>&1 &
            sleep 1" \
            || { echo "FAIL: natgw$ns slirp 档启动失败"; return 1; }
        return 0
    fi
    # conntrack 档：若上一组合用过 slirp 档，先恢复 l/c veth 拓扑
    $SSH "if ! ip -n natgw$ns link show l$ns >/dev/null 2>&1; then
        ip -n client$ns link del tap9 2>/dev/null
        pkill -x slirp4netns 2>/dev/null
        ip link add l$ns type veth peer name c$ns
        ip link set l$ns netns natgw$ns; ip link set c$ns netns client$ns
        ip -n natgw$ns addr add 10.0.$ns.1/24 dev l$ns
        ip -n natgw$ns link set l$ns up
        ip -n client$ns addr add 10.0.$ns.100/24 dev c$ns
        ip -n client$ns link set c$ns up
        ip -n client$ns route add default via 10.0.$ns.1
    fi"
    if [ "$t" = variable ]; then
        rule="MASQUERADE --random-fully"
    else
        rule="MASQUERADE"
    fi
    $SSH "ip netns exec natgw$ns iptables -t nat -F && ip netns exec natgw$ns iptables -t nat -A POSTROUTING -o w$ns -j $rule" \
        || { echo "FAIL: natgw$ns 设置 $t 规则失败"; return 1; }
}

parse() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)" 2>/dev/null; }

run_combo() {
    name=$1; t1=$2; t2=$3
    echo ""
    echo "===== 组合 $name: natgw1=$t1 natgw2=$t2 ====="
    OUT="$RUN/$name"; rm -rf "$OUT"; mkdir -p "$OUT"; echo "RUNNING" > "$OUT/verdict"
    # 先杀上一组合的客户端：scp 覆盖运行中的二进制会 ETXTBSY。
    # 必须按进程名杀主机全量——上一轮 setup_lab 重建 netns 后，旧客户端
    # 会变成 ip netns pids 看不见的僵尸，仍占着二进制。
    $SSH "pkill -x gtun-client" 2>/dev/null
    sleep 1
    set_nat 1 "$t1" || return 1
    set_nat 2 "$t2" || return 1

    echo "-- 冒烟：client1 到服务端连通"
    $SSH 'ip netns exec client1 ping -c1 -W2 '"$LAN_IP"' >/dev/null 2>&1' \
        || { echo "RESULT $name: FAIL 冒烟不通（宿主机转发或路由问题）"; echo "FAIL 冒烟" > "$OUT/verdict"; return 1; }

    echo "-- 部署客户端配置"
    $SSH "mkdir -p $D/c1 $D/c2"
    scp -q "$ROOT/bin/linux/client/gtun-client" "$SSH_TARGET:$D/gtun-client" || return 1
    for c in 1 2; do
        cat > "$OUT/c$c.client.yaml" <<EOF
server:
  addr: "$LAN_IP:10000"
  probe_base_port: 10000
identity:
  path: "$D/c$c/gtun-device-id"
  name: "nat-c$c"
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
        scp -q "$OUT/c$c.client.yaml" "$SSH_TARGET:$D/c$c/client.yaml" || return 1
    done

    echo "-- 启动 server（独立实例 + 独立 db）"
    pkill -TERM -x gtun-server 2>/dev/null; sleep 1
    cat > "$OUT/server.yaml" <<EOF
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
  path: "$OUT/server/gtun.db"
limits:
  max_devices_per_network: 8
  min_cidr_prefix: 24
  max_cidr_prefix: 28
logging:
  level: "info"
EOF
    mkdir -p "$OUT/server"
    (cd "$OUT/server" && nohup "$ROOT/bin/darwin-arm64/server/gtun-server" -config "$OUT/server.yaml" > "$OUT/server.log" 2>&1 < /dev/null &)
    sleep 1
    curl -s -o /dev/null http://127.0.0.1:9090/ready || { echo "RESULT $name: FAIL server 未就绪"; tail -3 "$OUT/server.log"; echo "FAIL server" > "$OUT/verdict"; return 1; }

    echo "-- 启动双客户端（netns 内）"
    $SSH "ip netns exec client1 sh -c 'cd $D/c1 && setsid nohup $D/gtun-client -config client.yaml >client.log 2>&1 </dev/null &'
        ip netns exec client2 sh -c 'cd $D/c2 && setsid nohup $D/gtun-client -config client.yaml >client.log 2>&1 </dev/null &'"
    sleep 4

    echo "-- 入网 + 配对 + CONNECT"
    A=$(curl -s $API/api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="nat-c1" and d["online"]][0])' 2>/dev/null) \
        || { echo "RESULT $name: FAIL nat-c1 未上线"; $SSH "tail -5 $D/c1/client.log"; echo "FAIL 未上线" > "$OUT/verdict"; return 1; }
    B=$(curl -s $API/api/devices | python3 -c 'import json,sys;print([d["device_id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="nat-c2" and d["online"]][0])' 2>/dev/null) \
        || { echo "RESULT $name: FAIL nat-c2 未上线"; $SSH "tail -5 $D/c2/client.log"; echo "FAIL 未上线" > "$OUT/verdict"; return 1; }
    NET=$(curl -s -X POST $API/api/networks -d "{\"name\":\"nat-$name\",\"cidr\":\"$CIDR\"}" | parse '["id"]') || return 1
    curl -s -X POST $API/api/networks/$NET/members -d "{\"device_id\":\"$A\"}" >/dev/null
    curl -s -X POST $API/api/networks/$NET/members -d "{\"device_id\":\"$B\"}" >/dev/null
    curl -s -X POST $API/api/networks/$NET/peerings -d "{\"device_a\":\"$A\",\"device_b\":\"$B\"}" >/dev/null
    curl -s -X POST $API/api/links/connect -d "{\"device_a\":\"$A\",\"device_b\":\"$B\"}" >/dev/null

    echo "-- 轮询链路状态（最多 90s）"
    # 端口受限 NAT 下 stable×stable 存在「先到先得」竞态（对端 punch 先到
    # 会占住本端映射端口的 reply 元组，本端端口保留失败回退随机端口，
    # 整个尝试死锁）。单次尝试成功率约 1/3，靠重试（换 socket 重新竞速）
    # 收敛——这里最多 8 次，并在结果里如实报告用了几次。
    START=$(date +%s)
    STATE=""; ATTEMPTS=0; REASON=""
    while [ $ATTEMPTS -lt 8 ]; do
        ATTEMPTS=$((ATTEMPTS+1))
        curl -s -X POST $API/api/links/connect -d "{\"device_a\":\"$A\",\"device_b\":\"$B\"}" >/dev/null
        T0=$(date +%s)
        while :; do
            STATE=$(curl -s $API/api/links | parse '["links"][0]["state"]' 2>/dev/null)
            [ "$STATE" = "CONNECTED" ] && break
            [ $(( $(date +%s) - T0 )) -gt 20 ] && break
            sleep 2
        done
        [ "$STATE" = "CONNECTED" ] && break
        REASON=$(curl -s $API/api/links | parse '["links"][0].get("last_reason","")' 2>/dev/null)
    done
    ELAPSED=$(( $(date +%s) - START ))
    [ "$STATE" = "CONNECTED" ] && REASON=""
    echo "link state: $STATE${REASON:+ (reason=$REASON)} 尝试 ${ATTEMPTS} 次耗时 ${ELAPSED}s"

    if [ "$STATE" = "CONNECTED" ]; then
        OK=1
        # 双侧 punch connected（建成时的硬要求：link1 侧也必须建成）
        $SSH "grep -q 'punch connected' $D/c1/client.log" || { echo "FAIL: c1 侧无 punch connected"; OK=0; }
        $SSH "grep -q 'punch connected' $D/c2/client.log" || { echo "FAIL: c2 侧无 punch connected"; OK=0; }
        if [ $OK = 1 ]; then echo "双侧 punch connected OK"; fi

        echo "-- 双向 ping 零丢包"
        if [ $OK = 1 ]; then
            P1=$($SSH 'ip netns exec client1 ping -c 8 -i 0.3 10.206.0.2 2>&1 | tail -2')
            echo "$P1" | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: c1→c2 丢包: $P1"; OK=0; }
            P2=$($SSH 'ip netns exec client2 ping -c 8 -i 0.3 10.206.0.1 2>&1 | tail -2')
            echo "$P2" | grep -qE ", 0(\.0)?% packet loss" || { echo "FAIL: c2→c1 丢包: $P2"; OK=0; }
        fi
        [ $OK = 1 ] && VERDICT="PASS" || VERDICT="FAIL 数据面"
    else
        VERDICT="NOT-CONNECTED ($STATE${REASON:+/$REASON})"
    fi

    echo "-- 归档日志"
    $SSH "cat $D/c1/client.log" > "$OUT/c1.client.log" 2>/dev/null
    $SSH "cat $D/c1/gtun-scan.log" > "$OUT/c1.gtun-scan.log" 2>/dev/null
    $SSH "cat $D/c2/client.log" > "$OUT/c2.client.log" 2>/dev/null
    $SSH "cat $D/c2/gtun-scan.log" > "$OUT/c2.gtun-scan.log" 2>/dev/null
    cp "$OUT/server.log" "$OUT/server.archive.log" 2>/dev/null || true
    echo "RESULT $name: $VERDICT (尝试${ATTEMPTS}次, ${ELAPSED}s)"
    echo "$VERDICT 尝试${ATTEMPTS}次" > "$OUT/verdict"
}

cleanup() {
    $SSH "pkill -x gtun-client; pkill -x slirp4netns" 2>/dev/null
    pkill -TERM -x gtun-server 2>/dev/null
}
trap cleanup EXIT

echo "== 构建已就绪，搭 Debian netns 实验室 =="
setup_lab || { echo "FAIL: 实验室搭建失败"; exit 1; }

echo "== Mac 侧路由（100.64/16 → debian）=="
sudo route -n add -net 100.64.0.0/16 192.168.31.105 >/dev/null 2>&1 || sudo route -n change -net 100.64.0.0/16 192.168.31.105 >/dev/null
route -n get 100.64.1.100 >/dev/null 2>&1 || { echo "FAIL: Mac 路由未生效"; exit 1; }
echo "route ok"

FAILED=0
for c in $COMBOS; do
    case $c in
        A) run_combo A stable stable || FAILED=$((FAILED+1)) ;;
        B) run_combo B variable stable || FAILED=$((FAILED+1)) ;;
        C) run_combo C variable variable || FAILED=$((FAILED+1)) ;;
        D) run_combo D variable slirp || FAILED=$((FAILED+1)) ;;
        *) echo "未知组合: $c"; FAILED=$((FAILED+1)) ;;
    esac
done

echo ""
echo "== 汇总 =="
for c in $COMBOS; do
    printf "%s: %s\n" "$c" "$(cat $RUN/$c/verdict 2>/dev/null || echo 未运行)"
done
exit $FAILED
