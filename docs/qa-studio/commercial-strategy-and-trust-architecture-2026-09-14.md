# WebTouch QA Studio 商业决策与信任架构

> 状态：**信任与连接边界定案；2026-09-16 商业决策升级：产品从单一 QA 扩展为 Improvement（质感与体验演进），确立 Local Skill + CLI + Remote API 为主形态，引入结构化 Communicate 沟通对齐协议**
> 日期：2026-09-14；2026-09-15 随产品定位 rebaseline；2026-09-16 升级 Improvement 范式与产品计划
> 作用：WebTouch 的商业定位、开放边界、客户连接模式、数据政策与核心 IP 边界事实源。
> 上位事实：WebTouch 核心项目、核心算法、核心规则与核心数据资产不开源，核心服务以云端为主。
> 非目标：本文不证明底层技术可行性，不把工程完成度当作商业成立证据，也不承诺法律或合规认证。
>
> 首个产品、Ship Moment/ICP、里程碑、推广上线、验收、定价、依赖排期和研发解锁由[`商业与产品运营执行方案 v8`](commercial-validation-execution-plan-2026-09-14.md)唯一负责。

## 0. 执行结论

### 0.1 已定的总策略

WebTouch 采用 **Open Edge, Closed Intelligence（开放边缘，封闭智能）**：

- 对开发者开放低摩擦的使用入口、接口契约、CI 连接器、示例和报告格式；
- 不开放状态理解、意图编译、路径规划、裁判逻辑、规则库、训练/评测集和跨客户学习数据；
- 核心判断与持续学习主要运行在 WebTouch 云端；
- 本地客户端保持为薄连接与协议层；核心 Judge、Evidence Protocol 编排、规则与持续学习不下放；
- 私有环境可通过客户侧 Runner 主动向外建立受限连接，不要求客户把预览站公开到互联网；
- 高敏场景可评估“客户 VPC 数据面 + WebTouch 云端智能/控制面”，但 A/B/C 均由客户证据选择，不在早期预设方案或承诺完整离线私有化、源代码交付。

### 0.2 产品与信任边界

WebTouch 不以“又一个自动化测试平台”进入市场，而以 **Vibe Coding Acceptance & Improvement Layer（Vibe Coding 交付验收与体验打磨层）** 进入：

> **Builder/Coding Agent 负责生成与修复；WebTouch 作为独立 Judge 与 Improvement Copilot，依据 Acceptance Contract、多源 Evidence 与设计工效规则，给出可复核的就绪差距、质感提升提案、结构化沟通选项与可执行修复指令。**

浏览器与视觉模型只是 Evidence Provider；源码/配置、日志、部署和支付状态也可成为证据源。Judge/Advisor 与 Builder 必须职责分离。

### 0.3 当前推进裁决

- **推进**：商业验证、信任架构、公开集成层设计、现有 Level 1/2 的真实试用；本地 Local Skill + CLI 与 Remote API 架构推进。
- **受控推进**：Level 3 只服务真实付费/强行为证据支持的一条关键链路，不扩成通用测试平台。
- **暂停扩建**：Level 4 通用 PRD 编译器与自主探索产品化，直至客户接入意愿、重复使用和付费证据通过门禁。
- **立即纠偏**：撤销“免费 = 开源核心”“企业 = 完整私有部署”“Level 4 已是杀手级”的现有口径。

### 0.4 2026-09-16 商业决策基准（Improvement + Communicate + 三位一体形态）

1. **价值跃升（从 QA 到 Improvement）**：不再局限于负向的“找茬挑刺”，而是转向正向的“质感与体验演进”——帮助 Vibe Coding 生成的粗糙原型一键升级为商业级、高质感、高转化率的成熟产品。
2. **交付形态（Local Skill + CLI + Remote API）**：以本地极速物理探针与终端交互为底座，通过 Local Skill 深度嵌入本地 Coding Agent，通过 Remote API 交付云端闭源高阶智能。
3. **交互机制（结构化 Communicate 协议）**：拒绝独断专行硬改代码，拒绝人工咨询黑洞；通过带 Trade-offs 的有限选项支，在终端/聊天流中与开发者和 Agent 快速对齐审美与功能取舍。

执行顺序、证据台账与研发解锁门槛由[`商业与产品运营执行方案 v8`](commercial-validation-execution-plan-2026-09-14.md)承接；本文只保留稳定的信任与开放边界。

## 1. 已知前提、未知项与证据等级

### 1.1 已知前提

以下前提由用户明确确认，本轮不再反复证明：

