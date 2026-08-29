// Package notice 承担窗口实时提示：简短中文的关键状态变更，供交互
// 运行时（Terminal 窗口）直读。与结构化日志分流——详细英文日志照旧
// 落盘 logging 配置的文件，这里只写 stderr，无级别、不受日志级别过滤。
package notice

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Notice 向窗口输出关键状态提示。零值不可用，须经 New 构造。
type Notice struct {
	mu     sync.Mutex
	writer io.Writer
}

// New 构造提示器；writer 传 nil 时回落 stderr。
func New(writer io.Writer) *Notice {
	if writer == nil {
		writer = os.Stderr
	}
	return &Notice{writer: writer}
}

// Printf 输出一行提示，行首带本地时刻，格式如「15:04:05 已连接端口服务器 …」。
// nil 接收者是合法的空操作——测试与无窗口场景不构造提示器。
func (notice *Notice) Printf(format string, args ...any) {
	if notice == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	notice.mu.Lock()
	defer notice.mu.Unlock()
	_, _ = fmt.Fprintf(notice.writer, "%s %s\n", time.Now().Format("15:04:05"), line)
}

// NAT 把画像里的 NAT 取值翻成窗口用的简单中文，未知取值原样显示。
func NAT(nat string) string {
	switch nat {
	case "stable":
		return "稳定型"
	case "variable":
		return "变化型"
	default:
		return nat
	}
}
