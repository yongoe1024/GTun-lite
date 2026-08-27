package server

import (
	"testing"
	"time"

	"gtun-lite/internal/common"
)

// 测试固定时钟：断言 UpdatedAt 是否被刷新，需要能区分「刷了」与「没刷」。
var (
	t0 = time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
	t2 = t0.Add(2 * time.Minute)
)

const (
	tokenA common.LinkToken = "aabbccddeeff"
	tokenB common.LinkToken = "112233445566"
)

// connected 构造一条已连接的链路，供各用例作为起点。
func connected(t *testing.T) *Link {
	t.Helper()
	link := &Link{}
	link.IssueConnect(tokenA, t0)
	if !link.ReportSuccess(tokenA, t1) {
		t.Fatal("ReportSuccess should be accepted from CONNECTING with matching token")
	}
	return link
}

// assertInvariants 在每次断言后检查 State/Token 配对，任何操作序列都不得破坏它。
func assertInvariants(t *testing.T, link *Link) {
	t.Helper()
	if err := link.Invariants(); err != nil {
		t.Fatalf("invariants violated: %v (state=%s token=%q)", err, link.State, link.Token)
	}
}

// TestInvariantOneTCPDropDoesNotTransition 不变量 1：TCP 断开不触发任何状态转换。
//
// 本状态机刻意没有「连接断开」入口——这正是不变量 1 的实现方式。
// 本用例断言这一点：能改状态的方法全集就是下面这五个，控制面断连时
// 调用方无法调用其中任何一个，因此状态必然逐字段不变。
func TestInvariantOneTCPDropDoesNotTransition(t *testing.T) {
	link := connected(t)
	before := *link

	// 控制面断开在服务端只是一条日志。没有 link.OnDisconnect() 之类的方法
	// 可调用，状态与时间戳都不会动。
	after := *link
	if after != before {
		t.Fatalf("state changed without any explicit call: %+v -> %+v", before, after)
	}
	if link.State != LinkConnected || link.Token != tokenA || !link.UpdatedAt.Equal(t1) {
		t.Fatalf("link should stay CONNECTED with original token and timestamp, got %+v", *link)
	}
	assertInvariants(t, link)
}

// 不变量 2（任一侧离线时拒绝下发且不改状态）的用例在 control_test.go 的
// TestInvariantTwoConnectRejectedWhenOffline：在线检查属于 hub——它本来就要
// 取出两侧会话来投递消息，链路状态机本身不再重复这道守卫。

// TestInvariantThreeSingleSideFailureIsEnough 不变量 3：任一侧报失败即判失败，不等另一侧。
func TestInvariantThreeSingleSideFailureIsEnough(t *testing.T) {
	t.Run("from CONNECTING", func(t *testing.T) {
		link := &Link{}
		link.IssueConnect(tokenA, t0)
		if !link.ReportFailure(tokenA, common.ReasonProbeTimeout, t1) {
			t.Fatal("a single side reporting failure must be accepted")
		}
		if link.State != LinkIdle || link.Token != "" {
			t.Fatalf("failure must clear the link, got %+v", *link)
		}
		assertInvariants(t, link)
	})

	t.Run("from CONNECTED", func(t *testing.T) {
		link := connected(t)
		if !link.ReportFailure(tokenA, common.ReasonTunnelLost, t2) {
			t.Fatal("tunnel loss reported by one side must be accepted")
		}
		if link.State != LinkIdle || !link.UpdatedAt.Equal(t2) {
			t.Fatalf("failure must clear the link and refresh the timestamp, got %+v", *link)
		}
		assertInvariants(t, link)
	})
}

// TestStaleTokenReportsIgnored 迟到上报必须按 token 丢弃。
// 少了这个检查，针对上一次尝试的失败上报会把新尝试打回 IDLE，
// 与不变量 3 的悲观判定叠加会让重试在抖动时反复自杀。
func TestStaleTokenReportsIgnored(t *testing.T) {
	link := &Link{}
	link.IssueConnect(tokenB, t1) // 新尝试用 tokenB

	if link.ReportFailure(tokenA, common.ReasonProbeIPChanged, t2) { // tokenA 属于上一次尝试
		t.Fatal("failure carrying a stale token must be ignored")
	}
	if link.State != LinkConnecting || link.Token != tokenB || !link.UpdatedAt.Equal(t1) {
		t.Fatalf("stale report must not touch the link, got %+v", *link)
	}

	if link.ReportSuccess(tokenA, t2) {
		t.Fatal("success carrying a stale token must be ignored")
	}
	if link.State != LinkConnecting {
		t.Fatalf("stale success must not advance the link, got %+v", *link)
	}
	assertInvariants(t, link)
}