1. WebTouch 的底层技术能力不是当前主障碍；
2. AI 生成代码显著增加人工网页验收负担，这个痛点真实存在，用户本人已有直接体验；
3. WebTouch 核心代码、核心技术与核心数据资产必须得到保护；
4. 开放内容的唯一商业目的，是改善开发者体验、降低采用门槛并形成获客入口；
5. 核心服务应主要位于云端，以维持统一能力、持续迭代和数据飞轮。

### 1.2 仍未证明的商业事实

创始人亲自证明了“痛点存在”，但不能自动证明以下命题：

- 足够多目标客户以相同频率遇到痛点；
- 痛点足以触发预算，而不只是偶尔抱怨；
- 客户愿意让私有预览环境、需求材料和运行证据连接到外部服务；
- WebTouch 相比现有视觉测试、自然语言测试和人工验收有足够大的替代价值；
- 客户在首次试用后会持续把它留在 PR/CI 中；
- 云端推理、浏览器执行、存储与支持成本能够形成健康毛利。

### 1.3 证据等级

商业裁决的证据等级只由[`商业与产品运营执行方案 v8`「证据等级与决策记录」](commercial-validation-execution-plan-2026-09-14.md)维护：E0 战略假说、E1 创始人/内部经验、E2 外部市场材料、E3 自有行为、E4 自有经济事实。本文只强调稳定边界：商业 Advance 必须有 E3/E4 支持；架构文档、访谈赞同或公开市场材料不能单独支持规模化投入。

## 2. 足以导出结论的分析模式

原有 JTBD、Truth Gap、RAT、实物期权、数据飞轮和六门槛仍然有效，但不足以单独处理云端接入阻力。WebTouch 后续统一使用六层商业结论模型：

1. **现实层**：哪些事实已被用户或实测确认，哪些仍是假说；
2. **购买层**：使用者、推动者、经济买家和否决者分别是谁；预算从哪里迁移；
3. **替代层**：客户今天怎样完成任务，WebTouch 替代的是人工时间、现有测试工具还是交付争议；
4. **信任层**：什么数据、权限和网络边界需要暴露；谁承担错误与泄露后果；
5. **交付与经济层**：连接拓扑、计费单元、服务成本、留存机制与数据飞轮；
6. **裁决层**：每个未知项对应可观察证据、通过阈值和停止条件。

这个模式能够形成结论，因为它强制把每条陈述归为四类之一：

- 已定前提；
- 可逆产品选择；
- 必须外部取证的商业假说；
- 命中即停止的风险。

“继续分析”不再意味着增加观点，而是逐项把假说升级为证据，或把它证伪。

## 3. 真正的首要商业风险：信任采用，不是技术实现

客户并不是简单地“愿意或不愿意用云”。他们会分别评估下面七种暴露：

| 暴露面 | 客户担心什么 | 若不解决的商业后果 | 必须提供的控制 |
|---|---|---|---|
| 私有预览地址 | 云服务进入内网、横向访问其他服务 | 安全部门直接否决 | 客户侧主动出站、目标域名 allowlist、每任务短租约 |
| 登录态与凭据 | Cookie、Token、2FA、支付身份泄露 | 无法测试真实登录流程 | 凭据只驻留客户侧；云端只下发意图，不接收明文凭据 |
| PRD 与设计稿 | 未发布产品战略和客户信息外泄 | 产品/法务否决 | 默认不训练、租户隔离、最短留存、可删除、敏感模式只传结构化准则 |
| 截图与 DOM | 截图含用户信息，DOM 含业务数据 | 隐私与合规风险 | 本地脱敏、字段屏蔽、局部裁剪、上传前清单 |
| CI Runner 权限 | PR 中的恶意代码窃取 runner secret | 安全事故扩大 | 一次性隔离 Runner、最小 token、受信分支门禁、无持久凭据 |
| 自动交互动作 | 误提交、误发消息、误下单或污染数据 | 客户不敢启用自动探索 | 默认只读、动作能力清单、破坏性动作硬拒绝、人类批准 |
| 审计结论 | 误报阻塞发布，漏报形成虚假安全感 | 团队关闭门禁并流失 | EXECUTED/NOT_RUN 运行事实 + PASS/FAIL/BLOCKED/NEEDS_EVIDENCE 裁决、证据可回放、人工签收、条件变化后 STALE |

因此，安全不是企业版最后补上的功能，而是从免费试用到付费转化的主产品路径。

## 4. 三种待验证的连接模式

