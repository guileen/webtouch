# proto/gen

由 `make proto-gen` 生成的桩代码目录（单一事实源：`../uws_service.proto`）。

- `python/`：grpc_tools.protoc 生成（`uws_service_pb2.py` / `uws_service_pb2_grpc.py`）
- `go/proto/v1/`：protoc-gen-go / protoc-gen-go-grpc 生成
  （`--go_opt=module=uws` 按 `go_package: uws/proto/v1;uwspb` 落盘；
  Go 侧 import 路径为 `uws/proto/gen/go/proto/v1`）

按 docs/contributing.md §3：proto 改动与其生成的 bindings 必须同一提交原子落地。
工具链（本机已装）：`protoc`（brew protobuf）+ `protoc-gen-go` / `protoc-gen-go-grpc`
（go install）+ `grpcio-tools`（uv pip，位于 `.venv`）。
