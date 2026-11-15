# freeclash 构建脚本
#
# 常用：
#   make dev       当前平台快速编译到 ./dist
#   make all       交叉编译各平台发行包
#   make test      跑测试
#   make service   编译并把当前平台的二进制安装成本机系统服务

BIN_NAME := freeclash
DIST     := dist

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

GOFLAGS ?=

.PHONY: all
all: linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64
	@echo "==> 打包完成，见 $(DIST)/"

# $(1)=GOOS $(2)=GOARCH $(3)=扩展名
define build-target
.PHONY: $(1)-$(2)
$(1)-$(2):
	@echo "==> 构建 $(1)/$(2)"
	@mkdir -p $(DIST)
	GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BIN_NAME)-$(1)-$(2)$(3) .
	@upx -9 -q $(DIST)/$(BIN_NAME)-$(1)-$(2)$(3) 2>/dev/null || true
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
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN_NAME)$(shell go env GOEXE) .
	@echo "==> 已生成 $(DIST)/$(BIN_NAME)"

# 本机安装为系统服务并启动（Windows 需管理员，Linux 需 sudo）
.PHONY: service
service: dev
	@echo "==> 安装系统服务"
	$(DIST)/$(BIN_NAME) install

.PHONY: service-uninstall
service-uninstall: dev
	$(DIST)/$(BIN_NAME) uninstall

.PHONY: service-status
service-status: dev
	$(DIST)/$(BIN_NAME) status

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
