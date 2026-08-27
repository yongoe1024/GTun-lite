.PHONY: build build-all build-linux build-darwin build-darwin-arm64 build-windows verify-manifest manifest test vet check schema clean

GO ?= go
LDFLAGS := -trimpath

# 三平台构建（对齐旧项目发布口径）。每平台目录是一个可发布程序包：
# 二进制 + 默认配置（cmd/ 下 yaml 是配置默认值事实源），Windows 另附
# wintun.dll（运行时从 exe 同目录加载）。数据库 schema 已编译进服务端。
build: build-linux

build-linux: ## 构建 Linux amd64 程序包到 bin/linux/
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/linux/gtun-server ./cmd/server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/linux/gtun-client ./cmd/client
	cp cmd/server/server.yaml bin/linux/server.yaml
	cp cmd/client/client.yaml bin/linux/client.yaml
	@echo "Linux package: bin/linux/"

build-darwin: ## 构建 macOS amd64 程序包到 bin/darwin/
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin/gtun-server ./cmd/server
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin/gtun-client ./cmd/client
	cp cmd/server/server.yaml bin/darwin/server.yaml
	cp cmd/client/client.yaml bin/darwin/client.yaml
	cp cmd/client/gtun-client.command bin/darwin/gtun-client.command
	@echo "macOS amd64 package: bin/darwin/"

build-darwin-arm64: ## 构建 macOS arm64 程序包到 bin/darwin-arm64/（Apple Silicon 原生）
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin-arm64/gtun-server ./cmd/server
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/darwin-arm64/gtun-client ./cmd/client
	cp cmd/server/server.yaml bin/darwin-arm64/server.yaml
	cp cmd/client/client.yaml bin/darwin-arm64/client.yaml
	cp cmd/client/gtun-client.command bin/darwin-arm64/gtun-client.command
	@echo "macOS arm64 package: bin/darwin-arm64/"

# 校验 syso 内嵌的 manifest 与源文件一致：改了 manifest 忘记 make manifest
# 时，syso（以及产出的 exe）静默携带旧声明——在构建期拦下而不是等运行期发现。
verify-manifest:
	@python3 -c "import sys; m=open('cmd/client/gtun-client.manifest','rb').read(); s=open('cmd/client/rsrc_windows_amd64.syso','rb').read(); sys.exit(0 if m in s else 1)" 		|| { echo 'FAIL: gtun-client.manifest 与 rsrc_windows_amd64.syso 不一致（改了源文件未重新生成）'; echo '      修复：make manifest'; exit 1; }

build-windows: verify-manifest ## 构建 Windows amd64 程序包到 bin/windows/（wintun.dll 必须与 exe 同目录）
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/windows/gtun-server.exe ./cmd/server
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/windows/gtun-client.exe ./cmd/client
	cp wintun/bin/amd64/wintun.dll bin/windows/wintun.dll
	cp cmd/server/server.yaml bin/windows/server.yaml
	cp cmd/client/client.yaml bin/windows/client.yaml
	@echo "Windows package: bin/windows/"

build-all: build-linux build-darwin build-darwin-arm64 build-windows ## 构建全部平台程序包

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

clean: ## 清理构建产物
	rm -rf bin/