```text
模式 A：公开目标
WebTouch Cloud ──访问──> Public Preview
      └──生成证据、历史与 PR 结论

模式 B：私有目标（候选方案）
Thin Local Client ──短时主动出站加密隧道──> WebTouch Cloud
      │                                  │
      ├──浏览器、登录态、明文凭据留本地   ├──任务编排与核心判断
      └──脱敏证据按策略上传               └──报告、协作与学习

模式 C：高敏企业
Customer VPC Data Plane ──私网/受限出站──> Dedicated WebTouch Control Plane
      ├──隔离执行、客户密钥、区域留存       ├──单租户智能服务
      └──可选只上传结构化证据               └──SSO、审计、策略与 SLA
```

### 4.1 模式 A：Cloud Direct

- 面向公开网站、演示站和无敏感数据的预览环境；
- 零安装，是获客与首次价值最快的入口；
- 不支持客户真实 Cookie，也不承诺访问企业内网；
- 首个 URL-first 可售实验优先使用该模式；是否存在免费层仍是后续包装假说。

### 4.2 模式 B：Customer Runner + Cloud Brain

- 薄客户端/Runner 位于开发者电脑、CI 临时容器或客户网络；
- 只建立客户发起、逐任务短租约的出站加密连接/反向隧道，不开放长期入站端口；
- 客户端只承载连接、协议、浏览器执行与本地策略；核心 Judge 与 Evidence Protocol 编排留在云端；
- Runner 只接收签名任务，访问显式 allowlist 内的目标；
- 浏览器档案、Cookie、2FA 和测试账号留在 Runner；
- 上传内容由项目策略确定：完整截图、脱敏截图、局部截图、DOM 摘要或只上传断言结果；
- 每次运行生成“数据离机清单”，让开发者和安全人员可复核。

BrowserStack Local 已证明“客户侧二进制 + 单一出站加密隧道”是市场可接受的成熟模式；其官方文档明确强调不需要把内部服务器公开到互联网。Cloudflare Tunnel 也采用本地守护进程主动建立 outbound-only 连接的同类边界。

### 4.3 模式 C：VPC Data Plane + Dedicated Cloud Intelligence

- 只为已有付费意愿且安全阻力明确的企业提供；
- 客户控制执行网络、测试数据和密钥；
- WebTouch 保留核心智能服务，不交付源代码；
- 提供单租户、SSO/RBAC、审计日志、区域留存、DPA/SLA 等企业控制；
- 完整离线 on-prem 不是默认路线。若未来出现足够高 ACV，再作为时间受限、可撤销许可证的例外评估。

## 5. 数据最小化与数据资产政策

### 5.1 客户拥有且默认不进入训练的数据

- PRD、设计稿、源码、网页正文；
- Cookie、Token、密码、2FA 与浏览器档案；
- 原始截图、HAR、Console 原文；
- 客户专属验收规则与业务流程。

默认政策：不训练、租户隔离、加密传输与存储、可配置留存、客户可删除。若未来使用第三方模型，必须披露模型提供方、处理区域和留存政策。

### 5.2 WebTouch 可以形成护城河的数据

- 自建或获得明确授权的评测集；
- 不可逆还原的页面状态与组件特征；
- 缺陷类型、误报/漏报标签和规则有效性统计；
- 动作边成功率、失败原因和环境兼容性统计；
- 用户主动提交的匿名反馈；
- 公开网页和公开组件形成的通用模式库。

任何跨客户学习必须先完成去标识化与权利来源确认。不能把“客户上传过”解释成“WebTouch 可以训练”。

## 6. 知识产权与开放边界

### 6.1 可以公开

- GitHub Action、npm/Homebrew 安装入口与薄 CLI；
- API/事件协议、配置 Schema、报告 Schema；
- SARIF/JUnit 等生态适配器；
- 示例项目、演示数据、文档和问题反馈入口；
- 不含核心权重与策略的最小 Runner 外壳，是否开源由独立安全审计决定。

### 6.2 必须封闭

- 意图编译与需求对齐策略；
- 页面状态理解、路径规划和自愈策略；
- 视觉/语义裁判组合、阈值校准与置信度融合；
- 规则库、组件基因、失败模式库；
- 训练数据、评测集、标注和客户反馈派生资产；
- 反滥用、风控、身份与云端调度内核。

### 6.3 当前许可证冲突

项目 README 当前声称使用 Apache 2.0，但链接指向的 `LICENSE` 文件并不存在；QA Studio 白皮书同时写着“开源核心”。在完成代码来源和历史分发审计前：

