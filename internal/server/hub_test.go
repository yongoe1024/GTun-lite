package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSubmitContractOnClose 锁定 submit 的返回值契约：err 非 nil ⇔ 命令未生效。
// 并发 submit 与 Close：排干保证入队成功的命令必然执行，因此「返回 nil 的
// 次数」必须与「实际执行次数」相等——任何幽灵成功（nil 但未执行）或
// 幽灵失败（报错但已执行）都会破坏管理操作据 err 决定展示与补偿的正确性。
func TestSubmitContractOnClose(t *testing.T) {
	store, err := OpenStore(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	config := DefaultServerConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := NewHub(store, config, log)
	t.Cleanup(owner.Close)

	const attempts = 200
	var executed atomic.Int32
	var accepted atomic.Int32
	halfway := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < attempts; i++ {
			err := owner.submit(context.Background(), func(*hubState) {
				executed.Add(1)
			})
			if err == nil {
				accepted.Add(1)
			} else if err != ErrServerClosed {
				t.Errorf("submit: %v", err)
			}
			if i == attempts/2-1 {
				close(halfway) // 前半段全部入队成功，保证竞态窗口必然铺开
			}
		}
	}()
	<-halfway
	owner.Close()
	wg.Wait()

	if accepted.Load() != executed.Load() {
		t.Fatalf("contract broken: %d submits accepted, %d executed", accepted.Load(), executed.Load())
	}
	if accepted.Load() < attempts/2 {
		t.Fatalf("window not exercised: only %d accepted before close", accepted.Load())
	}
}
