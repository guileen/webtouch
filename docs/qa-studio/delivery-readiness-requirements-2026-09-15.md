# WebTouch QA Studio 首单交付准备 Requirement Registry

> 审计时间：2026-09-15（Asia/Shanghai）
> 适用范围：从首 1–3 单受控礼宾交付，到公开获客、重复付费 Beta 的全生命周期准备度检查面
> 上位事实源：[商业与产品运营执行方案 v8](commercial-validation-execution-plan-2026-09-14.md)；本文只展开“逐项怎么验”，不建立第二份路线图或价格事实源。

> 方法定位：本文是 `multi-lane-execution-planning` 1.1.0 的 QA Studio dogfooding 证据。它演示如何从可验收终态反推 Requirement、跨越设备/系统/人员/组织/法务边界并把未满足条件闭合为动作；下列具体条目不作为其他项目的固定模板。

## 0. 使用规则

状态只允许使用：

- `PASS`：已看到可复核证据；
- `PARTIAL`：一部分存在，但仍不足以放行；
- `FAIL`：已观察到明确不满足；
- `EXTERNAL`：依赖用户、支付商、银行、会计、律师或主管机关给出证据；
- `CONDITIONAL`：只在选定地区、部署方式或客户类型后适用；
- `HOLD`：当前证据不授权投入。

放行门禁：

- `DRY`：允许做一次内部模拟订单；
- `PAID-CONCIERGE`：允许通过定向接洽、人工 Intake 和人工核验 URL 收取首 1–3 笔真实款项；只由 §1.1 专用清单阻断，不依赖 `PUBLIC`；
- `PUBLIC`：允许公开案例并接收陌生人的 URL；
- `PAID`：允许把公开/重复付费路径做成更可重复的运营系统；不是首笔礼宾收款门禁；
- `BETA`：允许扩大到重复付费 Beta。

勾选不是证据。每项只有在“动作”完成且“验收证据”落到指定位置后才能改成 `PASS`。任何 `FAIL` 或 `EXTERNAL` 项只要标记为对应门禁的阻断项，该门禁就保持 `HOLD`。

## 1. 当前审计结论

- **DRY：HOLD。** QA Runner 有真实能力，但 Studio 默认启动没有接入 Runner；无 Runner 时接口会生成 98 分 `PASS` 示例结果。当前引擎进程存在但端口未监听，CLI 与引擎构建版本不一致。
- **PAID-CONCIERGE：HOLD。** §1.1 的真实交付、最小运行安全、卖方/收款、条款/退款、人工 SOP 和逐单证据仍未闭合；但它不再等待公开案例、公开 URL intake、自动 Webhook、多租户或规模化归因。
- **PUBLIC：HOLD。** 尚无 URL 安全门禁、客户授权、数据保留/删除、隐私说明、公开案例脱敏与分享访问控制的完整证据。
- **PAID：HOLD。** 面向公开/重复付费的自动化支付状态、对账、争议材料和运营闭环仍未完成。
- **BETA：HOLD。** 尚无独立陌生客户付款、完整交付、复验、复购与单位经济数据。

本机已观察状态：MacBook Pro（Intel，8 核、32 GB），macOS 13.5；数据卷约 932 GiB、仅余约 21 GiB（98% 已用）；Chrome 152、Go 1.26.6、Node 22.16.0、npm 10.9.2、Python 3.14.7、Git 2.39.2；Gatekeeper 开启；FileVault 关闭；系统防火墙关闭；未配置 Time Machine 目的地；`~/.uws` 权限为 `0700`。

### 1.1 `PAID-CONCIERGE` 专用阻断清单（首 1–3 单）

本表是首笔礼宾收款的唯一阻断面。下方 137 项继续保留为全生命周期检查面，但其中标为 `PUBLIC`、`PAID` 或 `BETA` 的条目不会仅因标签存在而阻断 `PAID-CONCIERGE`。某项具体实现可由可审计的等效控制替代；例如资源安全要求来自“单单运行不会耗尽宿主”，不是预设必须空出恰好 100 GiB；支付要求来自“卖方能合法收款、结算和退款”，不是预设必须使用 Paddle。

