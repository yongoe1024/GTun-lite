// scanplan 是 stable 侧对 variable 对端的三级端口扫描候选流：纯逻辑、
// 不碰 socket，候选生成的正确性可穷举测试。发包节奏与退出条件由
// worker.rangeScan 负责，两级职责不混。
package client

import (
	"math/rand"
	"time"

	"gtun-lite/internal/common"
)

// scanStage 标识三级扫描的阶段。命中溯源与观察日志都以它标注
// 「这个候选/这次命中出自哪一级假设」。
type scanStage int

const (
	// scanStageNear 一级邻域：以对端最后观测端口为中心 ±10 外扩
	// （0,+1,-1,+2,-2,…），小偏移优先。假设：helper 映射落在画像端口
	// 近旁（时序交错、小偏移顺序分配）。成本 21 个候选（约 63ms）。
	scanStageNear scanStage = iota + 1
	// scanStageSweep 二级均匀扫描：步长 helper_count/4 覆盖全端口空间。
	// 假设：随机起点但映射连续（现网对称 NAT 的常见形态）——无论
	// helper_count 宽的连续段基址在哪，鸽笼原理保证段内至少落进 4 个
	// 候选；步长取段宽的 1/4 而非贴着段宽，是给丢包与端口跳号留冗余。
	scanStageSweep
	// scanStageRandom 三级随机：无重复随机填满剩余预算。无结构假设，
	// 对离散随机分配与一切未知规律兜底。对端的 helper 池就是
	// helper_count 个活靶子，单发命中概率 helper_count/portSpaceSize，
	// 预算内数千发即近必中。
	scanStageRandom
)

// String 返回观察日志用的阶段名。
func (stage scanStage) String() string {
	switch stage {
	case scanStageNear:
		return "near"
	case scanStageSweep:
		return "sweep"
	case scanStageRandom:
		return "random"
	default:
		return "unknown"
	}
}

// 端口空间下上界：候选回绕在这个范围内，避开特权端口与非法端口。
const (
	portSpaceMin  = 1024
	portSpaceMax  = 65535
	portSpaceSize = portSpaceMax - portSpaceMin + 1
)

// nearWindow 一级邻域半径（±nearWindow，共 2*nearWindow+1 个候选）。
const nearWindow = 10

// scanCandidate 是流式产出的一个扫描候选。Ordinal 是阶段内序号（1 起），
// 命中溯源用它回答「第几次计算猜中的」。
type scanCandidate struct {
	Port    int
	Stage   scanStage
	Ordinal int
}

// scanStream 按三级顺序产出候选：邻域 → 均匀扫描 → 无重复随机。
// 上一级耗尽立即进入下一级，无轮次重发——预算的大头必须花在新候选上；
// 对已扫端口的再次覆盖只由流整体耗尽后的重建承担（15s 预算 @3ms 约
// 5000 发，远小于 64512 的端口空间，实际耗不尽）。
type scanStream struct {
	nearPort int // 一级中心（对端最后观测端口）
	stride   int // 二级步长（helper_count/4）
	rng      *rand.Rand

	near      []int // 一级候选（中心外扩序），nearIdx 为游标
	nearIdx   int
	sweep     []int // 二级候选（升序），sweepNext 为游标
	sweepNext int
	sweepOrd  int   // 二级阶段内序号（跳过与一级重叠的步点，只数实际发出的）
	ports     []int // 三级池：整个端口空间，惰性部分洗牌
	drawn     int   // 洗牌游标：左侧已抽出，右侧待抽
	randomOrd int   // 三级阶段内序号
}

