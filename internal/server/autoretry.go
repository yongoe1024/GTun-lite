package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 链路失败自动重试：一个全局开关 + 一个每 interval 遍历一次内存链路表的
// 循环，凡断开（IDLE）的链路重发 CONNECT。没有按设备对维护的注册表——
// 链路增删、状态流转、服务器重启丢内存态，循环下一拍都自然适应。
//
// 作用范围是内存链路记录（建过链的配对，失败经不变量 3 收敛回 IDLE 后
// 留在表里）：从未建过链的新配对没有「失败」可言，首次建链仍需手动下发。
// 开关状态同为内存态，重启一并归零，管理页经 GET /api/links 如实回显。
//
// 已知边界：手动 DISCONNECT 后记录也回到 IDLE，开关开着时 5s 内会被
// 扫回去——断链前先关开关（管理页开关提示语已注明），不为此改拆链逻辑。

const AutoRetryInterval = 5 * time.Second

// AutoConnectIdle 单条 owner 命令内遍历内存链路表，对全部断开（IDLE）
// 的链路重发 CONNECT。查状态与下发在同一命令内完成，与管理操作、状态
// 上报不存在交错窗口；CONNECTING（尝试进行中）与 CONNECTED（已满足）
// 自然跳过。逐对的失败（单侧离线、配对被删等）记 Debug 后继续其余链路，
// 不中断本轮扫描。返回本轮实际下发的链路数。
func (owner *Hub) AutoConnectIdle(ctx context.Context) (int, error) {
	issued := 0
	err := owner.submit(ctx, func(state *hubState) {
		for pair, record := range state.links {
			if record.State != LinkIdle {
				continue
			}
			if err := owner.issueConnect(state, pair); err != nil {
				owner.log.Debug("auto retry connect skipped",
					"devices", [2]string{string(pair[0]), string(pair[1])}, "error", err)
				continue
			}
			issued++
		}
	})
	if err != nil {
		return 0, err
	}
	return issued, nil
}

// AutoRetry 是自动重试循环的运行体与开关。单 goroutine：Start 启动、
// Stop 停止（等待退出）。interval 只允许在循环未运行时修改——生产路径
// 从不改；测试先 Stop 再改再 Start，保证新值从第一拍就生效且无数据竞争。
type AutoRetry struct {
	hub      *Hub
	log      *slog.Logger
	interval time.Duration // 生产 5s；测试可在 Stop 期间调短
	enabled  atomic.Bool

	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewAutoRetry 创建循环体并启动它；开关默认关，循环空转的代价是每拍
// 一次原子读。进程退出即止（hub 关闭后扫描仅记 Debug 日志）。
func NewAutoRetry(hub *Hub, log *slog.Logger) *AutoRetry {
	a := &AutoRetry{hub: hub, log: log, interval: AutoRetryInterval}
	a.Start()
	return a
}

// Start 启动扫描循环，已在运行时无效果。
func (a *AutoRetry) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop != nil {
		return
	}
	stop := make(chan struct{})
	a.stop = stop
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.run(stop)
	}()
}

// Stop 停止扫描循环并等待退出，未运行时无效果。之后可改 interval 再 Start。
func (a *AutoRetry) Stop() {
	a.mu.Lock()
	if a.stop == nil {
		a.mu.Unlock()
		return
	}
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	close(stop)
	a.wg.Wait()
}

// Set 翻开关（幂等）。开关只影响下一拍是否扫描。
func (a *AutoRetry) Set(enable bool) { a.enabled.Store(enable) }

// Enabled 返回开关当前状态。
func (a *AutoRetry) Enabled() bool { return a.enabled.Load() }

// run 按拍扫描直到 stop 关闭。关着时每拍空转一次；interval 在每拍开始
// 时读取，Stop/Start 换新值即刻生效。
func (a *AutoRetry) run(stop <-chan struct{}) {
	for {
		timer := time.NewTimer(a.interval)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if a.enabled.Load() {
			ctx, cancel := context.WithTimeout(context.Background(), a.interval)
			_, err := a.hub.AutoConnectIdle(ctx)
			cancel()
			if err != nil {
				// 成功下发的逐对日志由 issueConnect 自己输出；这里只关心
				// 整拍失败（当前仅 hub 已关闭一种）。
				a.log.Debug("auto retry sweep skipped", "error", err)
			}
		}
	}
}
