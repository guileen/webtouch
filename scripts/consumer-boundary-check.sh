#!/usr/bin/env bash
# 消费方边界门禁（2026-09-23 审查结论，防复发而非当前解耦重构）：
# Agent Hub / AIGC 这类消费方只允许通过**支持边界**消费 WebTouch——
#   - CLI / JSON（`uws inspect`、`uws probe` 等）
#   - Agent Hub 客户端模型 API；WebTouch 私有 provider REST 仅由 Hub 消费
#   - 稳定的 Proto 客户端契约（proto/gen 下的存根）
# 禁止 import 引擎内部实现包（`uws/pkg/*`、`uws/cmd/*`、Python 侧 `uws.pkg`）：
# 那些包没有兼容承诺，一旦被消费方直接依赖，任何内部重构都会变成跨项目破坏。
#
# 现状：静态核对未发现越界（2026-09-23）。这条门禁的价值在将来——让第一次越界
# 在提交时被拦住，而不是在重构后的联调现场。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# 消费方相对本仓根；不存在则跳过（例如只 checkout 了 webtouch 时的本地开发）。
DEFAULT_CONSUMERS="../agent-hub ../../aigc"
CONSUMERS="${UWS_CONSUMER_PATHS:-$DEFAULT_CONSUMERS}"

violations=0
checked=0
skipped=0
for rel in $CONSUMERS; do
  case "$rel" in
    /*) dir="$rel" ;;
    *) dir="$ROOT/$rel" ;;
  esac
  if [ ! -d "$dir" ]; then
    echo "consumer-boundary: 跳过（不存在）$rel"
    skipped=$((skipped + 1))
    continue
  fi
  checked=$((checked + 1))
  # 区分「无匹配」与「扫描失败」：rg 退出码 1 = 无匹配（正常），>=2 = 真错误。
  # 旧实现用 `2>/dev/null || true` 把扫描错误也当成“无匹配”，会静默放过越界。
  rg_err="$(mktemp)"
  rg_rc=0
  hits="$(rg -n --no-heading \
      -e '"uws/pkg/' \
      -e '"uws/cmd/' \
      -e 'uws\.pkg\.' \
      -e 'from uws\.pkg' \
      "$dir" \
      --glob '*.go' --glob '*.py' \
      --glob '!**/.venv/**' --glob '!**/venv/**' --glob '!**/node_modules/**' --glob '!**/__pycache__/**' \
      2>"$rg_err")" || rg_rc=$?
  if [ "$rg_rc" -gt 1 ]; then
    echo "consumer-boundary: ❌ 扫描 $rel 失败（rg rc=$rg_rc），不能当作“无匹配”放过：" >&2
    cat "$rg_err" >&2
    rm -f "$rg_err"
    exit 1
  fi
  rm -f "$rg_err"
  if [ -n "$hits" ]; then
    echo "consumer-boundary: ❌ $rel 直接依赖引擎内部包（应走 CLI/JSON、Gateway REST 或 Proto 契约）："
    echo "$hits"
    violations=$((violations + 1))
  fi
done

if [ "$violations" -ne 0 ]; then
  exit 1
fi
echo "consumer-boundary OK（检查 $checked 个消费方，跳过 $skipped 个未 checkout 的；允许边界：CLI/JSON、Agent Hub 模型 API、Proto 客户端契约）"