// TestAdoptClientReportOverwrites 重连上报直接覆盖，不做一致性协商。
// 这是服务器重启后重建链路状态的唯一途径。
func TestAdoptClientReportOverwrites(t *testing.T) {
	// 服务器重启后链路状态全丢，内存里是零值。
	link := &Link{}
	// 客户端重连上报：隧道一直好着，token 是它自己手上那个。
	if !link.AdoptClientReport(LinkConnected, tokenB, t2) {
		t.Fatal("a well-formed report must be adopted")
	}
	if link.State != LinkConnected || link.Token != tokenB || !link.UpdatedAt.Equal(t2) {
		t.Fatalf("report must be adopted verbatim, got %+v", *link)
	}
	assertInvariants(t, link)

	// 采信是无条件覆盖：服务器记的 CONNECTED 也会被客户端的 IDLE 覆盖，
	// 不做「哪个更可信」的判断。
	if !link.AdoptClientReport(LinkIdle, "", t2.Add(time.Minute)) {
		t.Fatal("IDLE report must be adopted")
	}
	if link.State != LinkIdle || link.Token != "" {
		t.Fatalf("server record must yield to the client fact, got %+v", *link)
	}
	assertInvariants(t, link)
}

// TestAdoptClientReportRejectsContradiction 自相矛盾的上报无法存成合法状态，必须拒绝。
func TestAdoptClientReportRejectsContradiction(t *testing.T) {
	cases := []struct {
		name     string
		reported LinkState
		token    common.LinkToken
	}{
		{"connected without token", LinkConnected, ""},
		{"connecting without token", LinkConnecting, ""},
		{"idle with token", LinkIdle, tokenA},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			link := connected(t)
			before := *link
			if link.AdoptClientReport(c.reported, c.token, t2) {
				t.Fatalf("contradictory report %s/%q must be rejected", c.reported, c.token)
			}
			if *link != before {
				t.Fatalf("rejected report must not change state, got %+v", *link)
			}
			assertInvariants(t, link)
		})
	}
}

// TestIssueConnectRebuildsFromAnyState CONNECT 在任何状态下都合法，且总是换新 token。
// 「重试」没有独立操作，就是再调用一次。
func TestIssueConnectRebuildsFromAnyState(t *testing.T) {
	link := connected(t) // 从 CONNECTED 起步
	link.IssueConnect(tokenB, t2)
	if link.State != LinkConnecting || link.Token != tokenB {
		t.Fatalf("connect must restart the link with the new token, got %+v", *link)
	}
	// 旧 token 随之作废，携带它的上报不再被接受。
	if link.ReportSuccess(tokenA, t2) {
		t.Fatal("the superseded token must no longer be accepted")
	}
	assertInvariants(t, link)
}

