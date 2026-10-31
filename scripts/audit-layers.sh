#!/usr/bin/env bash
# 分层边界审计（2026-09-23 审查结论）：核心不得反向依赖应用层；应用层不得直接依赖
# 浏览器内核。与旧 Makefile 内联版的关键差别：**区分「无匹配」与「扫描失败」**。
# rg 退出码 1 = 无匹配（正常）；>=2 = 真错误（如路径/权限问题），必须失败而不是放过。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

status=0

# run_rg <描述> <违规文案> <rg 参数...>
run_rg() {
  local desc="$1" viol="$2"; shift 2
  local out rc=0
  out="$(rg "$@" 2>&1)" || rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "audit-layers: ❌ $desc 扫描失败（rg rc=$rc）：" >&2
    echo "$out" >&2
    status=2
    return
  fi
  if [ -n "$out" ]; then
    echo "audit-layers: ❌ $viol"
    echo "$out"
    status=1
  fi
}

# 只在已 checkout 的路径上扫；全都不存在则跳过（本地只 checkout 部分目录的开发场景）。
existing() {
  local out=() p
  for p in "$@"; do [ -e "$p" ] && out+=("$p"); done
  printf '%s\n' "${out[@]+"${out[@]}"}"
}

# 1) 核心/界面 Go 包不得反向依赖应用层网关。
go_paths=($(existing pkg/engine pkg/browser pkg/matcher pkg/classifier pkg/verifier pkg/knowledge pkg/vlm cmd/uws cmd/uws-engine))
if [ "${#go_paths[@]}" -eq 0 ]; then
  echo "audit-layers: 跳过（核心 Go 包路径不存在）"
else
  run_rg "核心/界面 Go 包反向依赖" \
    "核心/界面 Go 包反向依赖应用层网关（apps/gateway / cmd/uws-gateway）" \
    -n -e '"uws/apps/gateway"' -e '"uws/cmd/uws-gateway"' \
    "${go_paths[@]}" --glob '*.go'
fi

# 2) coordinator 根级模块（除 cli.py 组合根外）不得反向依赖 coordinator.app。
if [ -e coordinator ]; then
  run_rg "coordinator 反向依赖" \
    "coordinator 核心模块反向依赖应用层（coordinator.app），仅 cli.py 组合根允许" \
    -n -e 'from coordinator\.app' -e 'import coordinator\.app' \
    coordinator --glob '*.py' --glob '!coordinator/app/**' --glob '!coordinator/cli.py'
else
  echo "audit-layers: 跳过（coordinator 不存在）"
fi

# 3) 应用层 apps/ 不得直接依赖浏览器内核 pkg/browser。
if [ -e apps ]; then
  run_rg "应用层依赖浏览器内核" \
    "应用层 apps/ 直接依赖浏览器内核 pkg/browser（应用层只应经 pkg/contract 或 gRPC 引擎接口）" \
    -n -e '"uws/pkg/browser"' apps --glob '*.go'
else
  echo "audit-layers: 跳过（apps 不存在）"
fi

if [ "$status" -ne 0 ]; then
  exit "$status"
fi
echo "audit-layers OK"
