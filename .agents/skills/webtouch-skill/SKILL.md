---
name: webtouch-skill
description: 当任务需要驱动真实浏览器时使用：网页自动化或网页操作（发微博/发帖、购物下单流程、表单填写、登录或调试网页问题、页面数据抓取、UI 验证），网页视觉对比/体验评审，通过网页大模型聊天，或让本地 Agent 与 WebTouch 引擎/网关交互。不涉及真实网页/浏览器的任务不要使用。
---

# WebTouch 使用技能（webtouch-skill）

WebTouch = 语义优先驱动真实 Chrome：已知 DSL 路径优先确定性执行，目标模式用 VLM 处理
长尾页面决策。登录态由持久调试 Chrome（profile `~/.uws-chrome-debug` / 端口 9222）保留。
**本 skill 只讲怎么用**；安装/发布/升级（`make install`、`make engine-restart`）是项目规范，见
`world-agent/webtouch/AGENTS.md`，不由执行任务的 Agent 做。

## 先窄读当前安装版能力

CLI help 描述的是已安装二进制，仓库里的 `cmd/uws/agent-guide.md` 可能更新得更早；版本/能力
仍不确定时，按本次准备使用的入口抽取相关段落，不要把整份帮助倾倒进上下文：

```bash
webtouch help | rg -n -C 2 -- '运行模式硬规则|run --plan|--observe|--screenshot|--session'
```

这条 agent-control 查询只回传相关运行规则和 run 参数。使用其它子命令时，把匹配词换成目标子命令；
匹配结果不足或无结果，再读完整 `webtouch help` 的相关章节。首次任务或版本变化时核实一次即可；
同一任务、已确认版本没有变化时沿用已核实能力，避免每一步重复输出帮助。Help 中没有必需参数时，
不要假定仓库源码里的新参数已安装；沿用可用入口，缺少必要能力就**停下上报版本差异**。
Agent 的工作流规则以本 Skill 为准，CLI help 不替代它。缺入口/版本漂移/probe 缺组件 → 停下，
不热改、不自救。

## 选执行模式

### 快速路径：目标模式或已知 DSL

目标清楚、页面路径常见、动作可逆且无需逐步核验时，用 `webtouch "目标"`（等价
`webtouch chat "…"` / `webtouch -p "…"`；不传子命令 = chat）。自动分流：

- 文本 DSL 语法校验通过 → 直接确定性执行，不烧 VLM；否则 VLM 每轮“截图 + AXTree →
  自决下一步”（click/input/done/impossible/login），引擎按 node_id 执行。
- 登录/验证/账号风险页 → **停下等用户**；付费/扣费护栏 PAUSED「需要确认」，无绕过参数。
- 页面已直接回答目标就总结，不重复导航；同一节点重复且页面没变化时先诊断，不能空转。
- 结束自动给状态、当前 URL、页面 AXTree 摘要与截图路径。

熟悉且稳定的站点路径也可直接用 `webtouch run --plan "<DSL>"`，整段计划一次执行。

### Codex 接管路径：逐步观察、决策、核验

多步任务、页面语义有歧义、此前失败/重复，或必须证明具体页面结果时，由调用本 skill
的 Agent（如 Codex）负责选择下一步；WebTouch 执行该步并回传页面证据。使用同一个专属
`--session <任务名>`，每次只发一条 DSL：

```bash
webtouch run --attach debug --session codex-task-20260923 --screenshot --plan 'nav:https://example.com/'
webtouch run --attach debug --session codex-task-20260923 --observe --screenshot
webtouch run --attach debug --session codex-task-20260923 --screenshot --plan 'click:node:12'  # 12 仅示例，须替换成上一回执中的 ref
```

