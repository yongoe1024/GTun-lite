#!/bin/bash
# GTun-Lite 客户端双击启动器（macOS）。
# Finder 双击 .command 会用 Terminal 执行本脚本：切到程序包目录后以
# sudo 运行客户端（TUN 设备创建需要提权），日志实时显示在本窗口。
# 交互模式剥离随包 yaml 的日志落盘键（logging.file/error_file）生成
# 临时配置 .client-console.yaml，退出时自动清理——双击场景看窗口，
# 不看文件；服务化部署请直接运行 gtun-client 并保留 yaml 落盘配置。
# 停止：Ctrl-C，或直接关闭本窗口。
cd "$(dirname "$0")"
echo "== GTun 客户端交互模式：日志实时显示在本窗口（不写日志文件）=="
sed -e '/^  file:/d' -e '/^  error_file:/d' client.yaml > .client-console.yaml
trap 'rm -f .client-console.yaml' EXIT
sudo ./gtun-client -config .client-console.yaml
