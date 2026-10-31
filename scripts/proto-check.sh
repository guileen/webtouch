#!/usr/bin/env bash
# Proto 生成物一致性门禁（2026-09-23 审查结论）：把 .proto 重新生成到临时目录，
# 与仓内已提交的 proto/gen 逐字节对比。漂移即失败——防止「改了 .proto 忘了跑
# proto-gen」或「生成物被手改」这类只在联调时才爆炸的契约事故。
#
# 这是低成本门禁：先于 buf lint / buf breaking（那两项需要固定基线与 Buf 配置）。
# 它只回答「生成物是否新鲜」，不替代新旧 Coordinator/Engine 组合验收，
# 也不保证消费者能处理未知枚举。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PY="${PY:-$ROOT/.venv/bin/python}"
if ! command -v protoc >/dev/null 2>&1; then
  echo "proto-check: 未找到 protoc（本机需 protoc + protoc-gen-go + protoc-gen-go-grpc；无 protoc 时用 Docker bufbuild/buf 见 Makefile proto-gen 注释）" >&2
  exit 2
fi
if [ ! -x "$PY" ]; then
  echo "proto-check: 未找到 $PY（Python 生成物需要 grpc_tools；先 uv sync / 装 dev 依赖）" >&2
  exit 2
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/proto/gen/go" "$TMP/proto/gen/python"

protoc -I./proto \
  --go_out="$TMP/proto/gen/go" --go_opt=module=uws \
  --go-grpc_out="$TMP/proto/gen/go" --go-grpc_opt=module=uws \
  proto/uws_service.proto

"$PY" -m grpc_tools.protoc -I./proto \
  --python_out="$TMP/proto/gen/python" \
  --grpc_python_out="$TMP/proto/gen/python" \
  proto/uws_service.proto

# 只比对两个生成子树：proto/gen/README.md 是手写说明，不属于生成物。
drift=0
for sub in go python; do
  if ! diff -ru --exclude=__pycache__ "proto/gen/$sub" "$TMP/proto/gen/$sub"; then
    drift=1
  fi
done
if [ "$drift" -ne 0 ]; then
  echo "" >&2
  echo "proto-check: 生成物漂移（上面的 diff 左=仓内，右=重新生成）。请执行 make proto-gen 并提交生成物。" >&2
  exit 1
fi
echo "proto-check OK：proto/gen 与 proto/*.proto 一致"
