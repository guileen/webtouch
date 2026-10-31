#!/usr/bin/env bash
# gateway-session-recover.sh — 网关 chat 502「会话绑定失活」最短恢复（2026-10-02 派单 d1f5493c6）
#
# 症状：内部模型目录可读，但 POST /internal/provider/chat 502（毫秒级返回），
#       错误文本含 `bound session target <tid> cannot be adopted ... 目标 <tid> 已不存在`。
# 根因：引擎把「会话名 → Target」落盘（默认 ~/.uws/sessions.json，TTL 24h）；同名会话在
#       内存无活跃上下文时按该绑定续页，且 RequireReuseTarget=true ⇒ 目标页签已被关掉时
#       fail-closed 硬失败；运行期没有 API 能清绑定（CloseContext 无内存上下文时提前返回）。
# 手法：删掉「目标已不存在」的绑定（**保留活绑定**）→ 重启引擎 →（可选）冒烟。
# 用法：scripts/gateway-session-recover.sh [--dry-run] [--smoke]
#   --dry-run  只报会删哪些绑定，不落盘、不重启
#   --smoke    重启后真发一条测试消息（会在站点留一条会话，需自行清账）
set -euo pipefail

CDP="${UWS_CDP:-http://127.0.0.1:9222}"
GATEWAY="${UWS_GATEWAY:-http://127.0.0.1:8000}"
SESSIONS="${UWS_SESSIONS_JSON:-$HOME/.uws/sessions.json}"
CLI="${UWS_CLI:-webtouch}"
MODEL="${UWS_SMOKE_MODEL:-deepseek-chat}"
DRY=0
SMOKE=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    --smoke)   SMOKE=1 ;;
    *) echo "未知参数：$arg"; exit 2 ;;
  esac
done

[ -f "$SESSIONS" ] || { echo "找不到绑定表：$SESSIONS"; exit 1; }

# 先证现场：chat 502 才值得动这个脚本（只看网络可达 + 错误文本，不改任何东西）
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GATEWAY/internal/provider/models" || true)"
echo "前置读数：GET /internal/provider/models → http=${code:-无}"

# CDP 目标表：取不到就中止——否则会把全部活绑定误判为死绑定
tmp="$(mktemp -t gateway-session-recover)"
trap 'rm -f "$tmp" "$tmp.count"' EXIT
if ! curl -fsS --max-time 8 "$CDP/json/list" -o "$tmp"; then
  echo "CDP 不可达（$CDP）：已中止，未改任何文件"; exit 1
fi

if [ "$DRY" = 1 ]; then
  python3 - "$SESSIONS" "$tmp" <<'PY'
import json, sys
p, tp = sys.argv[1], sys.argv[2]
live = {t["id"] for t in json.load(open(tp))}
entries = json.load(open(p)).get("entries", {})
dead = [k for k, v in entries.items() if v.get("target_id") not in live]
print("dry-run：死绑定 %d 条 %s；活绑定 %d 条" % (len(dead), dead, len(entries) - len(dead)))
PY
  exit 0
fi

cp -p "$SESSIONS" "${SESSIONS}.bak-$(date +%Y%m%d-%H%M%S)"
python3 - "$SESSIONS" "$tmp" "$tmp.count" <<'PY'
import json, sys
p, tp, cp = sys.argv[1], sys.argv[2], sys.argv[3]
live = {t["id"] for t in json.load(open(tp))}
doc = json.load(open(p))
entries = doc.get("entries", {})
keep = {k: v for k, v in entries.items() if v.get("target_id") in live}
dropped = [k for k in entries if k not in keep]
doc["entries"] = keep
open(p, "w").write(json.dumps(doc, ensure_ascii=False))
open(cp, "w").write(str(len(dropped)))
print("删除死绑定 %d 条：%s" % (len(dropped), dropped))
print("保留活绑定 %d 条：%s" % (len(keep), list(keep)))
PY

if [ "$(cat "$tmp.count")" = "0" ]; then
  echo "绑定表无死条目 ⇒ chat 502 另有原因，未重启引擎（别急着 restart）"; exit 1
fi

"$CLI" daemon restart
sleep 3
"$CLI" daemon status | head -2

if [ "$SMOKE" = 1 ]; then
  echo "冒烟（会在站点留下一条测试会话，记得清账）："
  curl -s -w '\nhttp=%{http_code} t=%{time_total}s\n' --max-time 180 \
    -X POST "$GATEWAY/internal/provider/chat" -H 'Content-Type: application/json' \
    -d "{\"channel_id\":\"${UWS_SMOKE_CHANNEL:-deepseek-web}\",\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"只回答两个字：收到\"}]}" | head -c 800
else
  echo "已恢复绑定表并重启引擎；如需端到端确认，加 --smoke 再跑一次（会留站点会话）。"
fi