| ID | 必须成立的结果 | 当前状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| CJR-01 | 单单运行不会耗尽宿主资源 | FAIL | 实测一单的磁盘峰值、临时文件和失败重跑占用，按峰值加安全余量清理；不得以固定容量替代测量 | 资源基线、峰值记录、清理后余量和失败动作 | 用户+工程 / PAID-CONCIERGE |
| CJR-02 | 真实 Runner 可用且无假 PASS | FAIL | 对齐 CLI/Engine，接入真实 Runner；无 Runner、未执行或证据不足时失败关闭 | 公开 fixture 端到端报告；nil Runner 负例 | 工程 / PAID-CONCIERGE |
| CJR-03 | 每单输入、输出和复验范围被冻结 | PARTIAL | 填写 Acceptance Contract；必填执行材料只含授权公开 URL 与 Ship Moment，版本/最多 3 条页面路径/视口/参考图可选；明确不要求 Map、仓库或安装连接器，并冻结 48 小时内一次同范围人工复验 | 一份完整样例，Contract 与结果字段一一对应 | 产品 / PAID-CONCIERGE |
| CJR-04 | 运行事实、Judge 裁决和原始证据可复核 | FAIL | 分离 `EXECUTED/NOT_RUN` 与 `PASS/FAIL/BLOCKED/NEEDS_EVIDENCE`，保存版本、时间、证据哈希、覆盖/未覆盖、人工签发、最小 JSON 和可复制修复指令 | 结果包可独立回放，失败/阻断不冒充 PASS；无源码证据时不伪造文件名/行号 | 产品+审核 / PAID-CONCIERGE |
| CJR-05 | 目标经过授权且运行时不能进入内网或危险目标 | FAIL | 人工核验客户授权和公开 HTTPS 域；连接前、DNS 解析后和逐跳重定向时仍拒绝本机/内网/链路本地/凭据 URL | Intake 记录；协议、IP、DNS 和重定向正反例 | 法务+工程 / PAID-CONCIERGE |
| CJR-06 | 浏览器与动作被限制在单单隔离、只读范围 | FAIL | 每单独立空白 Profile 和目录；禁止登录、下载、上传、提交表单、删除、发消息和真实付款 | 两单隔离测试；危险动作全部拒绝或暂停 | 工程 / PAID-CONCIERGE |
| CJR-07 | 客户数据最小化、受保护且可按承诺删除 | FAIL | 冻结最小字段、权限、保留期、删除步骤和日志脱敏；静态保护可用 FileVault、独立加密卷或等效隔离满足 | 数据清单、权限检查、删除演练和脱敏抽样 | 数据+运营 / PAID-CONCIERGE |
| CJR-08 | 存在可验证的法定卖方和可用收款/结算路径 | EXTERNAL | 以真实所在地与业务描述核验支付商、KYC、银行、票据和税务适配；不预设支付商或必须新设公司 | 账户准入、结算方式、卖方身份及会计/法律处理口径 | 用户+财务 / PAID-CONCIERGE |
| CJR-09 | 客户付款前看见完整 offer、责任和救济 | FAIL | 订单预览写清 US$99、币种、URL-first 必填/可选输入、2 小时起点/暂停、48 小时内一次同范围人工复验、失败、退款、隐私、授权和责任限制；明确不要求 Map 或安装 Skill/MCP/Plugin | 客户接受的版本化订单/条款记录 | 产品+法务 / PAID-CONCIERGE |
| CJR-10 | 付款、退款和账务事实可人工对账 | FAIL | 首阶段允许人工确认到账、人工原路退款和人工 ledger；记录订单、手续费、退款、结算与票据 | sandbox/小额演练、退款回执和对账表一致 | 财务+运营 / PAID-CONCIERGE |
| CJR-11 | 单人能在承诺窗口内稳定履约 | FAIL | 每日最多 1 单；固定 preflight、人工复核、交付通知、故障通知、退款/改期和复验 SOP | 一次虚构订单桌面演练无临场补规则 | 创始人+运营 / PAID-CONCIERGE |
| CJR-12 | 首笔付款同时产出商业证据而非孤立收入 | FAIL | 每次报价记录买方角色、预算来源、触发时刻、当前替代方案、拒绝原因、人工分钟和全部可变成本 | 报价/订单/成本台账可按客户和 Ship Moment 对账 | 创始人 / PAID-CONCIERGE |
| CJR-13 | 平台原因的不完整交付不会被计费成功 | FAIL | 演练 Runner 故障、证据不足、超时和客户不可达；分别落 `NOT_RUN/BLOCKED/REFUNDED` 并执行通知与退款 | 四种失败演练及状态、通知、退款记录 | 产品+运营 / PAID-CONCIERGE |

## 2. 产品与交付定义

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| DEF-01 | 唯一可售 Job | PASS | 保持首单只卖一次 Ship Readiness Acceptance Job，不新增套餐 | 执行方案 §0、§6 一致 | 创始人 / PAID |
| DEF-02 | 两个 Ship Moment 使用同一 offer | PASS | 只改变 0→1 与 1→1.1 场景文案，不改价格与交付范围 | 两张场景卡逐字段 diff 仅信息变量不同 | 增长 / PUBLIC |
| DEF-03 | 输入边界冻结 | PARTIAL | 写清执行材料必填为授权公开 URL + Ship Moment；版本/提交、最多 3 条页面路径、视口和参考图可选；仓库、Source Map 与连接器安装非首单输入 | Contract Schema + 一份填写完成样例 | 产品 / DRY |
| DEF-04 | 输出边界冻结 | PARTIAL | 固定运行事实、四态 Judge 裁决、Gap、Priority、Evidence、复现、覆盖/未覆盖、可复制修复指令、最小 JSON 和人工复验入口 | Result Schema 与真实结果页字段一一对应 | 产品 / DRY |
| DEF-05 | 2 小时时钟定义 | FAIL | 定义起点为“付款成功且材料齐备”，暂停条件、恢复条件、工作时段和超时通知 | SOP 中有状态转换与三种超时演练 | 运营 / PAID |
| DEF-06 | 一次复验定义 | PARTIAL | 首轮冻结为首次交付可访问后 48 小时内人工申请；允许修复导致的目标版本变化，不新增页面、视口、动作或标准；记录窗口不足反馈 | 订单预览、Contract、SOP 三处同文 | 产品 / PAID |
| DEF-07 | 不做事项 | PARTIAL | 列出登录凭据、真实支付、破坏性动作、内网、无限页面、合规认证等默认排除项 | 订单预览客户确认记录 | 产品 / PUBLIC |
| DEF-08 | 禁止宣传用语 | PASS | 保持不说“绝对 Ready”“权威认证”“零风险”“适合所有软件” | 发布前 claims checklist 逐条通过 | 创始人 / PUBLIC |

