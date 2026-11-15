# freeclash 构建脚本
#
# 常用：
#   make dev       当前平台快速编译到 ./dist
#   make all       交叉编译各平台发行包
#   make test      跑测试
#   make service   编译并把当前平台的二进制安装成本机系统服务
#
# UPX 压缩默认开启，找不到 upx 时自动跳过；需要关闭时：
#   make all UPX=          或   make dev NO_UPX=1
#
# 注意：UPX 压过的服务端二进制偶尔会被杀软误报（尤其是装成 Windows 服务时），
# 遇到拦截就用 NO_UPX=1 重新构建。

BIN_NAME := freeclash
DIST     := dist

# 本机平台信息：GOOS 可被环境/命令行覆盖，便于 make dev 交叉验证
GOOS ?= $(shell go env GOOS)
EXE  := $(shell go env GOEXE)

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

# ---- UPX 压缩 ----
UPX       ?= upx
UPX_FLAGS ?= -9 -q

ifeq ($(NO_UPX),1)
UPX :=
endif

.PHONY: all
all: linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64
	@echo "==> 打包完成，见 $(DIST)/"
	@ls -lh $(DIST)/ 2>/dev/null || true

# $(1)=GOOS $(2)=GOARCH $(3)=扩展名
define build-target
.PHONY: $(1)-$(2)
$(1)-$(2):
	@echo "==> 构建 $(1)/$(2)"
	@mkdir -p $(DIST)
	GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BIN_NAME)-$(1)-$(2)$(3) .
	@if [ "$(1)" = "darwin" ]; then \
		echo "==> macOS 二进制不支持 UPX，跳过压缩"; \
	elif [ -n "$(UPX)" ] && command -v $(UPX) >/dev/null 2>&1; then \
		$(UPX) $(UPX_FLAGS) $(DIST)/$(BIN_NAME)-$(1)-$(2)$(3) && echo "==> UPX 压缩完成"; \
	else \
		echo "==> 跳过 UPX（未安装或已禁用），保留未压缩版本"; \
	fi || echo "==> UPX 压缩失败，保留未压缩版本"
endef

$(eval $(call build-target,linux,amd64,))
$(eval $(call build-target,linux,arm64,))
$(eval $(call build-target,darwin,amd64,))
$(eval $(call build-target,darwin,arm64,))
$(eval $(call build-target,windows,amd64,.exe))

# 当前平台快速编译，便于本机调试与直接安装服务
.PHONY: dev
dev:
	@mkdir -p $(DIST)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN_NAME)$(EXE) .
	@if [ "$(GOOS)" = "darwin" ]; then \
		echo "==> macOS 二进制不支持 UPX，跳过压缩"; \
	elif [ -n "$(UPX)" ] && command -v $(UPX) >/dev/null 2>&1; then \
		$(UPX) $(UPX_FLAGS) $(DIST)/$(BIN_NAME)$(EXE) && echo "==> UPX 压缩完成"; \
	else \
		echo "==> 跳过 UPX（未安装或已禁用），保留未压缩版本"; \
	fi || echo "==> UPX 压缩失败，保留未压缩版本"
	@echo "==> 已生成 $(DIST)/$(BIN_NAME)$(EXE)"

# 本机安装为系统服务并启动（Windows 需管理员，Linux 需 sudo）
.PHONY: service
service: dev
	@echo "==> 安装系统服务"
	$(DIST)/$(BIN_NAME)$(EXE) install

.PHONY: service-uninstall
service-uninstall: dev
	$(DIST)/$(BIN_NAME)$(EXE) uninstall

.PHONY: service-status
service-status: dev
	$(DIST)/$(BIN_NAME)$(EXE) status

# 安装到 /usr/local/bin（仅 Linux/macOS）
.PHONY: install
install: linux-amd64
	@echo "==> 安装到 /usr/local/bin/$(BIN_NAME)"
	sudo install -m 0755 $(DIST)/$(BIN_NAME)-linux-amd64 /usr/local/bin/$(BIN_NAME)

.PHONY: test
test:
	go test ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: check
check: fmt vet test

.PHONY: clean
clean:
	rm -rf $(DIST)
	@echo "==> 已清理"
