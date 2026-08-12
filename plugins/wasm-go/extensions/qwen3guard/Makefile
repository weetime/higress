# Qwen3Guard Plugin Makefile
#
# ⚠️ 构建必须在 Higress 的构建树里进行，不能在本仓库直接跑。
#    wasm-go 的 Dockerfile 是 `COPY . .` 整个 plugins/wasm-go 目录，然后
#    `WORKDIR /workspace/extensions/$PLUGIN_NAME`，所以源码必须位于
#    <higress>/plugins/wasm-go/extensions/qwen3guard/ 才编得出来。
#
#    本仓库的目录是**归档**，构建前先同步到构建树（用 cp，不要用软链——
#    docker build 打包上下文时不解引用符号链接，容器里会拿到死链）：
#
#      cp -r qwen3guard $(HIGRESS_ROOT)/plugins/wasm-go/extensions/
#      cd $(HIGRESS_ROOT)/plugins/wasm-go && PLUGIN_NAME=qwen3guard make build-push IMG=...
#
#    改完代码记得同步回本仓库，发版前 diff 确认两边一致：
#      diff -r qwen3guard $(HIGRESS_ROOT)/plugins/wasm-go/extensions/qwen3guard

PLUGIN_NAME := qwen3guard
# 镜像路径按 rise 自研插件约定 <hub>/camp/<name>（mcp-server-auth 多一层 higress/ 是特例）
REGISTRY ?= quanzhenglong.com/camp
VERSION ?= v0.0.1
IMG ?= $(REGISTRY)/$(PLUGIN_NAME):$(VERSION)

# Higress 构建树位置，按实际 checkout 覆盖，例如：
#   make build-push HIGRESS_ROOT=~/rise/weetime/higress
HIGRESS_ROOT ?= ../../../weetime/higress
WASM_GO_DIR := $(HIGRESS_ROOT)/plugins/wasm-go

.PHONY: help
help:
	@echo "Qwen3Guard Plugin Build Targets:"
	@echo "  make sync         - 把本目录同步到 Higress 构建树的 extensions/ 下"
	@echo "  make build        - 构建 wasm（先 sync）"
	@echo "  make build-push   - 构建并推送插件镜像（先 sync）"
	@echo "  make test         - 运行单元测试（本目录即可，不需要构建树）"
	@echo "  make diff         - 对比本目录与构建树那份是否一致"
	@echo ""
	@echo "  PLUGIN_NAME  = $(PLUGIN_NAME)"
	@echo "  IMG          = $(IMG)"
	@echo "  HIGRESS_ROOT = $(HIGRESS_ROOT)"

.PHONY: sync
sync:
	@test -d $(WASM_GO_DIR) || { echo "❌ 找不到构建树 $(WASM_GO_DIR)，用 HIGRESS_ROOT=<path> 指定"; exit 1; }
	cp -r . $(WASM_GO_DIR)/extensions/$(PLUGIN_NAME)
	@echo "✅ 已同步到 $(WASM_GO_DIR)/extensions/$(PLUGIN_NAME)"

.PHONY: diff
# Makefile 和 wasmplugin.yaml 只存在于本仓库（构建树不需要），排除掉只比源码
diff:
	diff -r -x Makefile -x wasmplugin.yaml . $(WASM_GO_DIR)/extensions/$(PLUGIN_NAME) \
	  && echo "✅ 源码两份一致"

.PHONY: build
build: sync
	cd $(WASM_GO_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) $(MAKE) build

.PHONY: build-push
build-push: sync
	cd $(WASM_GO_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) $(MAKE) build-push
	@echo ""
	@echo "✅ 已推送: $(IMG)"
	@echo "   下一步：把它加进 higress-helm/plugin-server-build/plugins.properties（新版置顶，"
	@echo "   保留上一版），跑 ./build.sh 重建 plugin-server，再更新 values.yaml 的 pluginServer.tag"

.PHONY: test
test:
	go test -v ./...

.PHONY: clean
clean:
	rm -f plugin.wasm main.wasm coverage.out coverage.html