## 3. 开发设备与本地工作环境

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| DEV-01 | 设备算力满足首单 | PASS | 保持单任务运行，不并行占满浏览器与模型任务 | 8 核/32 GB 设备记录；一次 2 小时演练无资源耗尽 | 工程 / DRY |
| DEV-02 | 系统处于受支持安全版本 | FAIL | 完整备份后把 macOS 13.5 更新到该机可用的最新安全版本；更新后重启 | `sw_vers`、Apple 安全更新页和回归记录 | 用户 / PUBLIC |
| DEV-03 | 可用磁盘余量 | FAIL | 清理可恢复的构建缓存和旧浏览器档案；目标同时满足剩余 ≥100 GiB、空闲 ≥15% | 更新前后 `df`；不删除客户证据或未确认文件 | 用户+工程 / DRY |
| DEV-04 | 全盘加密 | FAIL | 先完成可恢复备份，再由用户开启 FileVault，并保存恢复密钥到独立安全位置 | `fdesetup status` 为 On；恢复密钥存放确认但不入仓 | 用户 / PUBLIC |
| DEV-05 | 主机防火墙 | FAIL | 开启 macOS 防火墙；仅放行实际需要的本地服务；复测 WebTouch | 防火墙状态 + 本地运行 smoke | 用户+工程 / PUBLIC |
| DEV-06 | 屏幕自动锁定 | EXTERNAL | 用户设置离开后短时自动锁屏并要求密码；手工验证一次 | 设置截图或人工签收，不记录密码 | 用户 / PUBLIC |
| DEV-07 | 可恢复备份 | FAIL | 配置加密 Time Machine 或等价加密备份；执行一次备份和一次抽样恢复 | 备份时间、恢复文件哈希；不把客户原始数据同步到未经批准位置 | 用户 / PUBLIC |
| DEV-08 | 系统完整性检查 | PASS | 保持 Gatekeeper 开启，不绕过未知二进制警告 | `spctl --status` 为 assessments enabled | 用户 / PUBLIC |
| DEV-09 | 运行目录最小权限 | PASS | 保持 `~/.uws` 为 `0700`；客户任务文件默认 `0600` | 权限自动化断言 | 工程 / PUBLIC |
| DEV-10 | 工具链可复现 | PASS | 固定并记录 Go、Chrome 与 CLI/Engine 版本，不以“本机能跑”代替版本记录 | 环境 manifest | 工程 / DRY |
| DEV-11 | CLI/Engine 版本一致 | FAIL | 按项目稳定发布流程重新对齐二进制并重启；禁止任务中热改后继续跑 | `uws daemon status` 显示同一构建版本且端口监听 | 维护者 / DRY |
| DEV-12 | 电源与休眠不会中断订单 | EXTERNAL | 每单开始前接电源、关闭任务窗口内自动休眠、记录电量；结束后恢复日常设置 | 演练中无 sleep/断电，SOP 有开始/结束动作 | 执行者 / PAID |
| DEV-13 | 稳定网络与故障降级 | EXTERNAL | 记录主网络、备用热点和切换步骤；断网演练一次，不把中断判成客户失败 | 断网演练记录和客户通知模板 | 执行者 / PAID |
| DEV-14 | 时间同步与时区 | PARTIAL | 固定记录 Asia/Shanghai 与 UTC 时间戳；检查系统自动校时 | Contract、事件和报告时间可互相对账 | 工程 / DRY |
| DEV-15 | 客户数据与个人浏览分离 | FAIL | 为交付使用独立浏览器 Profile/OS 数据目录；禁止复用个人登录态 | 空白 Profile 演练、Cookie/LocalStorage 清理证据 | 工程 / PUBLIC |

## 4. 代码、构建与真实执行链

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| RUN-01 | QA Runner 能真实运行 | PARTIAL | 用固定公开 fixture 跑三视口检查并保存输入、输出和版本 | 可复跑报告与测试记录 | 工程 / DRY |
| RUN-02 | 无 Runner 必须失败关闭 | FAIL | 删除生产路径中的示例 PASS；`nil` Runner 返回明确不可用状态 | 单测证明不会返回 PASS；API smoke 返回失败 | 工程 / DRY |
| RUN-03 | Studio 接入真实 Runner | FAIL | 在 `uws studio` 构造真实 Runner 并传入服务；未连接时 UI 明示不可运行 | 端到端从 Studio 触发并产出真实报告 | 工程 / DRY |
| RUN-04 | 请求字段符合 Contract | FAIL | API 不再只收 URL；增加 contract_id、版本、环境、范围和策略版本 | Schema 校验正反例测试 | 工程 / DRY |
| RUN-05 | 运行事实与裁决分离 | FAIL | 分别实现 `EXECUTED/NOT_RUN` 与 `PASS/FAIL/BLOCKED/NEEDS_EVIDENCE` | 全状态表驱动测试，无互相冒充 | 工程 / DRY |
| RUN-06 | STALE 生命周期 | FAIL | URL、版本、环境或范围变化时使旧结论失效；保留原记录 | 变更任一关键字段后旧结果为 STALE | 工程 / DRY |
| RUN-07 | Judge 与 Builder 分离 | PARTIAL | 报告记录证据来源、规则版本和人工签名；生成/修复方不得静默改判 | 一份带 Decision/Rationale/Evidence/Reopen Condition 的结果 | 产品+审核 / DRY |
| RUN-08 | 失败不会覆盖原始证据 | PARTIAL | 所有重跑新建 attempt；禁止覆盖上次日志、截图或裁决 | 同一 Job 两次 attempt 均可回放 | 工程 / DRY |
| RUN-09 | 输出原子化 | FAIL | 先写临时目录，完整校验后再发布 result；中断只留下 incomplete | 强制中断测试不产生“完整报告” | 工程 / DRY |
| RUN-10 | 资源边界 | PARTIAL | 固定最大页面数、视口数、页面超时、总时长、截图尺寸、下载上限 | 超限正反例与明确 BLOCKED 原因 | 工程 / PUBLIC |
| RUN-11 | 依赖和构建可追溯 | PARTIAL | 每份报告记录 Git revision、二进制版本、Chrome 版本和策略版本 | 结果 manifest 可回指构建 | 工程 / DRY |
| RUN-12 | 回归测试 | PARTIAL | 为 Contract、四态、fail-closed、STALE、URL policy、退款事件补测试 | 指定测试集全绿，测试输出归档 | 工程 / DRY |
| RUN-13 | 发布与回滚 | PARTIAL | 固定安装、重启、健康检查、回滚和版本核对步骤 | 一次发布演练 + 一次回滚演练 | 维护者 / PUBLIC |
| RUN-14 | Kill switch | FAIL | 提供停止新订单、暂停运行、撤下公开提交入口的单一动作 | 演练后 5 分钟内阻止新任务进入 | 运营+工程 / PUBLIC |

