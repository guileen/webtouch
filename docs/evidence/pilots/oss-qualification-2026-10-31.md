# 开源合格化证据（2026-10-31）

状态：草案；尚未完成全部验收，不宣称开源合格。记录时间：2026-10-31 04:30 PDT。

候选源码 main `dfdb9147cc01498a197451df53ba60d6d07d1179`（PR #6 squash merge）；PR #4 当时仍 open，未纳入候选。本文档自身提交不包含在该 SHA 中。

## 已取得的 CI 证据

[候选 main CI](https://github.com/guileen/webtouch/actions/runs/41196967655) 的 push run 两平台通过：ubuntu-24.04（linux/amd64，Go 1.25.0，Python 3.11.17）和 macos-14（darwin/arm64，Go 1.25.0，Python 3.11.9）。两者执行 `make -k ci`：分层审计、Proto 一致性、全包 Go 测试、Python 基础测试、构建和版本自检通过；Python 均为 169 passed / 11 skipped。生成物及锁文件漂移检查通过。步骤、日志及时间以 run 为准。

[PR #6 CI](https://github.com/guileen/webtouch/actions/runs/41196781554) 同样双平台通过；其测试的是 GitHub 的 PR 合并提交，不能替代上述 main run。workflow 没有恢复 Go 或 uv 缓存；依赖及工具通过 workflow 固定版本安装。两个消费方审计因兄弟仓未检出而跳过，不代表跨仓验收。

## 待完成

- 完整记录干净环境的工具版本、每步退出码、Go/Python 测试与跳过原因，以及环境和状态隔离；核对上述 CI 是否满足全部冷环境验收要求。
- 审核第三方资产授权、凭据与个人路径、私密漏洞报告端到端入口。
- 实际浏览器、桌面壳、WASM/ONNX 运行时不属于上述 CI 的验证范围；说明支持范围。T-OSS 在证据闭环前保持 IN_PROGRESS。
