#!/usr/bin/env bash
# WebTouch 批量采集调度器（看门狗）——`INT-219` / `DEP-217`；落点入仓以满足 `CON-219`。
#
# 目的：在**引擎 24 槽硬顶**下支撑 500 样本连续自动化吞吐。槽位耗尽时 `uws run` 会
# `initialize_context: target pool exhausted` **静默不产 Episode**（1 会话名 = 1 槽，
# `run` 路径不释放），故必须按站数**主动硬杀重启引擎**；引擎重启实走 SIGKILL、
# **不回收 Chrome 子进程**，故必须配套**按唯一 profile 精确匹配**的孤儿清理。
#
# 硬约束（来源：`AGENTS.md` 授权矩阵 / `CON-217` / `CON-218` / handoff「批次配方」）：
#  1. 只操作**自建** Chrome：唯一 `--user-data-dir` 精确匹配；**9222（宿主）绝对只读**，
#     本脚本从端口与 profile 两处硬拒 9222，绝不 `pkill Chrome`（无精确匹配不杀）。
#  2. 每 N 站（默认 8；硬上限 24）强制重启引擎；`UWS_ENGINE_ARGS`（影子参数）必须原样保留，
#     否则重启会丢掉影子通道 ⇒ 采集批次失去影子对拍价值。
#  3. `DEEPSEEK_API_KEY` 必须**已导出**，否则拒绝重启——裸赋值不导出会让引擎静默禁用 VLM。
#  4. 每次重启后自检 `~/.uws/engine.log`：**必须**出现「影子通道已启用」；**一旦**出现
#     「onnx scorer 就绪」立即停机（＝决策分已进主决策链，违 `CON-218` / `INT-222`）。
#  5. 每站一条 `uws run`；站内不并发（并发会抢同一池）。站点清单须自行排除登录/人机验证页。
#  6. **单站墙钟上限**（`SITE_TIMEOUT`，默认 180s；`--site-timeout 0` 关闭）：个别站点会让
#     `uws run` **无限挂起**（`OBS-233`：输出 0 字节、引擎日志停在最后一条 `[WSTN-SHADOW]`、
#     Go CLI 停在 `wait4` 等 python 协调器、python 只挂着到 `50051` 的 gRPC 连接），不报错
#     也不退出 ⇒ 整批静默停摆。超时即终止本站（含 `pgrep -P` 精确回收子进程）并回 rc=124，
#     调度器照常记账续跑下一站。**只杀本站自建 run 的进程树**，不碰宿主 9222 与用户窗口。
#
# 重启边界的两件事（顺序固定，勿调换）：① 硬杀重启引擎（清 target 池缓存；`UWS_ENGINE_ARGS`
# 原样保留）；② **精确**回收自建 profile 的 Chrome 进程并**重新拉起**干净实例——引擎 SIGKILL
# 不回收 Chrome 子进程。**默认不杀活浏览器**：实测杀掉活动中实例后 headless Chrome 不再存活、
# 后续站点全部 `no browser is open`；只在 9224 已无服务时清孤儿并兜底拉起。需要显式回收请传
# `--recycle-chrome`（仅确认可安全重启浏览器时用）。
#
# 用法：
#   scripts/wt-batch-scheduler.sh --selftest                 # 守卫自检（不联网、不起进程）
#   scripts/wt-batch-scheduler.sh --dry-run --list <清单>     # 只打印计划，无副作用
#   scripts/wt-batch-scheduler.sh --list <清单> [--tag b6] [--restart-every 8]
#                                [--port 9224] [--profile /private/tmp/wtx-chrome-9224]
#                                [--out-dir /private/tmp/wt-throughput] [--close-pages] [--recycle-chrome]
#                                [--plan-override 'click:Documentation']  # 语料统一换计划族（实测产出差 4 倍）
#                                [--site-timeout 180]                     # 单站墙钟上限秒数（0=不限；超时 rc=124）
#                                [--exclude-domains <文件>]                # 已采域黑名单（一行一二极域；命中整站跳过、不耗槽位）
#
# 清单格式（一行一站，`|` 分隔；同 `run_list.sh`）：`name|url|plan`
#   例：`mdn|https://developer.mozilla.org/en-US/|input:search=webtouch`
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PORT=9224
PROFILE=/private/tmp/wtx-chrome-9224
OUT_DIR=/private/tmp/wt-throughput
RESTART_EVERY=8
HARD_SLOT_CAP=24
TAG=b6
SITE_TIMEOUT="${SITE_TIMEOUT:-180}"   # 单站墙钟上限（秒）；0=不限（仅排障用，见 `OBS-233`）
LIST=""
EXCLUDE=""
MODE=run
CLOSE_PAGES=0
KEEP_CHROME=0
PLAN_OVERRIDE=""
CHROME_BIN="${UWS_SCHED_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"