## 5. URL、浏览器与动作安全

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| SEC-01 | 只允许 HTTP/HTTPS | FAIL | 解析后拒绝 `file:`, `ftp:`, `data:`, `javascript:` 等协议 | URL policy 单测 | 工程 / PUBLIC |
| SEC-02 | 拒绝 URL 内凭据 | FAIL | 拒绝 `user:password@host`，日志不得回显凭据 | 正反例测试与脱敏日志 | 工程 / PUBLIC |
| SEC-03 | 阻断本机、内网和链路本地地址 | FAIL | 解析 IPv4/IPv6，拒绝 loopback、private、link-local、metadata 和保留地址 | SSRF 测试集覆盖 IPv4/IPv6/混淆写法 | 工程 / PUBLIC |
| SEC-04 | DNS 解析后复核 | FAIL | 每次连接前校验所有解析 IP；解析结果变成内网立即停止 | DNS rebinding 测试 | 工程 / PUBLIC |
| SEC-05 | 重定向逐跳复核 | FAIL | 默认限制重定向次数，每跳重新做协议、域名和 IP 校验 | 外网→内网重定向被拒绝 | 工程 / PUBLIC |
| SEC-06 | 目标所有权或授权 | FAIL | 提交者勾选其有权测试目标；高风险目标要求域名验证或书面授权 | intake 授权记录绑定 contract_id | 法务+产品 / PUBLIC |
| SEC-07 | 默认只读动作 | PARTIAL | 首单只允许导航、读取、截图；表单提交、上传、删除、购买和发消息默认禁用 | 动作 allowlist 与拒绝测试 | 工程 / PUBLIC |
| SEC-08 | 真实付款动作绝不自动执行 | PASS | 保持现有付费 CTA 暂停门禁；交付任务不点击客户站真实支付确认 | 付费页面回归显示 PAUSED | 工程 / PUBLIC |
| SEC-09 | 登录和私有页面默认不接 | PARTIAL | 首单公共 URL；必须登录的订单改为 `NOT_RUN` 或另签数据与凭据协议 | 订单预览与 Runner 行为一致 | 产品 / PUBLIC |
| SEC-10 | 下载与文件系统隔离 | FAIL | 禁止自动下载或限制到任务临时目录；限制类型/大小；任务结束清理 | 恶意下载测试不越界 | 工程 / PUBLIC |
| SEC-11 | 浏览器隔离 | PARTIAL | 每单独立临时 Profile；禁用共享 Cookie；任务后销毁或按合同保留 | 两单之间存储隔离测试 | 工程 / PUBLIC |
| SEC-12 | 并发与滥用控制 | FAIL | 首批并发固定为 1；按 IP/客户限频；异常任务可封禁 | 并发、限流和拒绝事件测试 | 工程 / PUBLIC |
| SEC-13 | 网络出口可审计 | PARTIAL | 记录目标域、解析 IP、重定向和外发 Evidence Provider；不记录秘密 | 单次任务 egress manifest | 工程 / PUBLIC |
| SEC-14 | 安全事件处置 | FAIL | 定义发现敏感数据、恶意站点、凭据泄露、越权访问后的停止、隔离、通知和删除动作 | 桌面演练记录；每步有负责人和时限 | 安全+运营 / PUBLIC |

## 6. 客户数据、隐私与证据

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| DATA-01 | 数据清单 | FAIL | 列出 URL、联系人、DOM、截图、日志、IP、付款标识、人工备注及其用途 | 数据字典逐字段完成 | 数据 / PUBLIC |
| DATA-02 | 最小收集 | FAIL | 每个字段写“为何必需”；没有当前用途的字段删除或不采集；首单默认不主动发现、抓取或留存 Source Map | Contract Schema 无无主字段；网络与结果清单无未授权 Map | 数据 / PUBLIC |
| DATA-03 | 数据分类 | FAIL | 标为公开、客户机密、个人数据、凭据、支付数据、内部规则；定义每类处理方式 | 分类表与存储路径映射 | 安全 / PUBLIC |
| DATA-04 | 真实客户运行目录 | FAIL | 客户原始数据放 `~/.uws/qa-studio/jobs/<job-id>`，不进 Git；目录 `0700`、文件 `0600` | 权限测试与仓库 secret scan | 工程 / PUBLIC |
| DATA-05 | 传输加密 | PARTIAL | 公开表单、结果页、回调只走 HTTPS；禁止明文远程 API | TLS 检查与 HTTP→HTTPS 行为 | 工程 / PUBLIC |
| DATA-06 | 静态数据保护 | FAIL | 先启用 FileVault；敏感导出另加密；密钥不与数据同目录 | 加密状态和恢复演练 | 用户+工程 / PUBLIC |
| DATA-07 | 访问最小化 | FAIL | 建立谁能看 intake/raw/result/payment；首阶段默认仅创始人和指定审核者 | 权限矩阵与一次拒绝测试 | 运营 / PUBLIC |
| DATA-08 | 保留与删除 | FAIL | 为未付款提交、已付款任务、退款、公开案例分别设期限；到期自动/人工删除 | 到期删除演练、删除日志和例外理由 | 法务+数据 / PUBLIC |
| DATA-09 | 客户删除请求 | FAIL | 提供联系入口、身份核验、检索、删除、完成通知步骤 | 用虚构客户做一次端到端演练 | 运营 / PUBLIC |
| DATA-10 | 第三方处理者清单 | FAIL | 列出托管、分析、邮件、支付、模型/VLM 服务及发送字段、地区、保留期 | Subprocessor 清单和供应商链接 | 法务+数据 / PUBLIC |
| DATA-11 | 跨境数据判断 | EXTERNAL | 确认卖方、服务器、客户、模型服务所在地区；由法律顾问判断适用机制 | 书面适用性结论和待办 | 法务 / PAID |
| DATA-12 | 隐私告知 | FAIL | 在 URL 提交前展示收集内容、用途、保留、共享、权利和联系人 | 页面可见、版本号入订单事件 | 法务+产品 / PUBLIC |
| DATA-13 | 日志脱敏 | PARTIAL | 禁止记录 Cookie、Authorization、token、完整表单值和 URL 凭据；对查询串做策略化处理 | 自动测试和抽样日志复核 | 工程 / PUBLIC |
| DATA-14 | 公开案例授权 | FAIL | 默认不公开；单独取得可撤回的案例授权并二次脱敏 | consent_id、脱敏 checklist、发布人签名 | 运营 / PUBLIC |
| DATA-15 | 结果分享控制 | FAIL | 使用不可猜测链接、过期时间、撤销能力；敏感结果要求登录或一次性访问 | 未授权、过期、撤销测试 | 工程 / PUBLIC |
| DATA-16 | 证据完整性 | PARTIAL | 每个原始证据记录哈希、采集时间、工具版本和来源；修订不覆盖原件 | manifest 可验证哈希 | 工程 / DRY |

