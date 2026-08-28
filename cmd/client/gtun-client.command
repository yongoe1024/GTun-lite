#!/bin/bash
# GTun-Lite 客户端双击启动器（macOS）。
# Finder 双击 .command 会用 Terminal 执行本脚本：切到程序包目录后以
# sudo 运行客户端（TUN 设备创建需要提权），日志实时显示在本窗口。
# 交互模式剥离随包 yaml 的日志落盘键（logging.file/error_file）——
# 双击场景要的是窗口实时日志而不是文件；服务化部署请直接运行
# gtun-client 并保留 yaml 的落盘配置。
# 停止：Ctrl-C，或直接关闭本窗口。
cd "$(dirname "$0")"
sed -e '/^  file:/d' -e '/^  error_file:/d' client.yaml > .client-console.yaml
sudo ./gtun-client -config .client-console.yaml
rm -f .client-console.yaml
