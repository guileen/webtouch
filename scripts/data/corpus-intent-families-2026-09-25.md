# 扩批语料意图族配额口径（`T-23` M1 · 2026-09-25）

> **定位**：`scripts/data/corpus-sites.txt` 的**分配口径声明**（`CON-219` 证据，可复核）。
> 任务与验收 → [`../../Tasks.md`](../../Tasks.md) `T-23`；白皮书 §2.1/§2.2 → [`../../docs/stage3-adaptive-interaction-engine-plan-2026-09-25.md`](../../docs/stage3-adaptive-interaction-engine-plan-2026-09-25.md)。

## 一、意图族 → 计划字面量

| 意图族 | 计划（第三列） | 语义目标 |
| --- | --- | --- |
| `input:search` | `input:search=webtouch` | 站内检索框 |
| `click:nav_doc` | `click:Documentation` | 文档/知识导航入口 |
| `click:login_entry` | `click:Sign in` | 鉴权入口（**只点入口，不填凭据、不触金库**） |
| `click:signup_entry` | `click:Sign up` | 注册/订阅入口 |
| `input:form_field` | `input:email=webtouch` | 表单邮箱字段 |
| `click:submit` | `click:Submit` | 动作提交确认按钮 |

## 二、配额（实测口径）

- 独立二极域 **518**（单域名样本 **1**，满足 `≤2`）；族分布见下表，每族 **≥20 域**。
- 阶段判据：样本 **≥500** 且独立二极域 **≥450** 且单域名 **≤2** 且 `CON-214` 特征对称 ⇒ 才可写「M1 达标」。
- **依据**：本笔实测族产出差（`OBS-228`）——`input:search` 0.375 条/站 vs `click:Documentation` 2.0 条/站 ⇒ 族分配决定总产出，故占比按「站点类型是否有该族」而非平均分配。

## 三、分配表（`corpus-sites.txt` 行号区间，1-based 含端点）

| 行号区间 | 域数 | 意图族 | 归类依据 |
| --- | --- | --- | --- |
| 1–242 | 242 | `click:nav_doc` | 语言/框架/库/OS/桌面/编辑器/shell/CI/IaC/云/虚拟化（文档导航密集） |
| 243–256 | 14 | `click:submit` | 包管理器与注册表（表单/提交密集） |
| 257–260 | 4 | `click:nav_doc` | 文档工具（docsify/mkdocs/sphinx/gitbook） |
| 261–281 | 21 | `input:form_field` | 大学与 MOOC（联系/订阅邮箱字段） |
| 282–289 | 8 | `click:submit` | 刷题/教程站（表单与确认按钮） |
| 290–301 | 12 | `click:nav_doc` | ML 库与 AI 公司（文档/API 导航） |
| 302–317 | 16 | `input:search` | 词典/维基/古籍/开放图书馆 |
| 318–333 | 16 | `input:form_field` | 政府机构与国际组织（联系/订阅表单） |
| 334–352 | 19 | `input:search` | 学术期刊与标准组织（论文检索） |
| 353–391 | 39 | `click:signup_entry` | 消费/工业品牌（账户与订阅注册） |
| 392–444 | 53 | `click:login_entry` | 金融/银行/券商/加密/航空/酒店/出行 |
| 445–452 | 8 | `input:search` | 国家门户 |
| 453–470 | 18 | `click:signup_entry` | 新闻与媒体（订阅/Paywall 注册） |
| 471–489 | 19 | `input:form_field` | 博物馆与体育组织（订阅/联系表单） |
| 490–502 | 13 | `input:search` | 电商与集市（商品检索） |
| 503–518 | 16 | `input:search` | 天气/时间/换算/搜索引擎 |

族汇总：`click:nav_doc` 258 · `input:search` 72 · `click:signup_entry` 57 · `input:form_field` 56 · `click:login_entry` 53 · `click:submit` 22。

## 四、边界与安全

- 站点清单**不含**登录后页面：计划全部停在「入口/检索/表单/提交」层，**无凭据填充、无金库解引用**（`CON-212` 不变）。
- 遇人机验证/风控 ⇒ fail-closed 隔离（真风险停止线），**不做任何绕过**；每域**单次**尝试，不在同一真实账号页反复重试。
- 剩余样本余量：单域名 `≤2` ⇒ 可再跑一遍（换族）补足 500；**未达标前只写真实计数**。