## 7. 结果质量与人工复核

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| QA-01 | Contract 与结果绑定 | FAIL | 每份结果写 contract_id/version、目标版本、环境、范围和策略版本 | Schema 与页面均显示 | 产品 / DRY |
| QA-02 | 每条结论有证据 | PARTIAL | 给每条 Gap 绑定截图/日志/DOM 锚点/复现步骤，不允许只有语言判断 | 抽查 100% 高优先级结论可复核 | 审核者 / DRY |
| QA-03 | 覆盖与未覆盖明确 | FAIL | 列出实际页面、视口、动作及未运行项 | 报告字段与执行 manifest 对账 | 审核者 / DRY |
| QA-04 | 优先级定义 | FAIL | 冻结阻断发布、高、普通、建议四级判据及例子 | 两名复核者对样例分级无原则冲突 | 产品 / DRY |
| QA-05 | 修复指令可执行 | PARTIAL | 每条指令写目标、证据支持的位置提示、预期变化、验证方法和不确定性；没有 Map/源码证据时不得伪造文件名或行号 | Builder 只靠 HTML/JSON/复制指令能修复一个 fixture 并复验 | 审核者 / DRY |
| QA-06 | 人工签发 | FAIL | 高优先级结论逐条确认；记录审核人、时间、保留/改判理由 | reviewer checklist 完整 | 审核者 / PAID |
| QA-07 | 误报/漏报记录 | FAIL | 客户争议和人工发现的漏检均入 goldset，不只保留成功案例 | dispute/false-positive/false-negative 台账 | 产品 / BETA |
| QA-08 | 结果页自我 QA | FAIL | 用同一 Contract 检查结果页移动端、链接、隐私、过期和打印/导出 | 结果页自身验收报告 | 工程 / PUBLIC |
| QA-09 | 可访问性口径 | CONDITIONAL | 若宣称可访问性检查，冻结 WCAG 2.2 范围和自动/人工边界；否则明确不构成合规认证 | Contract 和报告不夸大覆盖 | 产品 / PUBLIC |
| QA-10 | 客户验收 | FAIL | 交付后要求客户确认收到、能打开、理解前三项 Gap；记录异议 | delivery_accepted 或 dispute 事件 | 运营 / PAID |

## 8. 网站、报价、订单与支付

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| PAY-01 | 正式域名和 HTTPS | FAIL | 选择销售域名，配置 HTTPS、续期和所有权；确定是否在中国大陆托管 | 域名解析、证书和续期告警 | 用户+工程 / PUBLIC |
| PAY-02 | 页面说清产品 | FAIL | 首页明确“软件辅助 + 人工复核的交付验收”，避免被理解为认证、纯咨询或设备维修 | 陌生人复述测试通过 | 增长 / PUBLIC |
| PAY-03 | 价格与交付物可见 | FAIL | 显示 US$99、范围、交付目标、复验和排除项 | 价格页截图与 offer_version | 产品 / PUBLIC |
| PAY-04 | 条款/退款/隐私可访问 | FAIL | 页脚和结账前提供 Terms、Refund、Privacy，记录客户接受版本 | 支付商域名审核前自检 | 法务+产品 / PUBLIC |
| PAY-05 | 订单预览 | FAIL | 付款前显示 Contract 摘要、总价、币种、税费处理、交付起点、失败和退款 | 一份完整 sandbox 订单 | 产品 / PAID |
| PAY-06 | 支付商产品适配 | EXTERNAL | 向候选支付商发送准确产品描述，书面确认 QA Studio 不属于其禁止/受限的人类服务或技术支持类别 | 支付商书面回复或账户审核通过 | 创始人 / PAID |
| PAY-07 | 支付商地区适配 | EXTERNAL | 以卖方实际居住地/注册地核验支持；不得把 Stripe 默认成中国大陆可用 | 官方支持页 + 账户可开户证据 | 创始人 / PAID |
| PAY-08 | Sandbox 支付闭环 | FAIL | 创建产品/价格，完成成功、失败、取消、重复回调和退款测试 | 测试交易 ID 与事件对账 | 工程+财务 / PAID |
| PAY-09 | 生产 KYC | EXTERNAL | 提交真实身份、地址、主体、受益所有人等材料；处理补件 | 账户 production approved | 用户 / PAID |
| PAY-10 | 收款账户 | EXTERNAL | 建立与卖方名称匹配的银行/支付账户；完成小额验证 | payout method verified | 用户+财务 / PAID |
| PAY-11 | Webhook 验签与幂等 | FAIL | 验签、按 event_id 去重；金额/币种/产品不匹配不启动订单 | 重放和伪造 webhook 测试 | 工程 / PAID |
| PAY-12 | 付款后才启动计时 | FAIL | 仅在确认到账且材料齐备后生成 `READY_TO_RUN`；付款点击不算付款 | 状态机测试和事件时序 | 工程 / PAID |
| PAY-13 | 发票/收据 | EXTERNAL | 明确谁向客户开票、票据抬头和税号收集；测试下载或发送 | 样例票据经会计确认 | 财务 / PAID |
| PAY-14 | 退款执行 | FAIL | 分别演练全额退款、免费重跑、客户原因 NOT_RUN；记录时限和通知 | sandbox 退款与账务事件一致 | 财务+运营 / PAID |
| PAY-15 | 拒付/争议材料 | FAIL | 保存订单接受、Contract、交付、访问、客户确认和沟通证据 | 一份虚构争议材料包 | 运营 / PAID |
| PAY-16 | 每日对账 | FAIL | 每个工作日核对订单、付款、退款、手续费、结算和内部 ledger | 首笔前完成 sandbox 对账，首笔后每日完成 | 财务 / PAID |