记录第一次回执的 Target ID；每步续接都比对 Target ID 与 URL，session 名相同不代表
Target 一定相同。任一意外变化都暂停，不发送下一条动作，先检查报告说明。`--screenshot` 会把
当前 Target 截图附在成功回执中（本地文件），便于 Agent 在需要时自行查看；检查目标节点是否与任务直接相关，
再决定下一步。AX 证据足够时直接据此判断；同名/泛化标签、视觉状态不明或 AX 与目标冲突时，
用可用的图像查看工具打开该步截图再判断，不要求用户代看。**stdout 中出现截图路径不代表模型已看见像素；
需要视觉判断时必须实际打开图片，无法打开则说明只核对了 AX、没有视觉核验。**点击后用新页面证据核对预期结果；必要时检查该步报告的截图。`SUCCEEDED`
只表示执行调用成功，不证明页面已到达目标。候选只显示「查看 / 设置 / 详情」等泛化文字、
上下文不足或目标节点缺失时先停，不猜节点。首次建立新任务可用 `--target-url`；续用已有
专属会话时只传 `--session`。持久绑定的 Target 已失效时，引擎应报错并停下；不要换 session
名重试，也不要把其他标签当作续页。

`--observe --session <ID>` 是**只读续接**入口：要求显式 session，不传动作计划/目标 URL，只返回该会话
当前 URL 与 AX 树，可用 `--screenshot` 附本地截图。它只使用已运行的 CDP 端点，不会为找 session 启动或重启
Chrome；缺失/失效的会话直接失败，也不会请求 VLM 诊断。适用于只需查看现有页面、检查动作后状态或读取文本；
不发送页面动作。回执显示 `about:blank` 时只能说明没有可观察网页，不能推断旧页面已经接管。

**Codex 单次工具回合看图**：截图路径本身不是图像输入。CLI 的成功回执、失败回执和 probe
使用不同的路径标签；这个通用转接只运行你指定的那一条 WebTouch 命令，打印完整 stdout，识别三种
截图行，并用 `tools.view_image` 读图、以 `image()` 返回。读取失败时在同一回执里明确报告：

```javascript
// 先由 Agent 决定这一步是否允许调用 WebTouch，再把 cmd 替换为唯一一条获准命令。
const r = await tools.exec_command({cmd: "webtouch run --observe --session codex-task --screenshot"});
text(r.output);
const m = r.output.match(/页面截图：\x60([^\x60]+)\x60|失败证据截图：([^\r\n]+)|Screenshot:\s*([^\r\n]+)/);
const screenshotPath = m && (m[1] || m[2] || m[3]).trim();
if (!screenshotPath) {
  text("本条回执没有截图路径；只使用文本/AX 证据，不声称做了视觉核验。");
} else {
  try {
    const shot = await tools.view_image({path: screenshotPath, detail: "high"});
    image(shot.image_url, "high");
  } catch (err) {
    text(`截图读取失败（${screenshotPath}）：${String(err)}`);
  }
}
```

把示例 session 换成当前任务已建立的专属 session；先遵守熔断停止线，且不要用 probe 绕过原
session 绑定。`run`/`observe`/`probe` 由 Agent 按任务各自选择；该 helper 不解析报告后自动发第二条
浏览器命令。这样同一个模型可见的工具结果里同时有完整文本回执和真实像素。若 CLI 没回截图路径或查看
工具失败，明确记录缺的是视觉证据，不可声称已看图。

**AX-first 的实际使用经验**：先读回执里已有的 AX 树，再决定是否需要 `extract`。如果所需事实已在
AX 中清楚呈现，直接基于该页面证据回答或选择动作，不再发语义猜测式 `extract`。公开页 dogfood 中，
`extract:页面标题` 在 example.com 与 IANA 页都记录 `no_match`，尽管 AX 已包含可见标题和链接；基于 AX 中
实际出现的 href 导航才是有依据的下一步。遇到 `no_match`，先确认目标文本/节点是否真实存在，最多做
一次基于新证据的改写；不可原样重复，也不可用任务级 `SUCCEEDED` 覆盖逐步错误记录。