- 不发布任何核心仓库或核心二进制的开源许可证声明；
- 不把 monorepo 路径写进公开安装命令；
- 未来公开组件必须位于独立仓库，逐仓声明许可；
- 公开仓库不得通过构建脚本、submodule、符号文件或测试夹具带出核心实现和数据。

许可证一旦对外授予通常难以收回，属于必须最先处理的锁定类决策。

## 7. 商业模式与包装边界

Free、Team、Enterprise 只能作为候选包装名称；不得在真实付款、留存和支持成本出现前写成既成产品线。无论未来采用哪种包装，免费不等于开放核心，企业也不默认获得源码或完整离线部署。

具体 Ship Moment/ICP、价值度量、报价实验和经济性门槛统一见[`商业与产品运营执行方案 v8`「定价模型与实验」](commercial-validation-execution-plan-2026-09-14.md)。

## 8. 增长路径边界

“人工接洽/公开渠道 → URL 核验/提交 → 订单预览 → 付款 → 交付 → 修复 → 复验 → 再次购买/持续使用”目前只是待验证路径，不是已经成立的增长闭环。公开层服务发现与集成，闭源层承载 Judge 与持续价值；真实推广波次、漏斗和分母口径统一见[`商业与产品运营执行方案 v8`「里程碑与上线门禁」「推广计划与上线波次」「多泳道排期与关键时间节点」](commercial-validation-execution-plan-2026-09-14.md)。

## 9. 对外定位与宣传口径

### 9.1 产品类别

**Vibe Coding Acceptance Layer / Last-mile Verification**
中文：**Vibe Coding 交付验收层 / 最后一公里验收**。

### 9.2 首屏主张

英文建议：

> **Know what stands between your build and Ship Ready.**
> Submit a URL. Get prioritized gaps, reviewable evidence, and fix instructions your coding agent can execute.

中文建议：

> **提交一个 URL，知道离 Ship Ready 还差什么。**
> 获得优先级、可复核证据，以及可直接交给编码 Agent 的修复指令。

### 9.3 明确不说

- 不说“开源核心”；
- 不说“绝对确定”“不可辩驳”或“权威合规认证”；
- 未经真实基准，不说所有任务都在 10 秒内完成；
- 不把局部 0-Token 探针包装成 Level 4 全链路 0-Token；
- 不承诺替代 Playwright、Cypress 或全部人工 QA；
- 不宣传自动破解验证码，只说检测挑战、停止自动动作并请求合法人工接管。

## 10. Level 4 的商业与安全重构

Level 4 只有在以下边界下才值得继续：

1. 输入优先支持 Markdown PRD，不同时扩展 PDF、Word、Figma 与白板；
2. 将“PRD 自动编译”拆成需求提取、人工确认、执行和证据裁决，不让模型一次性决定真值；
3. 真实业务流程允许循环，产品契约应为 Acceptance Graph，而非强制 DAG；
4. 输出必须区分 `EXECUTED / NOT_RUN` 与 `PASS / FAIL / BLOCKED / NEEDS_EVIDENCE`，并在约束变化后标记 `STALE`；
5. 客户可选择不上传原始 PRD，只上传人工确认后的结构化验收准则；
6. 自动探索默认只读，任何有副作用的动作必须由能力清单和人工批准约束；
7. 首个价值不是“自动理解所有 PRD”，而是“一个 Acceptance Contract 在一个真实 Ship Moment 上形成可回放证据并驱动修复/复验”。

## 11. 商业验证计划入口

不再在本文维护另一份按周计划。里程碑、停止条件、排期和研发解锁统一见[`商业与产品运营执行方案 v8`](commercial-validation-execution-plan-2026-09-14.md)；推进由证据触发，不由“第几周”自动触发。

## 12. 待验证问题入口

客户的真实连接限制、预算来源、Ship Moment、渠道容量、复验/留存和结构化需求接受度都已纳入[`商业与产品运营执行方案 v8`的假设表与思维矩阵](commercial-validation-execution-plan-2026-09-14.md)，本文不维护平行问题清单。

## 13. 当前最终结论

1. 技术可行性与痛点存在性已经足以允许商业验证，不足以允许规模化开发；
2. 最大风险从“能不能做”转为“客户是否敢接、愿不愿留下并付钱”；
3. 云端不是单一部署选择，而是智能与数据平面的分工；客户敏感数据可留在本地，核心智能仍可留在 WebTouch；
4. 开发者友好应通过公开接口、免费试用、可审计 Runner 和工作流集成实现，不以开放核心换取传播；
5. 近期唯一高确定性主线是验证渠道容量、URL 提交、实际付款、限时交付、修复/复验和逐单经济；Level 4 通用实现继续扩张属于低确定性投入。

