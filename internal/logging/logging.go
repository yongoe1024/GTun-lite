// Package logging 构造两端共用的结构化日志。可选把普通日志（info 及以下）
// 与错误日志（warn 及以上）分别落盘为两个文件——服务化部署（schtasks/
// systemd）里 stderr 依赖启动器重定向，靠不住（真机测试曾因 run.cmd 行尾
// 问题丢掉全部客户端日志，排障失明）。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Options 描述日志输出。File 与 ErrorFile 均可选：未设置的流回落到
// stderr，保持交互运行时的既有行为（只配其一则另一类日志仍走 stderr）。
type Options struct {
	// Level 是 debug/info/warn/error，空串按 info。
	Level string
	// File 是普通日志（info 及以下）的追加写路径。
	File string
	// ErrorFile 是错误日志（warn 及以上）的追加写路径。
	ErrorFile string
	// Console 为 true 时所有记录在写文件之外同时输出到 stderr——
	// 双击/交互场景「窗口实时看 + 文件可回查」两全；未配置文件的
	// 流本就回落 stderr，不受影响。
	Console bool
}

// New 构造 logger，返回的关闭函数释放打开的文件（进程退出前调用）。
// 文件打开失败立即报错——fail-fast：日志路径配错必须拒启动，
// 而不是带病运行到需要排障时才发现没有日志。
func New(options Options) (*slog.Logger, func(), error) {
	level := parseLevel(options.Level)
	infoWriter := io.Writer(os.Stderr)
	errWriter := io.Writer(os.Stderr)
	var closers []func()
	closeAll := func() {
		for _, closeOne := range closers {
			closeOne()
		}
	}
	if options.File != "" {
		file, err := os.OpenFile(options.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		infoWriter = file
		if options.Console {
			infoWriter = io.MultiWriter(file, os.Stderr)
		}
		closers = append(closers, func() { _ = file.Close() })
	}
	if options.ErrorFile != "" {
		file, err := os.OpenFile(options.ErrorFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("open error log file: %w", err)
		}
		errWriter = file
		if options.Console {
			errWriter = io.MultiWriter(file, os.Stderr)
		}
		closers = append(closers, func() { _ = file.Close() })
	}
	handlerOptions := &slog.HandlerOptions{Level: level}
	return slog.New(splitHandler{
		info: slog.NewTextHandler(infoWriter, handlerOptions),
		errs: slog.NewTextHandler(errWriter, handlerOptions),
	}), closeAll, nil
}

// splitHandler 按 level 把记录分给普通流与错误流，两个底层 handler
// 共享同一 Level 过滤。都落到同一 writer（未配置文件的流回落 stderr）时
// 行为与单个 handler 一致。
type splitHandler struct {
	info slog.Handler
	errs slog.Handler
}

func (handler splitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.info.Enabled(ctx, level) || handler.errs.Enabled(ctx, level)
}

func (handler splitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelWarn {
		return handler.errs.Handle(ctx, record)
	}
	return handler.info.Handle(ctx, record)
}

func (handler splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return splitHandler{info: handler.info.WithAttrs(attrs), errs: handler.errs.WithAttrs(attrs)}
}

func (handler splitHandler) WithGroup(name string) slog.Handler {
	return splitHandler{info: handler.info.WithGroup(name), errs: handler.errs.WithGroup(name)}
}

// parseLevel 把配置文本解析为 slog 级别，未知取值按 info（与配置校验的
// 白名单一致，这里只做兜底）。
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