die() { echo "wt-batch-scheduler: ❌ $*" >&2; exit 2; }
info() { echo "wt-batch-scheduler: $*"; }

# ── 守卫（纯函数，供 --selftest 直接驱动）──────────────────────────────────────

# 端口守卫：默认只允许**自建** 9224 与 9300–9399 的临时区间；
# 显式拒绝宿主 9222、引擎 gRPC 50051、网关等其它已占端口。
assert_self_built_port() {
  local p="$1"
  case "$p" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$p" -eq 9222 ] && return 1
  [ "$p" -eq 50051 ] && return 1
  { [ "$p" -eq 9224 ] || { [ "$p" -ge 9300 ] && [ "$p" -le 9399 ]; }; } || return 1
  return 0
}

# profile 守卫：必须是 /private/tmp 或 $TMPDIR 下的**自建临时** profile；
# 拒绝空值、拒绝 `/Users/**`（宿主日常 profile）、拒绝 `/` 与过长泛化路径。
assert_self_built_profile() {
  local d="$1"
  [ -n "$d" ] || return 1
  case "$d" in
    /) return 1 ;;
    /Users/*) return 1 ;;
    /private/tmp/*) ;;
    /var/folders/*) ;;
    *) return 1 ;;
  esac
  case "$d" in
    *chrome-debug*) return 1 ;;   # 宿主调试 profile（9222）
  esac
  return 0
}

# 命令行匹配谓词：仅当命令行含**本脚本唯一 profile**且是 Chrome 时可清理。
cmdline_is_self_built() {
  local cmd="$1" d="$2"
  [ -n "$cmd" ] || return 1
  case "$cmd" in
    *Google\ Chrome*) ;;
    *) return 1 ;;
  esac
  case "$cmd" in
    *"--user-data-dir=$d"*) return 0 ;;
  esac
  return 1
}

# ── 自检 ──────────────────────────────────────────────────────────────────────

selftest() {
  local fail=0
  chk() { # chk <期望 0|1> <描述> <命令...>
    local want="$1" desc="$2"; shift 2
    local rc=0; "$@" || rc=$?
    if [ "$rc" -eq "$want" ]; then echo "  ✅ $desc (rc=$rc)"; else echo "  ❌ $desc 期望 rc=$want 实得 rc=$rc"; fail=1; fi
  }
  echo "== 端口守卫 =="
  chk 1 "拒绝宿主 9222"            assert_self_built_port 9222
  chk 0 "接受自建 9224"            assert_self_built_port 9224
  chk 0 "接受临时 9333"            assert_self_built_port 9333
  chk 1 "拒绝 50051（引擎 gRPC）"  assert_self_built_port 50051
  chk 1 "拒绝 9400（越出临时区间）" assert_self_built_port 9400
  chk 1 "拒绝空值"                 assert_self_built_port ""
  chk 1 "拒绝非数字"               assert_self_built_port 92x4
  echo "== profile 守卫 =="
  # 夹具账号 example 只为命中 /Users/* 拒绝分支，不指向真实本机用户。
  local host_profile="/Users/example/.uws-chrome-debug"
  chk 0 "接受 /private/tmp/wtx-chrome-9224" assert_self_built_profile /private/tmp/wtx-chrome-9224
  chk 1 "拒绝宿主 ~/.uws-chrome-debug"      assert_self_built_profile "$host_profile"
  chk 1 "拒绝任意 /Users/**"                assert_self_built_profile "/Users/example/Library/Chrome"
  chk 1 "拒绝 /"                            assert_self_built_profile /
  chk 1 "拒绝空值"                          assert_self_built_profile ""
  echo "== 孤儿匹配谓词 =="
  chk 0 "命中自建 9224 Chrome" \
    cmdline_is_self_built "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --headless=new --remote-debugging-port=9224 --user-data-dir=/private/tmp/wtx-chrome-9224 about:blank" /private/tmp/wtx-chrome-9224
  chk 1 "不命中宿主 9222 Chrome" \
    cmdline_is_self_built "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --user-data-dir=${host_profile} --remote-debugging-port=9222" /private/tmp/wtx-chrome-9224
  chk 1 "不命中同名非 Chrome 进程" \
    cmdline_is_self_built "python3 watch.py --user-data-dir=/private/tmp/wtx-chrome-9224" /private/tmp/wtx-chrome-9224
  chk 1 "不命中空命令行" cmdline_is_self_built "" /private/tmp/wtx-chrome-9224
  echo "== 槽位账本 =="
  chk 0 "8 站 < 硬顶 24" _ledger_ok 8 24
  chk 0 "24 站 = 硬顶 24" _ledger_ok 24 24
  chk 1 "25 站 > 硬顶 24" _ledger_ok 25 24
  echo "== 影子参数守卫（env 名＝UWS_ENGINE_ARGS，OBS-235）=="
  chk 0 "接受非空影子参数" _engine_args_present "--onnx-runtime-lib=/x --shadow-onnx-model=/y"
  chk 1 "拒绝空影子参数"   _engine_args_present ""
  chk 0 "设了 UWS_ENGINE_ARGS ⇒ 放行"  _args_pass UWS_ENGINE_ARGS
  chk 1 "只设 n 未设 UWS_ENGINE_ARGS ⇒ 拒绝" _args_pass n
  chk 1 "两者都空 ⇒ 拒绝"              _args_pass none
  echo "== 单站墙钟上限（OBS-233）=="
  chk 124 "超时⇒终止进程树并回 124" run_capped 1 sleep 5
  chk 0   "未超时⇒原样透传 rc"       run_capped 5 true
  echo "== 域名排除（--exclude-domains）=="
  chk 0 "解析 https 带路径" _host_is developer.hashicorp.com https://developer.hashicorp.com/consul
  chk 0 "解析 http 带端口/查询" _host_is example.com 'http://example.com:8080/x?y=1'
  chk 0 "解析裸域名"        _host_is example.com example.com
  chk 0 "命中排除域"        _excluded_probe example.com
  chk 0 "www. 变体命中"     _excluded_probe www.example.com
  chk 0 "二极域命中子域"    _excluded_probe docs.example.com
  chk 0 "多段后缀二极域"    _excluded_probe example.co.uk
  chk 0 "多段后缀带子域"    _excluded_probe blog.example.co.uk
  chk 1 "未命中不排除"      _excluded_probe not-excluded.dev
  echo "== 日志标记探测（OBS-236：pipefail 下 tail|grep -q 会因 SIGPIPE 假阴性）=="
  chk 0 "大日志、标记在窗口内 ⇒ 命中" _log_probe_hit
  chk 1 "无标记 ⇒ 不命中"             _log_probe_miss
  if [ "$fail" -eq 0 ]; then echo "== ✅ selftest 全过 =="; return 0; fi
  echo "== ❌ selftest 有失败 =="; return 1
}

_ledger_ok() { [ "$1" -le "$2" ]; }

_host_is() { [ "$(_host_of "$2")" = "$1" ]; }
_args_pass() { # 子 shell 里跑真守卫（避免 die 终止自检）；$1=要设的变量名。放行 0 / 拒绝 1
  ( unset UWS_ENGINE_ARGS n
    case "$1" in
      UWS_ENGINE_ARGS) UWS_ENGINE_ARGS="--shadow-onnx-model=/x" ;;
      n)               n="--shadow-onnx-model=/x" ;;
    esac
    require_engine_args >/dev/null 2>&1 ) && return 0
  return 1
}
_excluded_probe() { # 小夹具：临时排除文件，测完即删（不联网、不留盘）
  local f; f="$(mktemp "${TMPDIR:-/tmp}/wt-excl.XXXXXX")"
  printf 'example.com\nhaskell.org\nexample.co.uk\n' > "$f"
  _excluded "$1" "$f"; local rc=$?
  rm -f "$f"; return "$rc"
}

_log_probe_hit() { # 夹具：标记在窗口**前部**、总量 >64KB 管道缓冲（旧写法 `tail|grep -q` 在此必假阴性）
  local f; f="$(mktemp "${TMPDIR:-/tmp}/wt-log.XXXXXX")"
  { echo "影子通道已启用"; local i; for i in $(seq 1 900); do echo "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; done; } > "$f"
  _log_has "$f" "影子通道已启用" 1000; local rc=$?
  rm -f "$f"; return "$rc"
}
_log_probe_miss() { # 夹具：同规模但无标记
  local f; f="$(mktemp "${TMPDIR:-/tmp}/wt-log.XXXXXX")"
  { local i; for i in $(seq 1 900); do echo "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; done; } > "$f"
  _log_has "$f" "影子通道已启用" 1000; local rc=$?
  rm -f "$f"; return "$rc"
}

# ── 单站墙钟上限（`OBS-233`）──────────────────────────────────────────────────
# 实测（2026-09-26 b7f3）：`redwoodjs.com` / `adonisjs.com` 两站让 `uws run` **无限挂起**——
# 输出文件 0 字节、引擎日志停在最后一条 `[WSTN-SHADOW]`、Go CLI 停在 `wait4`（等其 python
# 协调器子进程）、python 只挂着一条到 `127.0.0.1:50051` 的 ESTABLISHED gRPC 连接；**不报错、
# 不退出**，整批静默停摆。故给每站加墙钟上限：超时即终止本站**进程树**并回 rc=124
# （同 GNU `timeout` 约定），调度器照常记账、续跑下一站。`limit<=0` ⇒ 不限（仅排障用）。
run_capped() { # run_capped <秒> <命令...>
  local limit="$1"; shift
  case "$limit" in ''|*[!0-9]*) "$@"; return $? ;; esac
  [ "$limit" -gt 0 ] || { "$@"; return $?; }
  "$@" &
  local pid=$! kids="" waited=0 g=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$waited" -ge "$limit" ]; then
      # 先记子进程（父死后 `pgrep -P` 就查不到了），再整树终止。
      kids="$(pgrep -P "$pid" 2>/dev/null | tr '\n' ' ')"
      echo "  ⏱ 单站墙钟超时 ${limit}s（pid=$pid）⇒ 终止本站进程树，避免整批静默停摆（OBS-233）" >&2
      kill -TERM "$pid" 2>/dev/null
      [ -n "$kids" ] && kill -TERM $kids 2>/dev/null
      while kill -0 "$pid" 2>/dev/null && [ "$g" -lt 5 ]; do sleep 1; g=$((g+1)); done
      kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
      return 124
    fi
    sleep 1; waited=$((waited+1))
  done
  wait "$pid"; return $?
}

# ── 运行态依赖 ─────────────────────────────────────────────────────────────────

# ── 域名排除（单域配额 `N_sample ≤ 2` 的**前置**执行面）────────────────────────
# 语料按站生成，但配额约束作用在**二极域**上（M1 判据 `独立二极域 ≥450`）；已采二极域再跑
# 只会稀释域名宽度。故支持 `--exclude-domains <文件>`（一行一个**二极域**，`#` 开头忽略；与
# 读数口径 `audit.py` 的 `reg2` 一致）：命中即**整站跳过并记账**，不消耗引擎槽位（不计入
# `--restart-every`）。文件缺失 ⇒ **fail-closed 报错退出**，不静默放行。比对时把站点 host 归一到
# 二极域（兼容 `www.` 前缀与子域：`docs.example.com` / `www.example.com` 均命中 `example.com`）。
_host_of() { # _host_of <url> → host（去 scheme/userinfo/path/query/port）
  local u="$1"
  u="${u#*://}"; u="${u%%/*}"; u="${u%%\?*}"; u="${u%%#*}"; u="${u##*@}"; u="${u%%:*}"
  printf '%s' "$u"
}

_reg2_of() { # _reg2_of <host> → 二极域（与语料读数 `audit.py` 的 reg2 同口径：co.uk 类多段后缀保留三级）
  local h="${1#www.}" s2 tld n
  local IFS='.'
  local -a p=($h)   # 依赖 IFS 分词（host 不含 glob 字符）
  n=${#p[@]}
  if [ "$n" -ge 3 ]; then
    s2="${p[$((n-2))]}"; tld="${p[$((n-1))]}"
    case "$s2" in co|com|org|net|ac|gov)
      case "$tld" in uk|cn|jp|au|kr|tw|hk) printf '%s\n' "${p[$((n-3))]}.$s2.$tld"; return ;; esac ;;
    esac
  fi
  if [ "$n" -ge 2 ]; then printf '%s\n' "${p[$((n-2))]}.${p[$((n-1))]}"; else printf '%s\n' "$h"; fi
}

_excluded() { # _excluded <host> <文件>：命中 0 / 未命中 1 / 文件不存在 2（按二极域比对）
  [ -n "$2" ] || return 1
  [ -f "$2" ] || return 2
  local h="$1"
  [ -n "$h" ] || return 1
  local bare="${h#www.}" reg
  reg="$(_reg2_of "$h")"
  # 单次多模式匹配（3 次 grep ⇒ 1 次，350 站预演由 ~60s 降到数秒）
  grep -qxF -e "$h" -e "$bare" -e "$reg" "$2" 2>/dev/null && return 0
  return 1
}

# 影子参数守卫。**实测订正（`OBS-235`，2026-09-26）**：`uws daemon restart` 实际读取的环境变量是
# **`UWS_ENGINE_ARGS`**（`cmd/uws/daemon.go:117` 的 `engineArgs()` 是唯一入口；全仓 Go 代码**没有**
# 读名为 `n` 的环境变量）。`OBS-230` 当年「引擎读 `n`、`UWS_ENGINE_ARGS` 只是历史别名」的结论
# **反了** ⇒ 只 `export n=...` 会**静默丢掉影子参数**（`check_engine_log` 只告警不 `die`）。故本守卫
# 改为**要求 `UWS_ENGINE_ARGS` 非空**，并把「只设了 n」当**拒绝项**（正是要防的静默降级）。
_engine_args_present() { [ -n "$1" ]; }

require_engine_args() {
  if ! _engine_args_present "${UWS_ENGINE_ARGS:-}"; then
    if _engine_args_present "${n:-}"; then
      die "只设了 n 但没设 UWS_ENGINE_ARGS ⇒ 引擎（cmd/uws/daemon.go 的 engineArgs）读的是 UWS_ENGINE_ARGS，重启后影子参数会静默丢失（实测 OBS-235）。请 export UWS_ENGINE_ARGS=\"--onnx-runtime-lib=... --shadow-onnx-model=...\" 后再跑；若确要无影子采集，显式传 --no-shadow-ok。"
    fi
    die "UWS_ENGINE_ARGS（影子 ONNX 参数）为空 ⇒ 重启会丢掉影子参数（采集批失去影子对拍价值）。请先 export UWS_ENGINE_ARGS=\"--onnx-runtime-lib=... --shadow-onnx-model=...\" 后再跑；若确要无影子采集，显式传 --no-shadow-ok。"
  fi
}

require_api_key() {
  if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
    die "DEEPSEEK_API_KEY 未导出 ⇒ 引擎会静默禁用 VLM 兜底。请先 export（~/.zshrc 已是 export，用 zsh -ic 即可）。"
  fi
}

http_code() {
  local c
  c="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$1/json/version" 2>/dev/null || true)"
  [ -n "$c" ] || c=000
  printf '%s' "$c"
}

ensure_chrome() {
  local code; code="$(http_code "$PORT")"
  if [ "$code" = "200" ]; then info "自建 Chrome 已在 :$PORT 就绪"; return 0; fi
  info "自建 Chrome 未就绪（:$PORT → $code），拉起：profile=$PROFILE"
  "$CHROME_BIN" --headless=new --no-first-run --no-default-browser-check \
    --remote-debugging-port="$PORT" --remote-debugging-address=127.0.0.1 \
    --user-data-dir="$PROFILE" about:blank >/dev/null 2>&1 &
  disown $! 2>/dev/null || true   # 脱离子 shell 作业表，避免回收旧实例时打出 "Killed: 9" 噪声
  local i
  for i in $(seq 1 30); do
    sleep 0.5
    code="$(http_code "$PORT")"
    [ "$code" = "200" ] && { info "自建 Chrome 已就绪（等待 ${i}×0.5s）"; return 0; }
  done
  die "自建 Chrome 拉起超时（:$PORT 仍未 200）"
}

engine_pid() { ./uws daemon status 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1; }

restart_engine() {
  local before after
  before="$(engine_pid)"
  info "硬杀重启引擎（before pid=${before:-?}）"
  ./uws daemon restart >/dev/null 2>&1 || die "uws daemon restart 失败"
  sleep 1
  after="$(engine_pid)"
  [ -n "$after" ] || die "重启后取不到引擎 pid"
  [ "$after" != "$before" ] || die "重启后 pid 未变（$after）——引擎可能未被硬杀"
  check_engine_log || die "重启自检失败：见上方日志判定"
  info "引擎重启完成：pid=$after"
}

# 日志标记探测：**不得用 `grep -q`**——本脚本开了 `set -o pipefail`，`grep -q` 命中后立即退出会让
# `tail` 收 SIGPIPE 并回 141，pipefail 取「最右非零」⇒ **命中也被判成未命中**（`OBS-236`，2026-09-26
# b10b 开工前自检实测：`tail -n 400 | grep -aq` 回 141，`grep -a … >/dev/null` 回 0；tail 输出
# 56848B 逼近 64KB 管道缓冲，故表现为「时好时坏」）。改判据为**读尽输入**的 `grep -a … >/dev/null`。
_log_has() { # _log_has <文件> <模式> <尾部行数>：命中 0 / 未命中 1
  [ -f "$1" ] || return 1
  tail -n "$3" "$1" 2>/dev/null | grep -a -- "$2" >/dev/null
}

# 重启自检：影子通道必须点亮；决策 scorer 出现即立刻停机（CON-218 / INT-222）。
check_engine_log() {
  local log="$HOME/.uws/engine.log"
  [ -f "$log" ] || { echo "  ⚠ 找不到 $log，跳过自检"; return 1; }
  if _log_has "$log" "onnx scorer 就绪" 200; then
    echo "  ❌ 日志出现「onnx scorer 就绪」⇒ 决策分已进主决策链，违 CON-218/T-3，停机" >&2
    return 1
  fi
  # 影子/ONNX 行由**新进程启动后**才写；固定 sleep 1 有时早于落盘 ⇒ 误报「未生效」（OBS-232）。
  # 故有界重试至多 30×0.5s（15s），仍无才如实告警（不掩盖真降级）。
  # 15s 依据：`daemon restart` → 落「影子通道已启用」实测 ≈7s（ONNX Runtime 加载），
  # 旧 10×0.5s=5s 在 b7f2 第 9 站前**误报**一次（OBS-232 复现第 2 次，日志 23:36:22 证明影子确已启用）。
  local tries=0 shadow_ok=0
  while [ "$tries" -lt 30 ]; do
    if _log_has "$log" "影子通道已启用" 400; then shadow_ok=1; break; fi
    sleep 0.5; tries=$((tries+1))
  done
  if [ "$shadow_ok" != "1" ]; then
    echo "  ⚠ 近 400 行＋15s 重试仍未见「影子通道已启用」——影子参数可能未生效（继续，但该批影子对拍不可信）" >&2
    return 0
  fi
  echo "  ✅ 影子通道已启用 ＋ 无决策 scorer（未越 T-3）"
  return 0
}

# 孤儿 Chrome 回收：仅按唯一 profile 精确匹配（INT-221 / CON-217）。
cleanup_orphans() {
  local pids pid killed=0
  pids="$(pgrep -f -- "--user-data-dir=$PROFILE" 2>/dev/null || true)"
  [ -n "$pids" ] || { info "孤儿清理：无自建 9224 Chrome 残留"; return 0; }
  for pid in $pids; do
    local cmd; cmd="$(ps -o command= -p "$pid" 2>/dev/null || true)"
    cmdline_is_self_built "$cmd" "$PROFILE" || { info "孤儿清理：跳过 pid=$pid（非自建实例）"; continue; }
    kill -TERM "$pid" 2>/dev/null || true
    killed=$((killed+1))
  done
  sleep 1
  for pid in $pids; do
    local cmd; cmd="$(ps -o command= -p "$pid" 2>/dev/null || true)"
    cmdline_is_self_built "$cmd" "$PROFILE" || continue
    kill -KILL "$pid" 2>/dev/null || true
  done
  info "孤儿清理：已回收 $killed 个自建 Chrome 进程（宿主 9222 未触碰）"
}

close_pages() {
  local ids
  ids="$(curl -s --max-time 5 "http://127.0.0.1:$PORT/json/list" 2>/dev/null \
        | /usr/bin/python3 -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for t in d:
    if t.get("type")=="page" and t.get("id"): print(t["id"])' 2>/dev/null || true)"
  local id
  for id in $ids; do curl -s --max-time 5 "http://127.0.0.1:$PORT/json/close/$id" >/dev/null 2>&1 || true; done
}

# 重启边界的浏览器回收：先按唯一 profile 精确杀（cleanup_orphans），再拉起干净实例。
# 顺序固定：先杀后起，否则后续站点无浏览器可 attach。
recycle_chrome() {
  if [ "$KEEP_CHROME" = "0" ]; then
    # 默认：**绝不杀活浏览器**（实测杀活实例后 headless Chrome 不再存活，后续站点全 "no browser is open"）。
    # 只在 9224 已无服务时清孤儿，随后由 ensure_chrome 兜底拉起。
    if [ "$(http_code "$PORT")" != "200" ]; then cleanup_orphans; fi
    ensure_chrome
    return 0
  fi
  info "--recycle-chrome：回收并重启自建 Chrome（仅在确认浏览器可被安全重启时使用）"
  cleanup_orphans
  ensure_chrome
}

# ── 主循环 ─────────────────────────────────────────────────────────────────────

run_list() {
  [ -n "$LIST" ] || die "--list 必填（除 --selftest/--dry-run 外）"
  [ -f "$LIST" ] || die "清单不存在：$LIST"
  [ -z "$EXCLUDE" ] || [ -f "$EXCLUDE" ] || die "--exclude-domains 文件不存在：$EXCLUDE（fail-closed，不静默放行）"
  assert_self_built_port "$PORT" || die "端口守卫拒绝：$PORT（9222 为宿主专属，绝不触碰）"
  assert_self_built_profile "$PROFILE" || die "profile 守卫拒绝：$PROFILE（必须是自建临时 profile）"
  if [ "$RESTART_EVERY" -gt "$HARD_SLOT_CAP" ]; then
    die "--restart-every=$RESTART_EVERY 超过槽位硬顶 $HARD_SLOT_CAP（会静默漏采）"
  fi

  mkdir -p "$OUT_DIR"
  SUMMARY="$OUT_DIR/summary-$TAG.tsv"
  echo -e "idx\tsite\trc\tobserved\turl" > "$SUMMARY"

  if [ "$MODE" = dry ]; then
    info "[dry-run] port=$PORT profile=$PROFILE restart_every=$RESTART_EVERY tag=$TAG out=$OUT_DIR"
    [ -n "$EXCLUDE" ] && info "[dry-run] exclude-domains=$EXCLUDE"
    local n=0 run=0 skip=0
    while IFS='|' read -r name url plan; do
      [ -z "$name" ] && continue
      n=$((n+1))
      if [ -n "$EXCLUDE" ] && _excluded "$(_host_of "$url")" "$EXCLUDE"; then
        skip=$((skip+1)); info "  [$n] $name SKIP_EXCLUDED <- $url"; continue
      fi
      run=$((run+1))
      if [ $(( run % RESTART_EVERY )) -eq 1 ] && [ "$run" -gt 1 ]; then info "  --(此处硬杀重启引擎 + 孤儿清理)--"; fi
      info "  [$n] $name <- $url | plan=$plan"
    done < "$LIST"
    info "[dry-run] 共 $n 站（跳过 $skip、执行 $run），预计重启 $(( (run > 0 ? run - 1 : 0) / RESTART_EVERY )) 次；未执行任何副作用"
    return 0
  fi

  require_api_key || exit 2
  [ "${NO_SHADOW_OK:-0}" = "1" ] || require_engine_args
  ensure_chrome
  check_engine_log || die "开工前自检失败"

  local i=0 since=0
  while IFS='|' read -r name url plan; do
    [ -z "$name" ] && continue
    i=$((i+1))
    if [ "$since" -ge "$RESTART_EVERY" ]; then
      restart_engine; recycle_chrome; since=0
    fi
    _ledger_ok "$since" "$HARD_SLOT_CAP" || die "槽位账本越界（since=$since）"
    # 每站前健康检查：浏览器若已死（headless 关掉最后一个页签会自行退出）就地恢复，避免整段 silently 空跑。
    if [ "$(http_code "$PORT")" != "200" ]; then
      info "站点 $i 前发现 9224 未就绪，自动恢复自建 Chrome"
      ensure_chrome
    fi
    local log="$OUT_DIR/log-$TAG-$i-$name.txt"
    if [ -n "$EXCLUDE" ]; then
      local host; host="$(_host_of "$url")"
      if _excluded "$host" "$EXCLUDE"; then
        printf '%s\t%s\t%s\t%s\t%s\n' "$i" "$name" "SKIP_EXCLUDED" "none" "$url" >> "$SUMMARY"
        echo "[$i] $name SKIP（域已在采集中：$host，单域配额前置剔除）"
        continue
      fi
    fi
    [ -n "$PLAN_OVERRIDE" ] && plan="$PLAN_OVERRIDE"
    run_capped "$SITE_TIMEOUT" ./uws run --attach "http://127.0.0.1:$PORT" --session "wtx-bs-$TAG-$i" \
      --target-url "$url" --plan "$plan" > "$log" 2>&1
    local rc=$?
    local obs url2
    obs="$(grep -ao 'observed[^,]*' "$log" | head -1 | tr -d '\n')"
    url2="$(grep -ao '^- 当前 URL：.*' "$log" | head -1 | sed 's/^- 当前 URL：//')"
    printf '%s\t%s\t%s\t%s\t%s\n' "$i" "$name" "$rc" "${obs:-none}" "${url2:-?}" >> "$SUMMARY"
    echo "[$i] $name rc=$rc ${obs:-none}"
    [ "$CLOSE_PAGES" = "1" ] && close_pages
    since=$((since+1))
  done < "$LIST"
  info "完成：$SUMMARY"
}

# ── 参数解析 ───────────────────────────────────────────────────────────────────

while [ $# -gt 0 ]; do
  case "$1" in
    --selftest) MODE=selftest; shift ;;
    --dry-run) MODE=dry; shift ;;
    --list) LIST="${2:-}"; shift 2 ;;
    --tag) TAG="${2:-}"; shift 2 ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --profile) PROFILE="${2:-}"; shift 2 ;;
    --out-dir) OUT_DIR="${2:-}"; shift 2 ;;
    --restart-every) RESTART_EVERY="${2:-}"; shift 2 ;;
    --site-timeout) SITE_TIMEOUT="${2:-}"; shift 2 ;;
    --exclude-domains) EXCLUDE="${2:-}"; shift 2 ;;
    --close-pages) CLOSE_PAGES=1; shift ;;
    --plan-override) PLAN_OVERRIDE="${2:-}"; shift 2 ;;
    --recycle-chrome) KEEP_CHROME=1; shift ;;
    --no-shadow-ok) NO_SHADOW_OK=1; shift ;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "未知参数：$1（--help 看用法）" ;;
  esac
done

case "$MODE" in
  selftest) selftest ;;
  dry|run) run_list ;;
esac
