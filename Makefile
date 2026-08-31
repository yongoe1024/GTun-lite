.PHONY: build build-all build-linux build-darwin build-darwin-arm64 build-windows build-android-lib verify-manifest manifest test vet check schema clean clean-bin

GO ?= go
LDFLAGS := -trimpath

# 跨平台构建。每平台目录下按 client/ 与 server/ 分为两个自包含程序包：
# 各自的二进制 + 默认配置（cmd/ 下 yaml 是配置默认值事实源），客户端另附
# 平台配件（macOS 双击启动器 / Windows wintun.dll，运行时从 exe 同目录
# 加载）。数据库 schema 已编译进服务端。
build: build-linux

build-linux: ## 构建 Linux amd64 程序包到 bin/linux/{client,server}/（先清空该平台目录）
	rm -rf bin/linux
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/linux/client/gtun-client ./cmd/client
	cp cmd/client/client.yaml bin/linux/client/client.yaml
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/linux/server/gtun-server ./cmd/server
	cp cmd/server/server.yaml bin/linux/server/server.yaml
	@echo "Linux packages: bin/linux/client/ bin/linux/server/"

build-darwin: ## 构建 macOS amd64 程序包到 bin/darwin/{client,server}/（先清空该平台目录）
	rm -rf bin/darwin
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin/client/gtun-client ./cmd/client
	cp cmd/client/client.yaml bin/darwin/client/client.yaml
	cp cmd/client/双击启动.command bin/darwin/client/双击启动.command
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin/server/gtun-server ./cmd/server
	cp cmd/server/server.yaml bin/darwin/server/server.yaml
	@echo "macOS amd64 packages: bin/darwin/client/ bin/darwin/server/"

build-darwin-arm64: ## 构建 macOS arm64 程序包到 bin/darwin-arm64/{client,server}/（Apple Silicon 原生，先清空该平台目录）
	rm -rf bin/darwin-arm64
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin-arm64/client/gtun-client ./cmd/client
	cp cmd/client/client.yaml bin/darwin-arm64/client/client.yaml
	cp cmd/client/双击启动.command bin/darwin-arm64/client/双击启动.command
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin-arm64/server/gtun-server ./cmd/server
	cp cmd/server/server.yaml bin/darwin-arm64/server/server.yaml
	@echo "macOS arm64 packages: bin/darwin-arm64/client/ bin/darwin-arm64/server/"

# 校验 syso 内嵌的 manifest 与源文件一致：改了 manifest 忘记 make manifest
# 时，syso（以及产出的 exe）静默携带旧声明——在构建期拦下而不是等运行期发现。
verify-manifest:
	@python3 -c "import sys; m=open('cmd/client/gtun-client.manifest','rb').read(); s=open('cmd/client/rsrc_windows_amd64.syso','rb').read(); sys.exit(0 if m in s else 1)" 		|| { echo 'FAIL: gtun-client.manifest 与 rsrc_windows_amd64.syso 不一致（改了源文件未重新生成）'; echo '      修复：make manifest'; exit 1; }

build-windows: verify-manifest ## 构建 Windows amd64 程序包到 bin/windows/{client,server}/（先清空该平台目录；wintun.dll 必须与 exe 同目录）
	rm -rf bin/windows
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/windows/client/gtun-client.exe ./cmd/client
	cp cmd/client/client.yaml bin/windows/client/client.yaml
	cp wintun/bin/amd64/wintun.dll bin/windows/client/wintun.dll
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/windows/server/gtun-server.exe ./cmd/server
	cp cmd/server/server.yaml bin/windows/server/server.yaml
	@echo "Windows packages: bin/windows/client/ bin/windows/server/"

# 安卓客户端内核 aar（gomobile bind）。两个环境硬约束：
# ① JAVA_HOME 必须指 JDK 17——javac 26 生成的新 class 属性会让壳工程
#   AGP 7.4 的老版 D8 在 dex 阶段 NPE，与用哪个 JDK 跑构建无关；
# ② gomobile 经 go.mod 的 tool 指令调用（go tool gomobile）；其伴生工具
#   gobind 靠 PATH 定位，故把 ~/go/bin 前置；NDK 由 ANDROID_HOME 定位。
# 只产出到 bin/android/，投放壳工程由使用侧自理。不并入 build-all：
# 依赖本机 JDK17/NDK 环境，没有该环境的机器不应被全量构建绊倒。
ANDROID_JAVA_HOME ?= $(HOME)/Library/Java/JavaVirtualMachines/ms-17.0.20.1/Contents/Home
ANDROID_HOME ?= $(HOME)/Library/Android/sdk

build-android-lib: ## gomobile bind 出安卓内核 aar 到 bin/android/
	rm -rf bin/android
	mkdir -p bin/android
	JAVA_HOME=$(ANDROID_JAVA_HOME) ANDROID_HOME=$(ANDROID_HOME) PATH="$(HOME)/go/bin:$$PATH" $(GO) tool gomobile bind -target=android -androidapi 26 -o bin/android/gtunlite.aar ./cmd/gtunlib
	@echo "Android aar: bin/android/"

build-all: clean-bin build-linux build-darwin build-darwin-arm64 build-windows ## 构建全部平台程序包（先整体清空 bin/；安卓 aar 另见 build-android-lib）

clean-bin: ## 清空 bin/ 构建产物根目录
	rm -rf bin/

# 由 cmd/client/gtun-client.manifest 重新生成 Windows 资源目标文件（UAC 提权声明）。
# 仅在修改 manifest 后需要；syso 已入库，常规构建不依赖此目标与外部工具。
manifest: ## 重新生成 rsrc_windows_amd64.syso（需网络拉取 rsrc 工具，不改 go.mod）
	GOPROXY=$(or $(GOPROXY),https://goproxy.cn,direct) \
		go run github.com/akavel/rsrc@v0.10.2 -arch amd64 \
		-manifest cmd/client/gtun-client.manifest \
		-o cmd/client/rsrc_windows_amd64.syso

# 自动化门槛（发布前必须全绿）
test: ## 运行全部测试（含 race detector）
	$(GO) test -race -count=1 ./...

vet: ## 运行 go vet
	$(GO) vet ./...

check: vet test ## vet + test 一键检查

# SQLite 预建库与验证（可选：服务器对空库也会按内嵌 schema 自动建表）
schema: ## 初始化并验证 SQLite schema
	sqlite3 gtun.db < schema/server.sql
	sqlite3 gtun.db "PRAGMA foreign_key_check; PRAGMA integrity_check;"
	sqlite3 gtun.db "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;"

clean: clean-bin ## 清理构建产物（同 clean-bin）
