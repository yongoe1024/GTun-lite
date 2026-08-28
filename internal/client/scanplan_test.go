package client

import (
	"math/rand"
	"testing"

	"gtun-lite/internal/common"
)

func testScanStream(lastPort, helperCount int, seed int64) *scanStream {
	return newScanStream(common.Port(lastPort), helperCount, rand.New(rand.NewSource(seed)))
}

// TestScanStreamNearOrder 一级邻域：中心外扩序 0,+1,-1,…±10，共 21 个。
func TestScanStreamNearOrder(t *testing.T) {
	t.Parallel()
	stream := testScanStream(40000, 256, 1)
	want := []int{40000, 40001, 39999, 40002, 39998, 40003, 39997, 40004, 39996,
		40005, 39995, 40006, 39994, 40007, 39993, 40008, 39992, 40009, 39991, 40010, 39990}
	for index, port := range want {
		candidate, ok := stream.next()
		if !ok || candidate.Stage != scanStageNear || candidate.Port != port || candidate.Ordinal != index+1 {
			t.Fatalf("near candidate %d: want port=%d stage=near ordinal=%d, got %+v ok=%v",
				index+1, port, index+1, candidate, ok)
		}
	}
}

// TestScanStreamNearWrap 一级中心贴近上界时候选在端口空间内回绕。
func TestScanStreamNearWrap(t *testing.T) {
	t.Parallel()
	stream := testScanStream(portSpaceMax-2, 256, 1)
	want := []int{portSpaceMax - 2, portSpaceMax - 1, portSpaceMax - 3, portSpaceMax,
		portSpaceMax - 4, portSpaceMin, portSpaceMax - 5, portSpaceMin + 1}
	for index, port := range want {
		candidate, ok := stream.next()
		if !ok || candidate.Port != port {
			t.Fatalf("wrap candidate %d: want %d, got %+v ok=%v", index, port, candidate, ok)
		}
	}
}

// TestScanStreamSweepStage 二级均匀扫描：各档位步长正确、从下界升序覆盖
// 全空间、全部落在步点上；与一级邻域重叠的步点被跳过（全流不重复），
// 二级耗尽后直达三级随机。40000 恰是 256 档的步点，本例同时覆盖跳过分支。
func TestScanStreamSweepStage(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name        string
		helperCount int
		stride      int
	}{
		{"256", 256, 64},
		{"512", 512, 128},
		{"1024", 1024, 256},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			stream := testScanStream(40000, tier.helperCount, 1)
			for i := 0; i < 2*nearWindow+1; i++ {
				if _, ok := stream.next(); !ok {
					t.Fatal("near stage drained too early")
				}
			}
			var want []int
			for port := portSpaceMin; port <= portSpaceMax; port += tier.stride {
				if !stream.nearMember(port) {
					want = append(want, port)
				}
			}
			for index, port := range want {
				candidate, ok := stream.next()
				if !ok {
					t.Fatalf("sweep drained at %d, want %d", index, len(want))
				}
				if candidate.Stage != scanStageSweep || candidate.Port != port || candidate.Ordinal != index+1 {
					t.Fatalf("sweep candidate %d: want port=%d ordinal=%d, got %+v", index+1, port, index+1, candidate)
				}
			}
			if candidate, ok := stream.next(); !ok || candidate.Stage != scanStageRandom {
				t.Fatalf("after sweep must come random, got %+v ok=%v", candidate, ok)
			}
			if got := stream.stageCount(scanStageSweep); got != len(want) {
				t.Fatalf("stageCount(sweep) = %d, want %d", got, len(want))
			}
		})
	}
}

// TestScanStreamSweepPigeonhole 鸽笼保证：任意 helper_count 宽的连续端口段
// （含跨上界回绕段）至少落进 helper_count/stride 个二级候选。这是二级
// 「确定性命中随机起点连续段」的数学根基，步长取段宽 1/4 即最少 4 个。
func TestScanStreamSweepPigeonhole(t *testing.T) {
	t.Parallel()
	helperCount := 256
	stream := testScanStream(40000, helperCount, 1)
	sweep := map[int]bool{}
	for _, port := range stream.sweep {
		sweep[port] = true
	}
	inWindow := func(anchor, width int) int {
		count := 0
		for offset := 0; offset < width; offset++ {
			if sweep[stream.wrapFrom(anchor, offset)] {
				count++
			}
		}
		return count
	}
	anchors := []int{portSpaceMin, portSpaceMin + 1, 40000, 50000, portSpaceMax - 300, portSpaceMax - helperCount + 1, portSpaceMax}
	for anchor := portSpaceMin; anchor <= portSpaceMax; anchor += 997 {
		anchors = append(anchors, anchor)
	}
	for _, anchor := range anchors {
		if got := inWindow(anchor, helperCount); got < helperCount/stream.stride {
			t.Fatalf("window at %d contains only %d sweep candidates, want >= %d", anchor, got, helperCount/stream.stride)
		}
	}
}

// wrapFrom 把锚点偏移 offset 后回绕进端口空间（测试用，与一级的 wrap 同一
// 回绕算术）。
func (s *scanStream) wrapFrom(anchor, offset int) int {
	shifted := (anchor - portSpaceMin + offset) % portSpaceSize
	if shifted < 0 {
		shifted += portSpaceSize
	}
	return portSpaceMin + shifted
}

// TestScanStreamRandomStage 三级随机：固定种子下无重复、全部界内、
// 不与一二级候选重叠（成员判属跳过）。
func TestScanStreamRandomStage(t *testing.T) {
	t.Parallel()
	helperCount := 256
	stream := testScanStream(40000, helperCount, 42)
	for i := 0; i < stream.stageCount(scanStageNear)+stream.stageCount(scanStageSweep); i++ {
		if _, ok := stream.next(); !ok {
			t.Fatal("stream drained before random stage")
		}
	}
	seen := map[int]bool{}
	for i := 0; i < 5000; i++ {
		candidate, ok := stream.next()
		if !ok {
			t.Fatalf("random stage drained after %d candidates", i)
		}
		if candidate.Stage != scanStageRandom || candidate.Ordinal != i+1 {
			t.Fatalf("random candidate %d: got %+v", i+1, candidate)
		}
		if candidate.Port < portSpaceMin || candidate.Port > portSpaceMax {
			t.Fatalf("random candidate outside port space: %d", candidate.Port)
		}
		if seen[candidate.Port] {
			t.Fatalf("random candidate repeated: %d", candidate.Port)
		}
		if stream.nearMember(candidate.Port) || stream.sweepMember(candidate.Port) {
			t.Fatalf("random candidate overlaps earlier stage: %d", candidate.Port)
		}
		seen[candidate.Port] = true
	}
}

// TestScanStreamExhaustsAllPortsOnce 全流耗尽：候选总数恰为端口空间大小，
// 每个端口恰一次（三级跳过逻辑不重不漏的穷举证明）。
func TestScanStreamExhaustsAllPortsOnce(t *testing.T) {
	t.Parallel()
	stream := testScanStream(40000, 256, 7)
	seen := make(map[int]bool, portSpaceSize)
	count := 0
	for {
		candidate, ok := stream.next()
		if !ok {
			break
		}
		if seen[candidate.Port] {
			t.Fatalf("port %d produced twice", candidate.Port)
		}
		seen[candidate.Port] = true
		count++
	}
	if count != portSpaceSize || len(seen) != portSpaceSize {
		t.Fatalf("expected exactly %d unique candidates, got count=%d unique=%d", portSpaceSize, count, len(seen))
	}
}
