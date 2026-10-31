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

## 2026-10-31 复核

- **SECURITY**：`gh api repos/guileen/webtouch/private-vulnerability-reporting` 返回 `{"enabled":true}`（2026-10-31 11:32 UTC）；`SECURITY.md` 指向私密 advisory、新旧版本支持边界和协同披露流程。此为配置与入口核实，未提交真实漏洞报告。
- **凭据与路径**：对 PR-C 跟踪树 `git ls-files` 的文本检查 `/Users/`、`/home/`、常见 GitHub/OpenAI/AWS 密钥格式、私钥头、赋值形式的 token/password/cookie 及 vault/cookie/credentials 导出文件名，并检查敏感扩展名；人工复核命中。10 处路径命中为占位、守卫或脚本变量；6 处凭据格式命中为本地默认值、选项名、测试密钥或测试 cookie；6 处 vault 文件名命中为文档/源码引用。未发现实际密钥、cookie 或 vault 导出；保留夹具。该模式扫描不能证明不存在所有形式的秘密，也未重写历史。
- **资产清点及授权缺口**：`git ls-files` 扩展名与 `vendor`、`third_party`、许可证/版权声明搜索：未发现 vendored 目录、图片、字体、独立 WASM 或已附第三方 NOTICE；依赖通过 `go.mod`/`go.sum`、`pyproject.toml`/`uv.lock` 获取，遵循各自许可，尚未完成逐依赖许可证及分发义务审计。`proto/gen/` 是仓内 proto 的生成文件；HTML/JS 含应用及测试页面，未发现单独来源声明，不能据此证明所有片段原创。唯一跟踪的模型 `pkg/classifier/testdata/classifier.onnx`，`pkg/classifier/MEMORY.md` 记为 Python 训练链生成，但训练数据权属、模型来源及可再分发授权尚无可复核凭证。**具体许可冲突：尚未发现；授权未明：上述模型及未完成的依赖/片段来源审查，不等同于无冲突。**
- **文档旧说法**：搜索 README 与 `docs/README.md` 中无 LICENSE / evidence 不在树的当前表述，未发现；现分别链接 MIT 和本案卷。

## 候选 CI 的复核范围

候选 run 创建于 2026-10-31 11:28:04 UTC，GitHub hosted Ubuntu 24.04 amd64 / macOS 14 arm64 各在全新 job checkout，workflow 关闭 Go、uv 缓存恢复；不复制开发机工作树。工作流 `uv sync --frozen --group dev --python 3.11`、`bash scripts/ci-bootstrap.sh`、`make -k ci` 和生成物/锁文件 diff 步骤均成功（步骤退出码 0）。Go 1.25.0、uv 0.13.6；Python 分别 3.11.17 / 3.11.9；protoc 35.1、protoc-gen-go 1.36.12、protoc-gen-go-grpc 1.6.2。`make -k ci` 含 lint、分层/消费方审计、proto-check、`go test -count=1 -v ./...`、Python 和 build；两个未检出兄弟仓的消费方审计跳过。

每平台 Go 日志含 1204 条 PASS、0 条 FAIL、80 条 SKIP（含子测试行，非独立测试去重计数）；Python 169 passed、0 failed、11 skipped，另有 11 个依赖未入仓 pipeline/benchmarks 的历史测试文件在 pytest 配置中排除。Go 跳过主要涉及显式禁用的真实 Chrome、缺失 ONNX runtime、未实现/需外部环境的用例；具体理由见 CI 完整日志。真实浏览器、桌面壳、WASM/ONNX 运行时和跨仓消费方不在通过范围。日志通过 `gh run view 41196967655 --log` 提取的本地副本 SHA-256 `10fe9ce86878ec873b05b4fd6a54a86897576d084ecb2c3448b5c9fea2d287d7`；权威日志见上方 CI 链接。

**仍未勾选**：模型及其他第三方来源/许可义务闭环；候选之后 PR 与最终 main 对应 SHA 绿灯；逐步完整退出码、全部 skip 原因与隔离证据的验收记录仍需进一步整理。最终 main CI 未绿之前不得宣称源码开源合格。

## 临时克隆复跑：BLOCKED

2026-10-31 11:32:48 UTC，从 GitHub 新 clone 并 detached checkout `dfdb9147cc01498a197451df53ba60d6d07d1179`；起始无 `.venv`、`bin`、`build`，Go 编译/模块和 uv 缓存均指向新建 tmp 目录，`WEBTOUCH_STATE_DIR` 指向新建状态目录，取消 Chrome E2E 等变量。此为开发机预演，继承宿主环境及 HOME，**不等同于一次性干净 runner**。macOS Darwin 27 arm64；Go 1.27.3，uv 0.13.6，Python 3.11.18，protoc 35.1，插件 1.36.12 / 1.6.2。`uv sync --frozen --group dev --python 3.11` 退出 0；`bash scripts/ci-bootstrap.sh` 退出 0；`make ci` 退出 **2**，在全包 Go 阶段停止，故此轮 Python 和 build 未执行；生成物/锁文件 diff 退出 0。

失败为 `cmd/uws` 的 `TestEngineArgsLearnedDefaultAndDisable` 和 `TestDaemonStateDirEnvOverride`：测试期待其临时 `UWS_ENGINE_DIR` 生效，但本轮按隔离方案设置的 `WEBTOUCH_STATE_DIR` 优先。候选 CI 没有设置该变量，不能覆盖该情形。修复测试隔离或约定后必须重新固定候选并复验，不可记 PASS。完整本机日志 `/tmp/webtouch-oss-verify.YIMWEi/run.log`，SHA-256 `5408b25e140b42c45ded5e01e3df2c3448b5c9fea2d287d7`；临时路径可能被清理，长期证据需存储到可访问的 CI artifact。
