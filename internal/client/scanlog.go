// ⚠️ 开发期临时设施：预测端口观察日志，发布前整体删除本文件
// 并移除 worker.go 中全部 scanNote/scanHit/scanInboundOnce 调用点。
//
// 用途：观察对端 NAT 的端口分配规律。记录 stable 侧三级扫描的每个候选、
// 入站信标的唯一源端口、命中预测的阶段与序号。写死路径、不走配置、
// 不镜像 stderr；打开失败静默降级为丢弃——观察日志绝不影响打洞运行，
// 这是它能被放心整体删除的前提。
//
// 所有入口都带 token（尝试标识）：本文件是进程级单文件，自动重试每次
// 尝试换新 token、多链路并发时行会交错，没有 token 无法归属。
package client

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"gtun-lite/internal/common"
)

// scanLogPath 写死于工作目录，与 gtun-*.log 命名族一致。
const scanLogPath = "gtun-scan.log"

var (
	scanLogOnce sync.Once
	scanLogInst *slog.Logger
)

// scanLogger 返回包级单例：纯追加文本日志，Info 级。
func scanLogger() *slog.Logger {
	scanLogOnce.Do(func() {
		file, err := os.OpenFile(scanLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			scanLogInst = slog.New(slog.NewTextHandler(io.Discard, nil))
			return
		}
		// 开发期工具，进程生命周期内不关闭。
		scanLogInst = slog.New(slog.NewTextHandler(file, nil))
		scanLogInst.Info("scan observation log opened", "path", scanLogPath)
	})
	return scanLogInst
}

// scanNote 记一条扫描观察（每发候选、阶段切换、起止等）。
func scanNote(token common.LinkToken, message string, args ...any) {
	scanLogger().Info(message, append([]any{"token", string(token)}, args...)...)
}

// scanHit 记预测命中：哪个阶段、阶段内第几次计算、命中的端口与耗时。
func scanHit(token common.LinkToken, stage scanStage, ordinal int, port int, elapsed time.Duration) {
	scanLogger().Info("prediction hit",
		append([]any{"token", string(token)},
			"stage", stage.String(), "ordinal", ordinal, "port", port, "elapsed", elapsed.String())...)
}

// scanInbound 记一个首次入站的源端口（对端 helper 映射的直接观测）。
// guessed=false 表示该端口不在此前候选里——本身就是分配规律的样本。
func scanInbound(token common.LinkToken, port int, guessed bool) {
	scanLogger().Info("inbound punch", "token", string(token), "port", port, "guessed", guessed)
}

var (
	inboundMu   sync.Mutex
	inboundSeen = map[inboundKey]struct{}{}
)

// inboundKey 按尝试归拢：每次尝试的对端映射是新的，逐尝试去重比进程级
// 去重更能反映「本次尝试看到了哪些端口」。
type inboundKey struct {
	token common.LinkToken
	port  int
}

// inboundSeenMax 入站观察的去重上限：正常不超过对端 helper 档位，
// 超界只可能是异常噪声，防御性停记。
const inboundSeenMax = 8192

// scanInboundOnce 同一尝试内每个源端口只记首次入站。
func scanInboundOnce(token common.LinkToken, port int, guessed bool) {
	inboundMu.Lock()
	defer inboundMu.Unlock()
	key := inboundKey{token: token, port: port}
	if len(inboundSeen) >= inboundSeenMax {
		return
	}
	if _, ok := inboundSeen[key]; ok {
		return
	}
	inboundSeen[key] = struct{}{}
	scanInbound(token, port, guessed)
}
