#!/bin/bash
# GTun-Lite 客户端双击启动器（macOS）。
# Finder 双击 .command 会用 Terminal 执行本脚本：切到程序包目录后以
# sudo 运行客户端（TUN 设备创建需要提权），日志实时显示在本窗口。
# 日志为窗口 + 文件双写（配置了 logging.file 即双写，见文档）：
# 本窗口实时显示，gtun-client.log / .err 照常生成。
# 服务化部署（nohup 丢弃 stderr）则等效纯文件。
# 停止：Ctrl-C，或直接关闭本窗口。
cd "$(dirname "$0")"
sudo ./gtun-client -config client.yaml
