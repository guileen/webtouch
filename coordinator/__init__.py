"""UWS 外部协调器 SDK：模板驱动 + LLM 兜底的 DAG 动作编排（spec/coordinator.md）。

2026-09-14 运行时全面收敛：历史应用层（api_server + providers）已彻底废除并归档至 `archive/coordinator/`，
全量 API 服务唯一收敛至 Go 网关（`cmd/uws-gateway` + `apps/gateway`）；Python 职能纯化为离线数据科学与评测实验室。
"""
