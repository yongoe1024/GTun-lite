#!/bin/bash
# GTun-Lite 客户端双击启动器（macOS）。
# Finder 双击 .command 会用 Terminal 执行本脚本：切到程序包目录后以
# sudo 运行客户端（TUN 设备创建需要提权），日志实时显示在本窗口。
# 交互模式在 yaml 的 logging 段注入 console: true（临时副本）：
# 窗口实时显示 + 文件照常落盘，两全。退出时临时副本自动清理；
# 服务化部署直接运行 gtun-client（yaml 原样，无窗口只落文件）。
# 停止：Ctrl-C，或直接关闭本窗口。
cd "$(dirname "$0")"
echo "== GTun 客户端交互模式：日志实时显示在本窗口，同时写入日志文件 =="
if grep -q "console:" client.yaml; then
  cp client.yaml .client-console.yaml
else
  sed -e '/^logging:/a\
  console: true' client.yaml > .client-console.yaml
fi
trap 'rm -f .client-console.yaml' EXIT
sudo ./gtun-client -config .client-console.yaml
