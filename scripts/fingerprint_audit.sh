#!/usr/bin/env bash
# 指纹外站验收一键入口（2026-09-13）。
#
# 为什么要这个脚本：验收用例本身是 Go 测试（要能复用到包内 CDP 能力），但跑它需要记住
# 四五个环境变量。这里把常用组合固定下来，人只填「有头/无头 + 跑哪些站点」。
#
#   scripts/fingerprint_audit.sh                 # 无头全量（默认；不碰桌面）
#   scripts/fingerprint_audit.sh --headed        # 有头（会真的开一扇 Chrome 窗口）
#   scripts/fingerprint_audit.sh --sites egress,sannysoft
#   scripts/fingerprint_audit.sh --profile-id fp_mac_01
#   scripts/fingerprint_audit.sh --chrome-args --enable-unsafe-swiftshader
#
# 证据落 docs/evidence/fingerprint-audit/<日期>-<模式>/（audit.json + audit.md + 截图）。
set -euo pipefail
cd "$(dirname "$0")/.."

sites=""; profile_id=""; profile_json=""; chrome_args=""; headed=0; out=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--headed) headed=1; shift ;;
	--sites) sites="$2"; shift 2 ;;
	--profile-id) profile_id="$2"; shift 2 ;;
	--profile-json) profile_json="$2"; shift 2 ;;
	--chrome-args) chrome_args="$2"; shift 2 ;;
	--out) out="$2"; shift 2 ;;
	-h|--help) sed -n '2,12p' "$0"; exit 0 ;;
	*) echo "未知参数: $1（-h 看用法）" >&2; exit 2 ;;
	esac
done

env_prefix=(env UWS_RUN_EXTERNAL_AUDIT=1)
[[ $headed == 1 ]] && env_prefix+=(UWS_AUDIT_HEADED=1)
[[ -n "$sites" ]] && env_prefix+=(UWS_AUDIT_SITES="$sites")
[[ -n "$profile_id" ]] && env_prefix+=(UWS_AUDIT_PROFILE_ID="$profile_id")
[[ -n "$profile_json" ]] && env_prefix+=(UWS_AUDIT_PROFILE_JSON="$profile_json")
[[ -n "$chrome_args" ]] && env_prefix+=(UWS_AUDIT_CHROME_ARGS="$chrome_args")
[[ -n "$out" ]] && env_prefix+=(UWS_AUDIT_OUT="$out")

mode=$([[ $headed == 1 ]] && echo headed || echo headless)
echo "==> 指纹外站验收（$mode）站点=${sites:-默认全量}"
"${env_prefix[@]}" go test ./pkg/browser/ -run TestExternalFingerprintAudit -v -count=1 -timeout 12m