## 14. 公开依据与审核边界

- [Stack Overflow 2025 Developer Survey](https://survey.stackoverflow.co/2025/work)：开发者技术采购影响与工具选择因素；
- [Stack Overflow 2023 Developer Survey](https://survey.stackoverflow.co/2023/)：开发者自主研究与免费试用行为；
- [BrowserStack Local 官方架构](https://www.browserstack.com/docs/local-testing/how-local-testing-works)：客户侧二进制、单一出站加密连接与内部站点访问；
- [Cloudflare Tunnel 官方架构](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/)：本地守护进程建立 outbound-only 连接；
- [GitHub Actions 安全指南](https://docs.github.com/en/actions/reference/security/secure-use)：自托管 Runner 对不可信 PR、持久环境和凭据暴露的风险；
- [OpenAI 企业隐私说明](https://openai.com/enterprise-privacy/)：不默认用企业数据训练、留存控制、加密和企业治理可成为采购信任基线。

本轮按 makeplan 协议尝试使用外部对抗审核，但安全门禁拒绝把内部商业策略和连接架构发送给外部 DeepSeek；没有绕过。因此本文完成了本地对抗审计与公开证据校准，但**不声明已经过独立外部模型审核**。

## 15. 2026-09-16 商业与产品重大升级：Improvement 范式与 Communicate 闭环

### 15.1 JTBD 升级与预算重定向（从“挑刺质检”到“体验演进”）

1. **JTBD 的根本升级**：
   - **旧定位（QA / 验收）**：用户买的是“别出丑 / 别有控制台红字和横向滚动条”。这是纯防御型、事后补救型动作，用户对单一的 Bug 报告单天然具备心理防御，测试外包预算在个人与中小团队中极度匮乏。
   - **新定位（Improvement / 体验演进）**：用户买的是**“把 AI 粗糙生成（Vibe-coded）的原型，一站式打磨升级为具备 Apple/Linear 级高质感、高转化率、零物理缺陷的 Ready-to-Ship 成熟产品”**。
2. **预算再分配（Budget Reallocation）**：
   - 转移并替代**资深 UI/UX 外包设计审查、设计系统咨询、前端资深重构与发布前打磨（Polishing）工时**。这一预算体量是单纯 QA 的 5~10 倍。
3. **真实度套利（Truth Arbitrage）**：
   - **市场真实断层**：Coding Agent（Cursor / Claude Code / v0）擅长快速生成组件，但对真实浏览器视网膜渲染、间距节奏、动态层级和物理交互手感完全缺乏常识（“盲人写代码”）；传统测试工具（Playwright / Lighthouse）只有冷冰冰的指标，毫无审美与建设性改良能力。
   - **WebTouch 独特解**：**本地真实原生浏览器探针（客观物理事实） + 云端审美与工效模型（提纯经验） + 结构化沟通对齐协议**。

### 15.2 三位一体产品形态（Local Skill + CLI + Remote API）

坚定维护 **Open Edge, Closed Intelligence**，三者职责严格划界：

1. **Local CLI (`webtouch` / `uws`) —— 物理执行器官与隐私盾牌**：
   - **本地直连**：直接驱动本地原生无干扰 Chrome 伴生壳（`NonActivatingPanel`），挂载开发者的 `http://localhost:3000` 或预览服务；
   - **零敏感数据出域**：源代码、本地登录态、内网接口完全不出开发者机器，解决模式 A 必须公开 URL 的合规死穴；
   - **物理事实提纯**：毫秒级提取多视口（390px/768px/1440px）渲染切片、控制台报错、网络瀑布、AXTree 树、WSTN 因果状态突变证据；
   - **交互式终端支持**：内置轻量交互式 TUI/问答界面，支持在终端直接选择改进方案。
2. **Local Skill (`webtouch-skill`) —— 本地 Coding Agent 的感知与交互手眼**：
   - 适配 Claude Code、Cursor、Gemini CLI、Antigravity 等主流编程 Agent；
   - 暴露标准化 Tool（`webtouch.inspect`、`webtouch.discuss_improvement`、`webtouch.apply_patch`）；
   - 使得 Agent 能在完成初版代码编写后，自动唤起 WebTouch 进行走查，并将沟通选项无缝呈现在聊天界面中，确认后由 Agent 一键改代码。
3. **Remote API (`WebTouch Cloud`) —— 闭源智能与决策大脑**：
   - **Multi-Aspect Evaluator**：结合高级视觉多模态与设计工程规则库，评估 Functional Invariants（客观缺陷）、Aesthetics（排版层级、间距节奏、暗色对比度）、Ergonomics（触控热区、空状态、移动端交互）；
   - **Dialogue & Trade-off Engine**：识别审美与业务冲突，提炼出 2~3 个最具杠杆率的改进方案，维持协商状态机；
   - **Instruction & Patch Synthesizer**：将用户确认的共识编译为针对具体技术栈（Tailwind / React / Vue / CSS）的精确代码修改指令或补丁；
   - **数据飞轮**：沉淀组件基因库与改进采纳率经验，越用越准。

### 15.3 Communicate 沟通机制与防咨询黑洞（铁律 5 约束）

主观美感与产品取舍不能由机器独断专行，也不能退化为人工设计咨询。系统设计**四步闭环协议（Structured Dialogue Protocol）**：

1. **第 1 步：诊断分流（Triage）**：
   - **P0 物理缺陷（Invariants）**：横向溢出、死链、JS 异常。直接下发确定性修复方案，无需商量；
   - **P1 质感与体验改进（Improvements）**：排版层级、移动端视口遮挡、CTA 转化指引、微动效与反馈。进入提案协商。
2. **第 2 步：方案权衡（Trade-offs Proposals）**：
   - Remote API 提炼出 2 个鲜明对比方向，附带理由与预期影响：
     - *选项 A（紧凑高转化风格）*：收紧首屏内边距，移动端吸底浮动 CTA，确保首屏完整呈现核心价值；
     - *选项 B（极简品牌留白风格）*：保留大呼吸感留白，将次要信息折叠至抽屉，增强背景对比度凸显主体。
3. **第 3 步：对齐沟通（Communicate & Align）**：
   - 在 Local Skill（IDE 聊天框）或 Local CLI（终端提示符）中，开发者或 Agent 快速选择 `[A]` / `[B]`，或输入单行约束（如：“选 A，但吸底按钮只在上滑时出现”）。
4. **第 4 步：指令编译与因果复验（Compile & Causal Verification）**：
   - Remote API 编译下发精准指令，本地 Coding Agent 写入代码；
   - Local CLI 自动热重载本地浏览器重跑测试，对比改动前后渲染与状态证据，完成验收闭环。

### 15.4 商业包装与计费假说（Freemium）

1. **Free 层（Local CLI）**：
   - 永久免费、无限次本地客观物理体检（P0 Bugs：溢出、控制台异常、死链、基础可访问性检查）。用于开发者生态无门槛普及与获客入口。
2. **Pro / Token 计费层（Remote API）**：
   - 云端高级视觉质感评审、Communicate 多轮协商点数、精细化 Tailwind/React 修复代码补丁生成。按次或按月订阅。

## 16. 落地推进路线图（Roadmap & RAT Gates）

- **里程碑 M0：通信协议与数据契约设计（1 周）**：
  - 冻结 Local CLI、Local Skill 与 Remote API 的数据模型（`InspectionPayload`、`ImprovementProposal`、`DialogueSession`、`ResolutionSpec`）。
- **里程碑 M1：Local CLI 改进模式与终端交互原型（2 周）**：
  - 扩展 CLI 子命令 `webtouch improve http://localhost:3000`；打通本地抓取、脱敏切片、Mock API 提案与终端交互选择闭环。
- **里程碑 M2：Remote API 神经符号评估与提案生成器（2 周）**：
  - 云端接入多模态审美与工效评审流水线，形成第一批开箱即用的 A/B 质感改进策略。
- **里程碑 M3：Local Skill 接入主流 Coding Agent（2 周）**：
  - 升级 `webtouch-skill`，打通 Cursor / Claude Code 的无缝协同（写代码 -> 打磨沟通 -> 自动修复 -> 复验）。
- **里程碑 M4：真实项目 Dogfooding 与 RAT 证伪门禁（2 周）**：
  - 选取 3~5 个真实的 Vibe Coding 开源或个人项目实测；
  - 验证关键指标：方案采纳率（>60%）、开发者多轮沟通意愿、前后质感显著提升证据。
- **里程碑 M5：商业化 API Key 鉴权与生态推广（2 周）**：
  - 上线云端计费系统，卡死 Vibe Coding 开发者群体的“去模板化/去廉价感”高意图入口。
