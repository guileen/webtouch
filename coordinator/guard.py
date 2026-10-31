"""执行护栏：扣费/购买等高影响动作执行前必须暂停并交由用户确认。

2026-09-03 用户要求：在浏览器上执行动作时，如果出现重要信息（例如页面将扣费），
必须暂停并提醒用户，不能默默直接下单/支付。护栏是确定性规则：

- 动作语义 / 值 / 当前 URL 命中付费、扣费、购买类词 → 执行前直接暂停；
  - 页面导航后 AXTree 出现付费 CTA（立即购买/去支付/确认订单/自动续费/扣费等）
    → 页面风险置位，后续「确认类」点击（下一步/确认/提交/继续/开通等）暂停；
- 暂停不是失败：报告标注「需要确认」，附原因 + URL + AXTree + 截图；
- 护栏无绕过参数：上层 Agent 收到暂停后必须停下询问用户，用户确认后以新计划继续。

宁可保守暂停，不可静默放行；误报只会多一次确认，漏报会直接产生扣费。
"""

from __future__ import annotations

from coordinator.models import ActionNode, TaskContext

CONFIRM_PREFIX = "confirmation_required"

# 页面级付费 CTA（出现在 AXTree/文案里才报警；避免“支持支付宝”这类普通提及误伤）。
PAGE_CTA_MARKERS = (
    "立即购买",
    "马上购买",
    "去购买",
    "点击购买",
    "购买套餐",
    "开通套餐",
    "立即支付",
    "去支付",
    "确认支付",
    "确认订单",
    "确认购买",
    "同意并支付",
    "同意并购买",
    "开通并支付",
    "提交订单",
    "下单",
    "自动续费",
    "自动扣款",
    "扣费",
    "扣款",
    "订阅并支付",
)

# 动作/URL 级强词：用户 DSL 意图里出现这些词即视为高影响动作。
ACTION_RISK_MARKERS = (
    "购买",
    "支付",
    "付款",
    "下单",
    "扣费",
    "扣款",
    "续费",
    "订阅",
    "开通",
    "付费",
    "缴费",
)

# URL 路径风险段（小写匹配）。
URL_RISK_FRAGMENTS = (
    "checkout",
    "payment",
    "/pay",
    "order/confirm",
    "order-confirm",
    "billing",
    "purchase",
    "subscribe",
    "/buy",
)

# 页面风险置位后仍可能“顺手点掉”的确认/推进类语义：这些点击最可能把用户送进扣费。
GENERIC_CONFIRM_ACTIONS = (
    "下一步",
    "确认",
    "确定",
    "提交",
    "继续",
    "同意",
    "完成",
    "开通",
    "立即使用",
)


def page_cta_marker(tree_text: str) -> str:
    """扫描 AXTree/页面文本，返回命中的付费 CTA；无命中返回空串。"""
    for marker in PAGE_CTA_MARKERS:
        if marker in (tree_text or ""):
            return marker
    return ""


def action_risk_marker(step: ActionNode) -> str:
    """动作语义/值是否命中付费/扣费强词；返回命中词，无则空串。"""
    haystack = " ".join(filter(None, (step.action_type, step.target_semantic, step.value or "")))
    for marker in ACTION_RISK_MARKERS:
        if marker in haystack:
            return marker
    return ""


def url_risk_marker(url: str) -> str:
    """当前 URL 是否落在 checkout/payment/order 等付费流程路径。"""
    low = (url or "").lower()
    for fragment in URL_RISK_FRAGMENTS:
        if fragment in low:
            return fragment
    return ""


def guard_before_execute(step: ActionNode, ctx: TaskContext) -> str:
    """执行前护栏：返回需暂停的确认理由；空串 = 放行。"""
    url = str(ctx.variables.get("initial_url", ""))
    for entry in reversed(ctx.history):
        if getattr(entry, "current_url", ""):
            url = entry.current_url
            break
    marker = action_risk_marker(step)
    if marker:
        return (
            f"{CONFIRM_PREFIX}: 动作 {step.action_type}「{step.target_semantic}」"
            f"命中付费/扣费词「{marker}」——执行前必须由用户确认，WebTouch 不自动通过"
        )
    if url_risk_marker(url) and step.action_type in ("click", "input", "select"):
        return (
            f"{CONFIRM_PREFIX}: 当前 URL 处于付费流程（{url}，命中"
            f"「{url_risk_marker(url)}」）——继续前必须由用户确认"
        )
    if (
        ctx.page_risk
        and step.action_type == "click"
        and any(word in (step.target_semantic or "") for word in GENERIC_CONFIRM_ACTIONS)
    ):
        return (
            f"{CONFIRM_PREFIX}: 当前页面出现付费提示「{ctx.page_risk}」"
            f"（URL={url}）——继续前必须由用户确认"
        )
    return ""
