package common

import (
	"strings"
	"testing"
)

// TestReasonTableIsClosedAndUnique 原因表必须封闭且无重复：AllReasons 返回的每个值
// 都能通过 ValidateReason，且彼此不同。新增常量时漏加进 reasons 数组，
// 会让它在线上被判为未知原因——这个用例把两处的不同步暴露出来。
func TestReasonTableIsClosedAndUnique(t *testing.T) {
	t.Parallel()
	all := AllReasons()
	if len(all) != 12 {
		t.Fatalf("reason table has %d entries, want 12", len(all))
	}
	seen := make(map[Reason]struct{}, len(all))
	for _, reason := range all {
		if err := ValidateReason(reason); err != nil {
			t.Errorf("ValidateReason(%q) rejected a table entry: %v", reason, err)
		}
		if _, duplicate := seen[reason]; duplicate {
			t.Errorf("duplicate reason %q", reason)
		}
		seen[reason] = struct{}{}
	}
	// 全部常量都必须在表里。逐个列出而非反射，是为了让漏加的常量在编译期
	// 就出现在这份清单里，而不是靠运行时枚举碰巧发现。
	for _, reason := range []Reason{
		ReasonProbeTimeout, ReasonProbeIPChanged, ReasonPunchTimeout,
		ReasonNATUnsupported, ReasonRouteConflict, ReasonTUNCreateFailed,
		ReasonConfigInvalid, ReasonTunnelLost, ReasonPeerOffline,
		ReasonHandshakeFailed, ReasonQueueFull, ReasonInternalError,
	} {
		if _, ok := seen[reason]; !ok {
			t.Errorf("constant %q is missing from the reasons table", reason)
		}
	}
}

// TestAllReasonsReturnsCopy AllReasons 必须返回副本，调用方改动不得污染包内表。
func TestAllReasonsReturnsCopy(t *testing.T) {
	t.Parallel()
	first := AllReasons()
	first[0] = "MUTATED"
	if second := AllReasons(); second[0] == "MUTATED" {
		t.Fatal("AllReasons exposed the package-level table")
	}
	if err := ValidateReason(Reason("MUTATED")); err == nil {
		t.Fatal("mutating the returned slice corrupted the validation set")
	}
}

// TestValidateReasonRejectsUnknown 表外的任意文本必须拒绝。原因值会进日志、
// 进管理页面、进运维判断，自由文本会让它退化成不可聚合的字符串。
func TestValidateReasonRejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value Reason
	}{
		{"empty", ""},
		{"free text", "something went wrong"},
		{"lowercase variant", "punch_timeout"},
		{"too long", Reason(strings.Repeat("A", maxReasonBytes+1))},
		// 控制面断开刻意不是失败原因：隧道不经过服务器，断连只值一条日志。
		{"control disconnect is not a reason", "CONTROL_DISCONNECTED"},
		// 以下几个属于本设计不存在的机制，不得被接受。
		{"claim recovery", "RECOVERY_CLAIM"},
		{"attempt watermark", "ATTEMPT_GONE"},
		{"queue budget", "QUEUE_BUDGET_EXCEEDED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateReason(c.value); err == nil {
				t.Fatalf("ValidateReason(%q) accepted a value outside the table", c.value)
			}
		})
	}
}
