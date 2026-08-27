package server

import (
	_ "embed"
	"io"
	"net/http"
)

// indexHTML 是管理页面的全部内容：单文件、无外部依赖。go:embed 编译进
// 二进制的理由与 schema/server.sql 相同——部署不会漏带文件，也不存在
// 文件系统副本与二进制内嵌副本漂移的问题。
//
// 页面是纯静态的：数据全部来自 /api/*，因此不引入模板渲染，也没有
// 缓存与协商的必要，每次原样输出。
//
//go:embed index.html
var indexHTML string

// index 输出管理页面（GET /）。
func (api *AdminAPI) index(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(writer, indexHTML)
}