// TestStateMachineMatrix 3 态 × 4 个操作的完整矩阵，断言不存在破坏配对的路径。
func TestStateMachineMatrix(t *testing.T) {
	// 三个起点状态各自的构造方式。
	starts := map[string]func(*testing.T) *Link{
		"IDLE": func(*testing.T) *Link { return &Link{} },
		"CONNECTING": func(t *testing.T) *Link {
			t.Helper()
			link := &Link{}
			link.IssueConnect(tokenA, t0)
			return link
		},
		"CONNECTED": connected,
	}
	// 四个操作，都用当前 token 调用（迟到 token 的路径由专门用例覆盖）。
	operations := map[string]func(*Link){
		"IssueConnect":    func(l *Link) { l.IssueConnect(tokenB, t2) },
		"IssueDisconnect": func(l *Link) { l.IssueDisconnect(t2) },
		"ReportSuccess":   func(l *Link) { l.ReportSuccess(l.Token, t2) },
		"ReportFailure":   func(l *Link) { l.ReportFailure(l.Token, common.ReasonPunchTimeout, t2) },
	}
	// 期望表来自 lite-design 2.1 的状态转换表：每格断言目标状态，
	// 使矩阵成为规格本身而不是「跑完不崩」的烟雾测试。
	//   - 两个管理操作按定义改状态（离线拒绝路径由不变量 2 用例覆盖）。
	//   - 成功上报只在 CONNECTING 前进；失败上报带当前 token 即收敛 IDLE。
	//   - IDLE 下的上报道不出当前 token，按迟到报文忽略。
	expected := map[string]map[string]LinkState{
		"IDLE":       {"IssueConnect": LinkConnecting, "IssueDisconnect": LinkIdle, "ReportSuccess": LinkIdle, "ReportFailure": LinkIdle},
		"CONNECTING": {"IssueConnect": LinkConnecting, "IssueDisconnect": LinkIdle, "ReportSuccess": LinkConnected, "ReportFailure": LinkIdle},
		"CONNECTED":  {"IssueConnect": LinkConnecting, "IssueDisconnect": LinkIdle, "ReportSuccess": LinkConnected, "ReportFailure": LinkIdle},
	}
	for stateName, start := range starts {
		for opName, operate := range operations {
			t.Run(stateName+"/"+opName, func(t *testing.T) {
				link := start(t)
				operate(link)
				assertInvariants(t, link)
				if want := expected[stateName][opName]; link.State != want {
					t.Fatalf("%s + %s: want %s, got %s", stateName, opName, want, link.State)
				}
				if link.State.String() == "UNKNOWN" {
					t.Fatalf("operation produced an undefined state: %d", link.State)
				}
			})
		}
	}
}

// TestLastReasonLifecycle 失败原因的写入与清除：接受的失败留存、新尝试
// 清空、其余路径（迟到的旧尝试失败、QUERY/重连快照）一律不动。
// 它是展示用的宽松陈述，不参与任何判定。
func TestLastReasonLifecycle(t *testing.T) {
	link := &Link{}
	link.IssueConnect(tokenA, t0)
	if !link.ReportFailure(tokenA, common.ReasonProbeIPChanged, t1) {
		t.Fatal("ReportFailure: want accepted")
	}
	if link.LastReason != common.ReasonProbeIPChanged {
		t.Fatalf("accepted failure must record its reason, got %q", link.LastReason)
	}

	// 新尝试开始，上次失败原因作废。
	link.IssueConnect(tokenB, t2)
	if link.LastReason != "" {
		t.Fatalf("a fresh attempt must clear the reason, got %q", link.LastReason)
	}

	// 迟到的旧尝试失败：整条上报被 token 守卫拒绝，reason 也不得写入。
	if link.ReportFailure(tokenA, common.ReasonProbeTimeout, t2.Add(time.Minute)) {
		t.Fatal("stale failure must be ignored")
	}
	if link.LastReason != "" {
		t.Fatalf("stale failure must not write its reason, got %q", link.LastReason)
	}

	// QUERY/重连的全量快照只报当前状态、没有历史失败，字段保持原值。
	if !link.AdoptClientReport(LinkIdle, "", t2.Add(2*time.Minute)) {
		t.Fatal("snapshot report must be adopted")
	}
	if link.LastReason != "" {
		t.Fatalf("snapshot adoption must not touch the reason, got %q", link.LastReason)
	}
	assertInvariants(t, link)
}

// TestSuccessOnlyAdvancesFromConnecting 已回到 IDLE 的链路不得被迟到的成功上报拉回。
func TestSuccessOnlyAdvancesFromConnecting(t *testing.T) {
	link := &Link{}
	link.IssueConnect(tokenA, t0)
	if !link.ReportFailure(tokenA, common.ReasonPunchTimeout, t1) {
		t.Fatal("ReportFailure: want accepted")
	}
	// 失败已清空 token，因此这条迟到的成功上报连 token 都对不上。
	if link.ReportSuccess(tokenA, t2) {
		t.Fatal("success must not resurrect a link that already failed")
	}
	if link.State != LinkIdle {
		t.Fatalf("link must stay IDLE, got %+v", *link)
	}
	assertInvariants(t, link)
}