AX 树是页面语义证据，不保证节点当前在视口内或肉眼可见。可以把 AX 明确报告的文字作为页面读取结果，
但不要声称它已在屏幕上显示；当动作取决于可见性、遮挡或当前布局，或用户要求“屏幕上看得到”的内容时，
打开该步截图确认。VLM 的视觉抽取也必须以当前截图为准，不能只从 AX 辅助摘录复制文字。

这条路径让 Agent 接管跨轮规划和目标相关性判断，保留 WebTouch 的目标页隔离、确定性动作、
执行护栏和页面回执。不要为接管决策改用裸坐标。简单、稳定、可逆的目标仍用快速路径，
避免每一步都增加 Agent 往返。

### 观察与视觉评审

- 义眼：`webtouch chat "…" --img/--url/--page/--diff …` 只返回结构化判断，不执行页面动作。
  **不传视觉参数 = 目标模式**。
- 页面观察：`webtouch probe [--url …] [--keep-open]` 输出 AXTree + 截图。probe 不提供
  `--session` 绑定；不能假定它保留的标签页可被后续 `run --session` 续接。优先用同一
  `run` 会话每一步的页面回执观察当前执行上下文。
- 普通任务禁用 Python/gRPC 脚本。高层缺能力 → 停下上报缺口，禁止 import gRPC 自救。

## 执行纪律（Agent 硬性）

- 失败/暂停：**先读报告**（原因/URL/AXTree/截图/VLM 诊断），不裸重试、不猜页面文字；
  仍不懂可用 `webtouch chat "看看这一步为什么失败/页面现状" --img <截图> --plain` 诊断。
- **Anti-thrashing 熔断**：它只锁触发熔断的真实账号页；在同一真实账号页不重试、不换 session 绕过、
  不刷新或继续业务动作。保留错误、Target 和 URL 证据。可继续只读观察，也可在独立公开测试页验证；
  可因代码改动构建或重启服务，但不得借此清除/掩盖账号页风险状态。发现登录、验证码或人机挑战后，
  该站点停止自动操作，待用户在持久 Profile 人工处理并显式清除本地锁后，才进行下一次只读确认。
- 重要发现必须停下反馈：付费/扣费、账号风险/安全提示、失败卡点——不许静默跳过继续。
- 输出直接消费 stdout；文件只在需要持久化时使用。
- 登录等待：让用户操作，Agent 不点业务按钮；用户完成后再确认页面状态/继续。
- 环境异常（引擎漂移、probe 缺组件、入口缺失）→ 上报，修复留给维护会话。

## 已知坑速查

- **新窗口/新标签（`NEW_WINDOW_OPENED`）**：协调器只会在引擎明确回报
  `active_target_id == new_target_id` 时跟随；接管失败或未确认时返回 `PAUSED`。看到暂停
  就停下，读报告并说明原目标尚未接管；不要操作新开的空白页，也不要把动作当成功。
- **共享调试浏览器**：每个任务使用专属 `--session`；不要把别人的标签页当成自己的上下文。
- `NOT_FOUND`= 页面结构大改 → 重新规划；`ABORTED` = 脱 DOM/遮挡 → 先读证据；
  `NAVIGATING` 时不要立刻下一步；`UNAUTHENTICATED` = 需登录/验证。
- 多标签 = 多 context；勿跨任务共享 Cookie。持久 Chrome 首次要手动登录一次。
- VLM Key 读 `UWS_VLM_API_KEY || DEEPSEEK_API_KEY`（不进日志/文档）。
- sim 引擎只回模拟结果，真实调试必须 attach 真 Chrome。

## 深读（按任务只读相关）

- `webtouch help`（= `cmd/uws/agent-guide.md`，与 CLI 同源）：完整子命令/参数/错误速查。
- `docs/agent-interaction-contract-2026-09-23.md`：Agent 入口边界、模式选择、证据与停止条件。
- `references/visual-replication.md`：像素级复刻/对比任务的区域扫描必读。
- `references/state-export.md`：ExportState 凭证延续（v1.5.6）契约与三坑。
- `coordinator/MEMORY.md`、`pkg/vlm/MEMORY.md`：实现边界与经验（维护会话用）。
