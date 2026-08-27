// Package schema 内嵌服务端 SQLite 建表语句。
//
// schema 是部署契约的一部分：store 启动时校验库内表集合与 server.sql
// 完全一致（见 internal/server 的 validateSchema），空库则用同一份文件
// 建表。内嵌保证「校验用的」与「建表用的」永远是同一份 SQL，不会出现
// 文件系统上的文件与二进制内嵌副本漂移的问题。
package schema

import _ "embed"

// ServerSQL 是 schema/server.sql 的完整内容。
//
//go:embed server.sql
var ServerSQL string