## 9. 法定卖方、公司主体、合同与税务

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| LEG-01 | 法定卖方身份 | EXTERNAL | 用户明确选择个人/个体经营者/现有公司/新公司中的实际签约与收款主体 | 一页 seller identity decision，含名称、地区、责任人 | 用户 / PAID |
| LEG-02 | 是否必须新设公司 | EXTERNAL | 先用支付商准入、责任隔离、开票、银行和税务需求判断；未证明必要前不为“看起来正规”而设立 | Advance/Hold 决策及依据 | 用户+会计/律师 / PAID |
| LEG-03 | 主体注册与存续 | CONDITIONAL | 若选择公司/个体，完成登记、年报和必要许可；若选择个人，记录支付商允许且当地法律/税务可行 | 官方登记记录或专业意见 | 用户 / PAID |
| LEG-04 | 经营范围/业务分类 | EXTERNAL | 让会计或登记顾问确认软件服务、软件测试/信息技术服务和人工复核描述是否匹配 | 书面确认，不自行猜税目 | 用户+会计 / PAID |
| LEG-05 | 受益所有人材料 | CONDITIONAL | 公司路径准备政府登记文件和持股超过 25% 人员资料；只交支付商安全入口 | KYC accepted；材料不入仓 | 用户 / PAID |
| LEG-06 | 银行名称一致 | EXTERNAL | 核对卖方名称、支付账户、银行账户、合同与票据名称完全一致 | 四方名称核对表 | 财务 / PAID |
| LEG-07 | 服务条款 | FAIL | 写明服务主体、范围、客户义务、知识产权、付款、取消、退款、责任限制、争议处理和终止 | 律师或合格顾问审阅版本 | 法务 / PAID |
| LEG-08 | 客户有权授权测试 | FAIL | 条款要求客户拥有目标或取得授权；禁止测试第三方未授权系统 | 下单勾选 + audit event | 法务 / PUBLIC |
| LEG-09 | 不构成认证或保证 | FAIL | 明确结果是限定时间/版本/范围的证据，不保证无缺陷、不替代法定合规审计 | 条款与结果页同文 | 法务+产品 / PUBLIC |
| LEG-10 | 知识产权归属 | PARTIAL | 明确 WebTouch 核心、通用规则、客户输入、客户结果、修复建议和公开案例的权利 | 条款 + IP 审计通过 | 法务 / PAID |
| LEG-11 | 数据处理角色与附件 | FAIL | 判断对联系人、站点内容、终端用户数据分别是控制者还是处理者；需要时准备 DPA | 数据流图和 DPA 模板 | 法务 / PAID |
| LEG-12 | 适用地区隐私法律 | EXTERNAL | 先冻结目标销售地区；再让法律顾问判断 GDPR/PIPL/州法等是否适用，不按网站可访问性自动推断 | jurisdiction applicability memo | 法务 / PUBLIC |
| LEG-13 | 中国大陆网站备案 | CONDITIONAL | 若销售站点部署在中国大陆服务器，开站前核验 ICP 备案等要求；境外部署则记录不适用依据 | 备案号或部署/专业判断记录 | 用户 / PUBLIC |
| LEG-14 | 跨境服务税务 | EXTERNAL | 由会计确认收入性质、增值税零税率/免税/应税条件、所得税、外汇和申报材料 | 会计书面处理口径 + 首笔记账模板 | 财务 / PAID |
| LEG-15 | 销售税/VAT 责任 | EXTERNAL | 若使用 MoR，核对其承担范围和卖方对 MoR 的税务；若自营收款，另做客户地区税务判断 | 支付商条款 + 会计确认 | 财务 / PAID |
| LEG-16 | 制裁和受限地区 | CONDITIONAL | 使用支付商地区限制并在条款保留拒单权；不得绕过被拦交易 | 受限地区测试或支付商控制说明 | 法务+支付商 / PAID |
| LEG-17 | 账簿与凭证 | FAIL | 保存合同版本、订单、付款、退款、手续费、结算和票据；期限由会计确定 | 月度对账包 | 财务 / PAID |
| LEG-18 | 专业责任保险 | HOLD | 只有企业客户、合同额度或责任要求出现后询价；首单不提前购买 | 客户/合同触发后重新评审 | 创始人 / BETA |

