#!/bin/bash
# GTun-Lite 客户端双击启动器（macOS）。
# Finder 双击 .command 会用 Terminal 执行本脚本：切到程序包目录后以
# sudo 运行客户端（TUN 设备创建需要提权），日志实时显示在本窗口。
# 停止：Ctrl-C，或直接关闭本窗口。
cd "$(dirname "$0")"
sudo ./gtun-client -config client.yaml
