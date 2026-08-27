// Package server 实现 GTun-Lite 服务端：TCP 控制面、链路状态机与管理 API。
//
// 权责划分是本包的核心约束：服务器只管下发意图（CONNECT / DISCONNECT / QUERY），
// 客户端是隧道事实的唯一来源。服务器不推断隧道状态——它只知道自己下发过什么，
// 以及客户端上报过什么；两者不一致时以客户端为准。三条不变量见 link.go。
package server
