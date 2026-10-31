# 5 分钟快速上手 WebTouch

WebTouch（历史名 WebTouch，Universal Web Surrogate）是一套低成本、自进化、高确定性的浏览器自动化引擎。其核心思想是将大模型的“智能”离线蒸馏为规则与轻量模型，使**内核层**在线执行耗时低于 50ms、LLM 调用率低于 15%（内核级口径，应用层不计入）。

本文档将引导你在 5 分钟内完成环境准备、引擎启动并通过 gRPC 与 Python SDK 运行首个自动化登录任务。

---

## 1. 前置条件 (Prerequisites)

在开始前，请确保本地已安装：
- **Go** ≥ 1.22 或 **Rust** / **C++** 编译工具链
- **Python** ≥ 3.10
- **Chromium / Google Chrome**（物理浏览器环境）
- **工具**：`grpcurl`（命令行调试）、`protobuf` 编译器
- **基础依赖包**：
  ```bash
  pip install grpcio grpcio-tools
  brew install grpcurl protobuf  # macOS 环境
  ```

---

## 2. 克隆与构建 (Clone & Build)

```bash
# 1. 克隆仓库
git clone https://github.com/webtouch/uws.git
cd uws

# 2. 生成 Python gRPC 桩代码（基于 proto/uws_service.proto）
python -m grpc_tools.protoc \
  -I./proto \
  --python_out=./proto \
  --grpc_python_out=./proto \
  proto/uws_service.proto

# 3. 编译 WebTouch 在线引擎二进制
go build -o bin/uws-engine ./cmd/uws-engine
```

---

## 3. 启动引擎 (Start Server)

启动 WebTouch 引擎服务。引擎默认监听 `50051`（gRPC 通信端口）与 `8080`（可视化调试端口）：

```bash
./bin/uws-engine \
  --grpc-addr=127.0.0.1:50051 \
  --api-key-file=./keys.json \
  --knowledge-base-url=http://127.0.0.1:8000/artifacts \
  --knowledge-version=v1.5.5 \
  --max-targets=6 \
  --browser=cdp            # 真实 Chrome 物理执行层（缺省 sim=模拟会话）
  --chrome-path=/Applications/Google Chrome.app/Contents/MacOS/Google Chrome
```
> `--knowledge-base-url` 指向 pipeline/exporter 产出的 artifact 目录
> （HTTP 网关/对象存储预签名 URL），缺省则使用内置内存知识库。
> `--browser=cdp` 启用真实 CDP 会话：每 `InitializeContext` 启动独立 headless
> Chrome（临时 user-data-dir）；请求带 `attach_cdp_url` 时附着已有 Chrome 新建标签页。
> 控制台输出 `[INFO] UWS Engine v1.5.5 started on :50051` 即表示启动成功。

---

## 4. 服务健康检查 (HealthCheck)

使用 `grpcurl` 调用 `HealthCheck` 接口，验证物理层与内核状态：

```bash
grpcurl -plaintext \
  -proto proto/uws_service.proto \
  localhost:50051 uws.v1.UwsEngine/HealthCheck
```

**响应示例：**
```json
{
  "ok": true,
  "version": "v1.5.5",
  "active_targets": 0,
  "max_targets": 6
}
```

---

## 5. 初始化沙盒上下文 (InitializeContext)

每个任务必须先分配独立的 `BrowserContext`（等同于隔离沙盒，物理隔离 Cookie 与 Storage）：

```bash
grpcurl -plaintext \
  -proto proto/uws_service.proto \
  -d '{
    "task_id": "018f3a2b-7c10-7000-8000-000000000001",
    "initial_url": "https://example.com/login",
    "viewport": {"width": 1920, "height": 1080, "device_scale_factor": 1.0},
    "timezone": "Asia/Shanghai"
  }' \
  localhost:50051 uws.v1.UwsEngine/InitializeContext
```

**响应示例：**
```json
{
  "target_id": "target_89a1c0de",
  "context_id": "ctx_018f3a2b",
  "ok": true
}
```

---

## 6. 执行原子动作 (ExecuteAction)

向 WebTouch 引擎下发语义化原子操作意图。WASM 探针将以 ≤15ms 的感知延迟匹配节点并执行动作：

```bash
grpcurl -plaintext \
  -proto proto/uws_service.proto \
  -d '{
    "intent_id": "018f3a2b-7c10-7000-8000-000000000002",
    "target_semantic": "username_input",
    "expected_action": "input",
    "value": "alice",
    "timeout_ms": 1500
  }' \
  localhost:50051 uws.v1.UwsEngine/ExecuteAction
```

**响应示例：**
```json
{
  "intent_id": "018f3a2b-7c10-7000-8000-000000000002",
  "success": true,
  "confidence": 0.96,
  "context_state": "STABLE",
  "local_verification": {
    "value_updated": true,
    "probe_status": "mutated",
    "elapsed_ms": 12
  }
}
```

---

## 7. Python SDK 登录模板实战

在实际业务中，外部协调器通过标准模板自动化驱动业务流程。以下是完整的 Python 执行脚本：

```python
import uuid
import grpc
from proto import uws_service_pb2 as pb2
from proto import uws_service_pb2_grpc as pb2_grpc

def run_login_workflow():
    # 1. 建立 gRPC 通道
    channel = grpc.insecure_channel("localhost:50051")
    client = pb2_grpc.UwsEngineStub(channel)

    # 2. 初始化浏览器沙盒与目标页面
    task_id = str(uuid.uuid4())
    init_resp = client.InitializeContext(pb2.InitContextRequest(
        task_id=task_id,
        initial_url="https://example.com/login",
        viewport=pb2.ViewportConfig(width=1920, height=1080, device_scale_factor=1.0),
        timezone="Asia/Shanghai"
    ))
    if not init_resp.ok:
        raise RuntimeError(f"Context 初始化失败: {init_resp.error_reason}")
    print(f"✅ 沙盒就绪 | Target ID: {init_resp.target_id}")

    # 3. 定义通用登录步骤序列（对应 TaskTemplate 规范）
    workflow_steps = [
        {"semantic": "username_input", "action": "input", "value": "alice"},
        {"semantic": "password_input", "action": "input", "value": "SecretPassword123"},
        {"semantic": "login_button",   "action": "click", "value": None},
    ]

    # 4. 依次执行原子动作
    for step in workflow_steps:
        intent_id = str(uuid.uuid4())
        resp = client.ExecuteAction(pb2.ActionIntentRequest(
            intent_id=intent_id,
            target_semantic=step["semantic"],
            expected_action=step["action"],
            value=step["value"],
            timeout_ms=2000
        ))

        if not resp.success:
            raise RuntimeError(f"步骤 [{step['semantic']}] 执行失败: {resp.error_reason}")

        state_name = pb2.ContextState.Name(resp.context_state)
        print(f"⚡ [{step['semantic']}] 成功 | 置信度: {resp.confidence:.2f} | 状态: {state_name} | 耗时: {resp.local_verification.elapsed_ms}ms")

    print("🎉 登录流程执行完成！")

if __name__ == "__main__":
    run_login_workflow()
```

---

## 8. 下一步

- 查阅完整接口协议：[`proto/uws_service.proto`](../proto/uws_service.proto)
- 了解内核算法与状态机：[`spec/algorithm-kernel.md`](../spec/algorithm-kernel.md)
- 协调器与多步骤规划设计：[`spec/coordinator.md`](../spec/coordinator.md)