公司主体当前结论：**“确定法定卖方”是收款硬前置，“新注册一家公司”不是自动硬前置。** Paddle 官方说明个人或 sole trader 不需要 business verification，但仍需域名与身份验证；是否适合本产品、是否能向所在地结算、如何纳税仍必须取得当期证据。Stripe 官方支持地区列表未列中国大陆，不能把中国大陆个人或公司直接使用 Stripe 当成默认方案。

## 10. 交付运营、客服与事故处理

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| OPS-01 | 单一 intake checklist | FAIL | 逐项确认客户身份、URL 授权、付款、材料、范围、敏感数据、可访问性和联系渠道 | 一份完整模拟 intake | 运营 / DRY |
| OPS-02 | Job ID 与目录创建 | FAIL | 到账后生成不可复用 job_id，创建最小权限目录并写入 contract snapshot | 目录、事件和订单使用同一 ID | 工程 / DRY |
| OPS-03 | 状态机 | FAIL | 实现 DRAFT→AWAITING_PAYMENT→READY_TO_RUN→RUNNING→REVIEW→DELIVERED→REVERIFY/REFUNDED/CLOSED；失败有独立原因 | 全转换正反例测试 | 产品+工程 / DRY |
| OPS-04 | 开始前检查 | FAIL | 执行者逐项检查电源、磁盘、引擎、网络、Contract、目标授权和输出目录，再点击开始 | 每单 preflight 记录 | 执行者 / PAID |
| OPS-05 | 人工复核清单 | FAIL | 高优先级证据、覆盖、未知项、修复建议、隐私和外部链接逐项签名 | reviewer checklist | 审核者 / PAID |
| OPS-06 | 交付动作 | FAIL | 发布受控结果页、发送通知、验证客户可打开、记录 delivered_at | 模拟客户完成访问 | 运营 / PAID |
| OPS-07 | 客户沟通模板 | FAIL | 准备材料缺失、运行受阻、延期、交付、退款、复验到期和安全事件模板 | 模拟订单逐模板走一遍 | 运营 / PAID |
| OPS-08 | 支持渠道 | FAIL | 建立使用法定卖方域名的支持邮箱；设置响应时段和无人值守提示 | 收发测试和工单记录位置 | 用户+运营 / PUBLIC |
| OPS-09 | 邮件可达性 | CONDITIONAL | 使用自有域邮件时配置 SPF、DKIM、DMARC；测试主要邮箱服务收件 | DNS 记录和投递测试 | 工程 / PUBLIC |
| OPS-10 | 退款与重跑授权 | FAIL | 规定谁能批准退款、何时自动退、重跑是否占复验；不得临场改口径 | 三种失败情景桌面演练 | 创始人+运营 / PAID |
| OPS-11 | 单人容量 | FAIL | 首批并发 1；日历只开放实际可交付时段；满额即关闭购买而非超卖 | 容量表和“售罄/候补”行为 | 创始人 / PAID |
| OPS-12 | 备用执行方案 | FAIL | 主设备、网络或 Runner 故障时明确暂停、客户通知、退款/改期；没有验证过的备用机不得冒充冗余 | 故障演练 | 运营 / PAID |
| OPS-13 | 复验闭环 | FAIL | 首次交付可访问后 48 小时内接受一次人工申请；校验仍属同一 Contract 范围，生成新 attempt，并把旧结论标记历史而非覆盖；PUBLIC 前不建设公开自动重跑链接 | 一次 fixture 修复→人工申请→复验全链路 | 执行者 / PAID |
| OPS-14 | 事故登记与复盘 | FAIL | 每次安全、质量、延迟、退款事故在 24 小时内登记原因、影响、处置和重开条件 | incident register + 周复盘 | 运营 / PUBLIC |

## 11. 获客、测量与单位经济

| ID | Requirement | 状态 | 具体动作 | 验收证据 | Owner / 阻断 |
|---|---|---|---|---|---|
| MKT-01 | 真实脱敏案例页 | FAIL | 用自有或明确授权站点完成真实报告；删除秘密、个人数据和核心规则 | 公开页 + 原始/公开版本 diff 审核 | 产品+增长 / PUBLIC |
| MKT-02 | 两张场景卡 | FAIL | 各写一个触发时刻、痛点、承诺和 CTA；价格与交付完全一致 | 字段 diff 和 comprehension test | 增长 / PUBLIC |
| MKT-03 | 外部声明审查 | FAIL | 每次发布前核对性能、准确性、客户数、认证和隐私声明都有证据 | claims register | 创始人 / PUBLIC |
| MKT-04 | 左锚事件 | FAIL | 每个渠道定义最早可观察事件，不用模糊“曝光”混算 | channel_event 原始事件 | 数据 / PUBLIC |
| MKT-05 | 完整漏斗事件 | FAIL | 记录结果页→URL→订单预览→付款点击→付款→交付→修复→复验→复购；失败也记录 | event dictionary + 测试事件 | 数据 / PUBLIC |
| MKT-06 | 归因 | FAIL | 每个入口带 channel/campaign/ship_moment；直接流量单列 | 事件可回到具体场景卡 | 数据 / PUBLIC |
| MKT-07 | 分母与样本隔离 | FAIL | 分开内部/熟人/陌生、免费/付费、0→1/1→1.1、自然/主动 | 看板能按 cohort 过滤且总数对账 | 数据 / PUBLIC |
| MKT-08 | 同意与分析最小化 | FAIL | 非必要追踪默认不加载或取得适用同意；支付/交付必要事件与营销分析分开 | cookie/analytics 行为测试 | 法务+数据 / PUBLIC |
| MKT-09 | 单位成本 | FAIL | 每单记录计算、浏览器、存储、网络、第三方 API、支付费、人工准备/审核/支持/退款分钟 | cost ledger 完整 | 财务 / PAID |
| MKT-10 | 贡献毛利 | FAIL | 以实际到账减全部可变成本，不用标价减模型费冒充毛利 | 每单 margin worksheet | 财务 / BETA |
| MKT-11 | 付费广告授权 | HOLD | 未有明确预算和可售门禁前只做零付费渠道；需要投放时单独授权金额和止损 | 预算批准和平台账单 | 创始人 / BETA |
| MKT-12 | 停止规则 | PASS | 20 个合格订单预览/付款点击仍 0 付款时，只改一个变量或停止该场景 | 同一事实源中的门禁裁决 | 创始人 / PAID |