// newScanStream 构建三级候选流。helperCount 决定二级步长：256/512/1024
// 档分别对应 64/128/256（ValidateHelperCount 在配置加载阶段已保证档位
// 合法，此处再钳一次下界纯属防御）。
func newScanStream(lastPort common.Port, helperCount int, rng *rand.Rand) *scanStream {
	stream := &scanStream{nearPort: int(lastPort), stride: helperCount / 4, rng: rng}
	if stream.stride < 1 {
		stream.stride = 1
	}
	for offset := 0; offset <= nearWindow; offset++ {
		stream.near = append(stream.near, stream.wrap(offset))
		if offset != 0 {
			stream.near = append(stream.near, stream.wrap(-offset))
		}
	}
	for port := portSpaceMin; port <= portSpaceMax; port += stream.stride {
		stream.sweep = append(stream.sweep, port)
	}
	stream.ports = make([]int, portSpaceSize)
	for index := range stream.ports {
		stream.ports[index] = portSpaceMin + index
	}
	return stream
}

// next 产出下一个候选；false 表示整个端口空间已发完（防御路径，
// 由调用方重建流重扫）。一级/二级候选在三级池中经 O(1) 判属跳过，
// 无需去重集合。
func (s *scanStream) next() (scanCandidate, bool) {
	if s.nearIdx < len(s.near) {
		s.nearIdx++
		return scanCandidate{Port: s.near[s.nearIdx-1], Stage: scanStageNear, Ordinal: s.nearIdx}, true
	}
	if s.sweepNext < len(s.sweep) {
		port := s.sweep[s.sweepNext]
		s.sweepNext++
		// 一级邻域中心恰在步点上时两个阶段会重叠，跳过保证全流不重复。
		if s.nearMember(port) {
			return s.next()
		}
		s.sweepOrd++
		return scanCandidate{Port: port, Stage: scanStageSweep, Ordinal: s.sweepOrd}, true
	}
	for s.drawn < len(s.ports) {
		// 惰性 Fisher–Yates 部分洗牌：每次与 [drawn, len) 均匀换位后
		// 取 drawn 位，等效全局洗牌的前缀消费，只洗实际发出的部分。
		swap := s.drawn + s.rng.Intn(len(s.ports)-s.drawn)
		s.ports[s.drawn], s.ports[swap] = s.ports[swap], s.ports[s.drawn]
		port := s.ports[s.drawn]
		s.drawn++
		if s.nearMember(port) || s.sweepMember(port) {
			continue
		}
		s.randomOrd++
		return scanCandidate{Port: port, Stage: scanStageRandom, Ordinal: s.randomOrd}, true
	}
	return scanCandidate{}, false
}

// stageCount 返回该阶段实际发出的候选总数；三级随机返回 -1（由剩余预算
// 驱动，上限不是固定数）。
func (s *scanStream) stageCount(stage scanStage) int {
	switch stage {
	case scanStageNear:
		return len(s.near)
	case scanStageSweep:
		count := 0
		for _, port := range s.sweep {
			if !s.nearMember(port) {
				count++
			}
		}
		return count
	default:
		return -1
	}
}

// wrap 把一级中心偏移 delta 后回绕进端口空间。
func (s *scanStream) wrap(delta int) int {
	shifted := (s.nearPort - portSpaceMin + delta) % portSpaceSize
	if shifted < 0 {
		shifted += portSpaceSize
	}
	return portSpaceMin + shifted
}

// nearMember 端口是否属于一级邻域（回绕感知的环距 ≤ nearWindow）。
func (s *scanStream) nearMember(port int) bool {
	delta := (port - s.nearPort) % portSpaceSize
	if delta < 0 {
		delta += portSpaceSize
	}
	if delta > portSpaceSize/2 {
		delta = portSpaceSize - delta
	}
	return delta <= nearWindow
}

// sweepMember 端口是否属于二级步点（与下界的差能被步长整除）。
func (s *scanStream) sweepMember(port int) bool {
	return (port-portSpaceMin)%s.stride == 0
}

// newScanRng 为一次扫描建随机源。math/rand 而非 crypto/rand：候选是
// 猜测不是秘密，可复现性优先于不可预测性。
func newScanRng() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}
