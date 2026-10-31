# WebTouch 工程入口（骨架阶段：保证可编译、可收集、可运行；实现随里程碑点亮）
# 与 docs/contributing.md、docs/tdd-checklist.md §0.3 对齐。
# 对外命令与正式二进制名是 webtouch / webtouch-engine / webtouch-gateway。
# uws / uws-engine / uws-gateway 只作为同产物软链（弃用别名）。Go module 路径仍是 uws。

PYTHON ?= .venv/bin/python
PYTEST ?= $(PYTHON) -m pytest
GO     ?= go
CMAKE  ?= cmake
# ---- 构建/版本（标准化，防孤儿进程 + 源码/进程版本漂移）----
# VERSION 文件是版本事实源；build 用 -ldflags 把「版本+git sha」注入引擎与 CLI 二进制，
# 使运行中的 EngineVersion / `webtouch version` 均可被 `webtouch daemon status` 的 HealthCheck 核对。
VERSION    ?= $(shell cat VERSION 2>/dev/null || echo dev)
GIT_SHA    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo nostamp)
BUILD_INFO  = $(VERSION)+$(GIT_SHA)
# 引擎包路径 = uws/pkg/engine（module 名未改）；CLI main 包注入须用其目录全路径 uws/cmd/uws（不能用 main.*）。
# 网关自身也注入构建标识（apps/gateway.BuildVersion）：/v1/health 的 version 字段 +
# 启动日志据此可被机器核对，专治「源码新版本 / 进程旧版本」漂移（make gateway-restart）。
GO_LDFLAGS  = -X uws/pkg/engine.EngineVersion=$(BUILD_INFO) -X uws/cmd/uws.buildVersion=$(BUILD_INFO) -X uws/apps/gateway.BuildVersion=$(BUILD_INFO)
# Emscripten（M2 WASM 探针；本机 emsdk 独立安装，EM_CACHE 指到可写目录以兼容沙箱）
EMCC   ?= $(shell command -v emcc 2>/dev/null || echo $(HOME)/emsdk/upstream/emscripten/emcc)
EMCMAKE ?= $(shell command -v emcmake 2>/dev/null || echo $(HOME)/emsdk/upstream/emscripten/emcmake)
EM_CACHE ?= $(CURDIR)/build/emcc-cache
EM_NODE ?= $(shell command -v node 2>/dev/null || echo $(HOME)/emsdk/node/24.19.0_64bit/bin/node)
# ONNX Runtime（A 案 ort-go CGO）：本机 macOS 13 x86_64 官方 wheel 止于 1.23.x，
# 从 .venv onnxruntime wheel 提取共享库；Linux 部署用官方 libonnxruntime.so.<ver>。
ORT_VERSION ?= 1.23.0
ORT_LIB ?= $(CURDIR)/build/ort/libonnxruntime.$(ORT_VERSION).dylib
ORT_SRC ?= $(firstword $(wildcard .venv/lib/*/site-packages/onnxruntime/capi/libonnxruntime.$(ORT_VERSION).dylib))


.PHONY: proto-gen proto-check build install release-local engine-restart gateway-restart version test test-go test-py lint fix pre-commit bench probe-test probe-wasm probe-bench ort-lib collect audit-consumers

## M1：proto 桩代码生成（本机无 protoc 时用 Docker：bufbuild/buf 或 python grpcio-tools）
proto-gen:
	mkdir -p proto/gen/go proto/gen/python
	protoc -I./proto --go_out=./proto/gen/go --go_opt=module=uws \
		--go-grpc_out=./proto/gen/go --go-grpc_opt=module=uws proto/uws_service.proto
	.venv/bin/python -m grpc_tools.protoc -I./proto \
		--python_out=./proto/gen/python --grpc_python_out=./proto/gen/python proto/uws_service.proto

## 契约门禁：生成物一致性（重新生成到临时目录后逐字节对比；漂移即失败）。
## 只回答「proto/*.proto 与 proto/gen 是否一致」，不替代新旧 Coordinator/Engine 组合验收。
proto-check:
	bash scripts/proto-check.sh

## 契约门禁：消费方（Agent Hub / AIGC）不得 import 引擎内部包 uws/pkg/*、uws/cmd/*。
## 这是未来架构守卫（当前无越界），确认边界变化时先改接口再放行。
audit-consumers:
	bash scripts/consumer-boundary-check.sh

## 版本+sha 打印（人/CI 可读）
version:
	@echo "WebTouch $(VERSION)  sha=$(GIT_SHA)  build_info=$(BUILD_INFO)"

## 标准化构建：产物统一放 bin/。
## 正式名：bin/webtouch、bin/webtouch-engine、bin/webtouch-gateway、bin/webtouch-lease。
## 弃用软链：bin/uws-engine、bin/uws-gateway、bin/uws-lease，以及仓库根 webtouch/uws。
## 源码目录仍是 cmd/uws、cmd/uws-engine、cmd/uws-gateway、cmd/uws-lease。
## 注：引擎完整能力还需 `make probe-wasm ort-lib`（WASM 探针 + ONNX 运行时库，均 gitignored）；
## probe-wasm 已并入 install，保证 webtouch probe 发布即可用。
build: version
	@echo "==> 构建引擎、CLI、网关与租约服务（ldflags: $(GO_LDFLAGS)）"
	mkdir -p bin
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/webtouch-engine ./cmd/uws-engine
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/webtouch-lease ./cmd/uws-lease
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/webtouch-gateway ./cmd/uws-gateway
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/webtouch ./cmd/uws
	ln -sfn webtouch-engine bin/uws-engine
	ln -sfn webtouch-lease bin/uws-lease
	ln -sfn webtouch-gateway bin/uws-gateway
	ln -sfn bin/webtouch webtouch
	ln -sfn bin/webtouch uws
	ln -sfn bin/webtouch-gateway webtouch-gateway
	ln -sfn bin/webtouch-gateway uws-gateway
	@echo "==> 构建完成"
	./webtouch version

## 本地 release：最近成功构建即可用。make install = build + probe-wasm + 固定 /usr/local/bin
## 入口（都指向仓库 bin/ 内产物，重 build 后自动生效；不依赖 launchd）。
## 运行中的引擎换新二进制请再执行 make engine-restart。
install: build probe-wasm
	ln -sfn $(CURDIR)/bin/webtouch /usr/local/bin/webtouch
	ln -sfn $(CURDIR)/bin/webtouch /usr/local/bin/uws
	ln -sfn $(CURDIR)/bin/webtouch-engine /usr/local/bin/webtouch-engine
	ln -sfn $(CURDIR)/bin/webtouch-engine /usr/local/bin/uws-engine
	ln -sfn $(CURDIR)/bin/webtouch-gateway /usr/local/bin/webtouch-gateway
	ln -sfn $(CURDIR)/bin/webtouch-gateway /usr/local/bin/uws-gateway
	@echo "==> 本地 release 完成：/usr/local/bin/{webtouch,uws,webtouch-engine,uws-engine,webtouch-gateway,uws-gateway} -> $(CURDIR)/bin（含 probe-wasm）"
	./webtouch daemon status

release-local: install

## 引擎守护进程重建重启：先 build（+sha）再 restart，杜绝「源码新版本/进程旧版本」漂移。
engine-restart: build
	./webtouch daemon restart

## 网关守护进程重建重启：只重建 `bin/webtouch-gateway`（不碰引擎二进制，避免引擎侧
## 「二进制新/进程旧」漂移），再刷新兼容软链，kickstart launchd 托管实例（label 仍是
## com.guileen.uws-gateway），最后把运行版本打出来供人/机器核对。
## 改完 `apps/gateway` 就该跑这一条，不要手工只换二进制不重启。
gateway-restart: version
	@echo "==> 重建网关（ldflags: $(GO_LDFLAGS)）"
	mkdir -p bin
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/webtouch-gateway ./cmd/uws-gateway
	ln -sfn webtouch-gateway bin/uws-gateway
	ln -sfn bin/webtouch-gateway webtouch-gateway
	ln -sfn bin/webtouch-gateway uws-gateway
	launchctl kickstart -k gui/$$(id -u)/com.guileen.uws-gateway
	@for i in 1 2 3 4 5; do \
	  curl -fsS http://127.0.0.1:8000/v1/health >/dev/null 2>&1 && break; \
	  sleep 1; \
	done
	@printf '==> 网关运行版本: '; curl -fsS http://127.0.0.1:8000/v1/health | python3 -c 'import sys,json;print(json.load(sys.stdin).get("version","(无 version 字段：进程可能仍是旧构建)"))'

## 可复现的 CI/测试回路：lint + 基础测试 + 契约 + 构建 + 版本自检。
ci: lint audit-layers audit-consumers proto-check test build
	./bin/webtouch version

## Go 测试（逻辑层，包内 *_test.go；probe_wasm 为 C++，走 probe-test）
test-go:
	$(GO) test -count=1 -v ./...

## Python 测试（单元 + 集成 + 安全；骨架阶段全 skip，收集与运行均为绿灯）
test-py:
	$(PYTEST) tests/unit tests/integration tests/security -q

## 全量基础测试（e2e 需真实 Chromium，单独执行；覆盖率门禁实现后启用）
test: test-go test-py

## 探针 native 测试（C++，TDD-WASM-*）
probe-test:
	$(CMAKE) -S pkg/probe_wasm -B build/probe_wasm
	$(CMAKE) --build build/probe_wasm
	ctest --test-dir build/probe_wasm --output-on-failure

## 探针 WASM 目标（Emscripten emsdk；16MB 线性内存硬边界）
probe-wasm:
	mkdir -p $(EM_CACHE)
	EM_CACHE=$(EM_CACHE) $(EMCMAKE) cmake -S pkg/probe_wasm -B build/probe_wasm_wasm -DUWS_BUILD_WASM=ON
	EM_CACHE=$(EM_CACHE) cmake --build build/probe_wasm_wasm

## 探针 WASM 性能基准（TDD-PERF-01：P99 < 15ms；wasm 缺失时自动 skip）
probe-bench: probe-wasm
	UWS_NODE=$(EM_NODE) $(PYTEST) benchmarks/bench_probe_wasm.py -q

## ONNX Runtime 共享库（A 案 ort-go CGO；从 .venv onnxruntime wheel 提取 dylib）
ort-lib:
	mkdir -p build/ort
	@if [ -z "$(ORT_SRC)" ]; then echo "未找到 onnxruntime $(ORT_VERSION) dylib（请先 uv sync 安装 dev 依赖）"; exit 1; fi
	cp "$(ORT_SRC)" "$(ORT_LIB)"
	@echo "ORT 共享库就绪: $(ORT_LIB)"

## lint：骨架阶段仅校验 Go 格式；ruff/mypy/clang-format 门禁随实现接入
lint:
	@test -z "$$(gofmt -l cmd pkg)" || (echo "gofmt 未通过"; exit 1)
	@echo "TODO(M2+): ruff check + ruff format --check + mypy + clang-format"

## 分层边界审计：核心禁止反向依赖应用层（Go：apps/gateway；Python：coordinator/app）。
## Go 侧查 pkg/* 与 cmd/uws*（核心/界面；目录名仍是 uws）是否 import 网关应用包；
## 应用层（apps/）禁止直接 import 浏览器内核 pkg/browser——跨层接口定义在 pkg/contract；
## Python 侧查 coordinator 根级模块（除 cli.py 组合根外）是否 import coordinator.app。
## 实现移到 scripts/audit-layers.sh：旧内联版把 rg 的“无匹配(1)”和“扫描失败(>=2)”
## 一起当通过，属 fail-open；脚本版只在无匹配时通过（2026-09-23 审查结论）。
audit-layers:
	@bash scripts/audit-layers.sh

## 自动格式化（实现后启用）
fix:
	@echo "TODO(M2+): ruff format + gofmt -w + clang-format -i"

## 提交前门禁：lint + 分层/消费方边界 + 契约生成物一致性 + 全量基础测试 + gRPC 契约回归
pre-commit: ci

## 性能基准（TDD-PERF-*；骨架阶段全 skip）
bench:
	$(PYTEST) benchmarks -q

## 收集审计：确认每个 TDD ID 的骨架文件可被收集
collect:
	$(PYTEST) --collect-only tests benchmarks -q