## 12. 并行 Work 与文件所有权

同时最多三个主动 Work；先完成一次 60–90 分钟的字段冻结会，冻结 `contract_id / job_id / offer_version / seller_identity / event_id / attempt_id` 及状态枚举，然后并行：

| Work | 独占修改范围 | 本轮目标 | 不得修改 |
|---|---|---|---|
| `work-delivery-truth` | `pkg/acceptance/`、`pkg/qa/`、`pkg/webcontrol/`、`cmd/uws/studio.go`、`docs/qa-studio/contracts/` | DEV-11、RUN-02～12、QA-01～05 | 支付、法务、计划和 handoff |
| `work-offer-ops` | `docs/qa-studio/offer/`、`docs/qa-studio/operations/` | DEF-05～07、PAY-03～05、OPS-01～13 | 产品运行代码、计划和 handoff |
| `work-trust-data` | `pkg/urlpolicy/`、`docs/qa-studio/trust/`、`docs/qa-studio/measurement/` | SEC、DATA、MKT-04～10；只提供 server 集成接口 | `pkg/webcontrol/`、计划和 handoff |

协调者独占修改：本 Requirement Registry、唯一执行方案、项目 README/GLOBAL、项目 handoff 和根 handoff。执行 Work 通过测试、样例或原始证据回报；协调者验收后再更新状态，避免多人同时改唯一事实源。

待前三个 Work 的阻断项通过后才拉取：

1. `work-proof-growth`：真实案例页、两张场景卡和零付费渠道实验；
2. `work-payment-launch`：支付商生产审核、checkout、退款和正式对账；
3. Level 3/企业连接/审核员平台继续 `HOLD`。

## 13. 外部核验基线（2026-09-15）

以下链接只证明当期规则，真正开户、法律和税务适用性仍以实际主体与专业意见为准：

- [Stripe Global Availability](https://stripe.com/global)：支持地区列表包含香港、新加坡、美国等，未列中国大陆；Stripe 中国大陆站也明确产品不在中国大陆提供。
- [Paddle Account Verification](https://www.paddle.com/help/start/account-verification/what-is-account-verification)：需要域名审核和身份验证；个人/sole trader 可不做 business verification。
- [Paddle Domain Review](https://www.paddle.com/help/start/account-verification/what-is-domain-verification)：网站需 HTTPS，并清楚展示产品、价格、交付、Terms、Refund 和 Privacy；生产域名必须审核。
- [Paddle Acceptable Use Policy 说明](https://www.paddle.com/help/start/intro-to-paddle/what-am-i-not-allowed-to-sell-on-paddle)：纯人类咨询、技术支持等类别可能不适用或受限，因此 QA Studio 必须以准确描述取得书面准入，不能自行推定。
- [Paddle VAT 说明](https://www.paddle.com/help/sell/tax/how-paddle-handles-vat-on-your-behalf)：其 MoR 模式处理面向买家的销售税，但卖方自身收入、所在地申报和对 MoR 的税务仍需会计判断。
- [国家税务总局跨境税收政策指引](https://www.chinatax.gov.cn/chinatax/n810219/n810744/n1671176/n2884609/c5222174/5222174/files/%E7%A8%B3%E5%A4%96%E8%B4%B8%E7%A8%8E%E5%A4%96%E8%B5%84%E7%A8%8E%E6%94%B6%E6%94%BF%E7%AD%96%E6%8C%87%E5%BC%95%E6%B1%87%E7%BC%96-20241219153722816.pdf)：向境外单位提供、完全在境外消费的软件等服务可能适用零税率，但需要满足条件和申报要求；不得直接把“跨境软件服务”写成当然免税。
- [European Commission：企业数据保护义务](https://commission.europa.eu/law/law-topic/data-protection/information-business-and-organisations_en)：若适用 GDPR，需要明确处理目的、法律基础、安全、数据主体权利和事件通知等义务。
- [OWASP SSRF Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html)：用户输入 URL 必须防内网/本机访问并控制重定向；完整 URL 解析本身属于高风险输入。
- [W3C WCAG 2.2](https://www.w3.org/TR/WCAG22/)：只有冻结具体符合范围并满足相应成功准则时才可作 conformance 声明；自动扫描不等于完整认证。
- [Apple macOS Ventura 13.7.5 安全更新](https://support.apple.com/en-us/122375)：当前开发机 13.5 明显落后于 Apple 已发布的 Ventura 安全版本，升级前须先完成可恢复备份。

## 14. 下一次状态更新条件

只有出现以下任一新证据才更新本表，而不是因“做过讨论”更新：

1. 本机设置或运行结果发生可验证变化；
2. Schema、代码、测试、页面、SOP 或台账实体落地；
3. 用户选择法定卖方路径；
4. 支付商、银行、会计或律师返回书面结论；
5. 内部演练、真实付款、交付、退款、复验或事故产生新原始事件。

每次更新先改对应 Requirement 的状态与证据，再回写唯一执行方案的门禁、执行板和 Gantt。连续两次评审没有减少当前关键路径上的 `DRY/PAID-CONCIERGE` 阻断项，首单计划转 `HOLD`；进入公开或重复付费阶段后，再分别以 `PUBLIC/PAID/BETA` 作为无进展判据。任何阶段都不得用新增功能替代准备度。
