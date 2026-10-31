# pkg/browser MEMORY（M3：CDP 会话 / Target 池）

## 已点亮（2026-08-19，Go 逻辑层）
- TDD-ENG-01（代理/时区注入 + UA/时区探活一致）、02（5 并发 Context Cookie 物理隔离）、
  09（预置 Cookie 注入）、10（池配额 + FIFO + RESOURCE_EXHAUSTED）、11（500 次/4h 优雅重启）。
- 2026-08-20（续8）：真实 CDP 会话（`cdp_session.go` + `wsconn.go` + `cdp.go`）落地——
  `TestCDPSessionRealChrome`（导航/探活/候选/点击/输入/SPA pushState）、
  `TestCDPSessionOcclusion`（遮挡）、`TestCDPSessionAttachMode`（附着）全绿；
  TDD-ENG-08 E2E 点亮（`tests/e2e/test_engine_attach.py`，`UWS_E2E_CDP=1` 门控）。
- TDD-ENG-03/04/05 能力已具备（贝塞尔轨迹/遮挡/挑战拦截），CDP 级 E2E 断言与
  ENG-12/13 仍待真实环境。
- 2026-08-20（续12）：真实 CDP P0 加固——视口外元素自动滚动定位（scrollIntoView 后重测）、
  导航失败显式化（loadFailed + chrome-error frameNavigated 双信号）、在途 XHR/Fetch
  30s TTL 剪枝、页面级 HTTPStatus 接入候选（≥400 整页物理湮灭）；
  `TestCDPSessionScrollIntoView` / `TestCDPSessionNavFailure` / `TestFreshInflightTTL` 全绿。
- 2026-08-20（续16，v1.5.1）：VLM 打码引擎原语——`ExecuteAction drag`（`ParseDragCoords`
  解析 value="x1,y1,x2,y2" + `dragAt` 坐标直发：mousePressed → 贝塞尔 mouseMoved(buttons=1)
  → mouseReleased；bypass 语义匹配与挑战门禁）+ `CaptureScreenshot`（`Page.captureScreenshot`
  → PNG + 视口尺寸）；真实 Chrome `TestCDPSessionDrag`（滑块 50→100）/`TestCDPSessionCaptureScreenshot`
  全绿。

## 踩坑（2026-08-20 续16 实测）
- **拖拽命中 thumb 的关键**：`mousePressed` 必须落在 range 滑块 thumb 中心（不是元素
  左上角/边缘）——滑块绝对定位时按 top/height 把 thumb 中心对准 VLM 坐标（如
  top:192px;height:16px → 中心 y=200，value=50 → 中心 x=200）；否则 value 不动。
- **挑战状态必须随 Document 响应重算**：`challenge = (403 && 挑战头)`，页面 reload 为
  200 后自动清除。原 `Navigate` 收尾无条件清 `challenge`/`lastStatus` 会把初始导航刚
  判定的拦截冲掉（ENG-05 真实侧未接所以没暴露）；导航失败路径才清页面状态。

## 真实 CDP 会话设计要点
- `CDPFactory.Create`：附着（`AttachCDPURL`）→ `resolveBrowserEndpoint` +
  `Target.createTarget`（新标签页）+ `attachToTarget(flatten)`；启动 → 每上下文
  独立 headless Chrome（临时 user-data-dir，Cookie/缓存物理隔离）。
- 物理动作：`Runtime.evaluate` 语义 DOM 定位（`data-uws-target` 标记）→
  `elementFromPoint` 遮挡检测 → `Input.dispatchMouseEvent`（Fitts 三次贝塞尔）/
  `Input.insertText`；403+Turnstile 挑战 → `challenge_intercepted`。
- SPA 时序：`Page.navigatedWithinDocument`（pushState）+ Network XHR/Fetch 在途计数
  + MutationObserver 计数限频采样（50ms×2s 窗口）→ `NavigationState`。
- 页面候选：`PageCandidates` 采集交互节点文本（真实 MATCH-05/06 输入通道）。

## 踩坑（2026-08-20 实测）
- **Chrome 进程生命周期禁止绑 `exec.CommandContext` 的 Create ctx**：ctx 取消即杀
  进程，会话创建返回后浏览器立即退出（连接 reset）。启动等待用独立超时 ctx，
  进程由 `Close`（teardownChrome）管理。
- **JSON 解码结构字段必须导出（大写）+ json tag**：`elementTarget` 小写字段被
  `json.Unmarshal` 静默跳过 → found=false 假阴性。
- **Chrome 151 的 `--remote-debugging-port` / `--user-data-dir` 必须等号形式**：
  分列传参会把值当第二个 URL → 「Multiple targets are not supported in headless
  mode」直接退出（Python subprocess 触发，Go/shell 等号形式正常）。
- WS 握手必须与连接共享同一个 `bufio.Reader`：握手期服务端先推的数据帧若被握手
  新建的 reader 缓冲则永久丢失（真实 Chrome 可能先推事件）。
- `Runtime.evaluate` 返回值类型多样（标量/对象/数组）：统一 `evalRaw` +
  `evalValue/evalBool/evalResult` 按目标类型解码，避免「object 解 string」报错。
- **数字返回值禁止走 `evalResult`（string 解码）（2026-08-20 续26 实测）**：
  `(window.__uwsMutCount || 0)` 返回 JSON number，`json.Unmarshal` 到 string 报
  类型错误且被调用方静默吞掉 → 真实 CDP 的 DOM 突变信号（`domMutated`/候选缓存
  失效）从未生效。数字必须 `evalValue(ctx, expr, &n)` 解码 int/float。
- 真实 Chrome 集成测试用 `cdpProbe` 一次性探测（sync.Once）：沙箱/无 Chrome 时
  8s 探测一次后整组快速 skip，`make pre-commit` 保持全绿。
- **新标签页抢焦点后后台标签 Input 挂起（ENG-13 实测）**：点击 `target="_blank"`
  后新标签页获得前台焦点，原页面（后台标签）的 `Input.dispatchMouseEvent` 会被
  Chrome 挂起直到 ctx 超时（`context deadline exceeded`）。解法：物理点击/输入前
  `Target.activateTarget` 把本 Target 提到前台（对已在前台的页面是 no-op）。
- **Gemini 窄屏 responsive UI 的模型选择器按钮是 `<gem-button>` 不是 `<button>`
  （2026-08-26 实测）**：innerWidth=767 时 `button[data-test-id='bard-mode-menu-button']`
  匹配不到、报「模型选择器按钮未找到: <nil>」；同 test-id 元素实际是自定义元素
  `GEM-BUTTON`（可见可点，点击后菜单项 `gem-menu-item` 正常出现，菜单容器仍可能是
  `GEM-MENU`）。选择器与按钮文本校验必须用标签无关的
  `[data-test-id='bard-mode-menu-button']`；菜单打开检测补 `gem-menu-item` 可见兜底
  （容器 testid 在两种布局下偶有差异）。
- **新窗口事件必须用一次性读清语义**：`Target.targetCreated` 只在新 target 创建时
  触发一次，但引擎是按动作消费的；`NewWindowOpened()` 读后清零，避免后续动作
  重复误报 NEW_WINDOW_OPENED（与 `NavigationState` 的 pushState 位同一风格）。
- **`Target.setDiscoverTargets` 用浏览器级命令**（无 sessionId），订阅
  `client.On("", "Target.targetCreated", ...)`；只记 `type=="page"` 且非自身
  targetId 的 target，避免 DevTools/other 类型与启动期自身 target 误报。
- **页面候选缓存语义（TDD-OPT-01）**：`PageCandidates` 同文档缓存以
  docGen + 采样 mutCount + 2s TTL 三条件失效——多信号抽取共享一次探针采集
  （爬虫每页 3 信号 = 1 采集 + 2 命中，命中路径实测 ~6µs）；导航、SPA 突变
  被采样、或超过 TTL 后自动重采；缓存快照深拷贝隔离（调用方修改不污染）。
- **headless=new 对连接拒绝不发 `Page.loadFailed`**（Chrome 151 实测）：而是
  `Page.frameNavigated` → `chrome-error://chromewebdata/`（带 unreachableUrl）+
  `Page.loadEventFired`。导航失败必须双信号判定（loadFailed + frameNavigated
  chrome-error/unreachableUrl），且 loadEventFired 先到也不能放行（Navigate 等待
  loop 退出后二次校验 navErr）。

## 设计要点
- `Pool` = 容量信号量 + FIFO 队列（`acquireReq{slot chan}`）；等待超时（默认 30s）→
  `ErrResourceExhausted`；任务结束 `Release` → 上下文销毁、槽位回落。
- `BrowserContext` 独占一条 `CDPSession`（`SessionFactory` 注入，测试用内存 mock）；
  Cookie/缓存隔离靠「每上下文独立会话」实现。
- 优雅重启：`wg` 排空 + `drainTimeout`（默认 30s）→ 同四元组重建会话；
  已确认结果存 `confirmed`，重启不丢失。

## 踩坑
- **FIFO 断言必须「先入队再断言」**：先起 goroutine A、`waitQueue(1)` 后再起 B，
  靠 goroutine 调度顺序断言队首会 flaky。
- **500 次边界**：`needsRestartLocked` 在导航入口判定，当前计数 499 时下一次导航才触发
  重启；用例按「499 次循环 + 1 次 final」设计。
- **会话替换有数据竞争**：`session` 读写用独立 `sessionMu sync.RWMutex`，restart 写、
  Navigate/Probe 读。
- 排空期间若持续有新导航，`wg.Wait` 可能被拖住——drainTimeout 是硬保护。

## 后续
- ENG-12（bot.sannysoft.com 无头检测，依赖外网）；ENG-03 CDP 级鼠标事件采样断言
  （可选增强）；ENG-05 真实 Turnstile 页 E2E；真实 DOM 候选 Children 子图填充；
  启动模式 `--browser=cdp` CLI 端到端（可选，Go 集成已覆盖核心路径）。
  ENG-13 已点亮（2026-08-20）：`Target.setDiscoverTargets` + `targetCreated` →
  `WindowEvents` → 引擎 NEW_WINDOW_OPENED；协调器真实 Attach 新 Target 待 v1.5 定案。
- P1/P2（待 `docs/evidence/research/cdp-research.md` 调研结论分批）：viewport/proxy 应用（ENG-01 真实侧
  补全）、优雅重启保登录态、Chrome 崩溃自愈、shadow DOM、iframe、Cookie 注入失败
  可观测、wsconn 加固（Close 加锁/帧上限/控制帧长度）。
  ~~SessionUrl 真实化~~（2026-08-27 已做：`TargetDebugInfo.TargetID/DebugPageURL`）。
# 物理层记忆

## 2026-08-27：findElementOpt 语义评分分支漏声明 `r`（「会话路由 Bug」真根因）

- 现象：uws 对小说站 /store 的 extract/click 全部 `ABORTED target_not_found`，
  CDP 直读与 Playwright 一次通过 → 一度误判为「附着到错误 Tab」的会话路由 Bug。
- 根因（诊断通道实证）：`findElementOpt` 的 JS 模板在**语义评分分支**构造
  `best = {x: r.left + r.width/2, ...}` 引用了从未声明的 `r`（该变量只声明在
  `text=` 分支与「选择器命中但无语义分」回退分支）——只要元素语义分 >0
  （如 /store 的「More ›」得分 80）必然抛 `ReferenceError: r is not defined`；
  旧 `executeAction` 又把 `err != nil || !found` 统一 mask 成 `target_not_found`，
  黑盒实验区分不出根因。选择器路径（DeepSeek/Gemini chat）靠「零分回退」分支
  恰好绕开该行，所以聊天链路从未暴露。
- 修复：评分分支在构造 best 前自声明 `const r = el.getBoundingClientRect();`。
- 诊断通道（同批落地，避免再黑盒空转）：`executeAction` 不再 mask——失败原因
  分类为 `eval_error:<exceptionDetails>` / `eval_empty` / `no_match`，并带
  `|url=<location.href>`；`findElementOpt` 对 eval 成功但 value 为空返回
  `errEvalEmpty` 哨兵。真实复现：修前
  `eval_error:...ReferenceError: r is not defined|url=https://myread.top/store`；
  修后 extract「More」成功返回 `More ›`、click 导航到
  `/more/h5Banner?sectionId=...&title=Banner`（与 CDP/Playwright 直读一致）。
- 回归测试：`TestCDPSessionFindElementSemanticMatchNoScopeBug`（真实 Chrome，
  沙箱自动 skip）+ 纯逻辑 `TestClassifyFindFailure` / `TestWsBaseFromEndpoint`。

## 2026-08-25 深夜：聊天 SSE 网络层捕获（方案 1，browser.ChatStreamSource）

- 根因：DeepSeek 联网搜索的 `-1-4-7` 碎片来自 DOM 引用角标二次渲染；DOM innerText
  会把独立角标节点（如 `<span class="ds-citation">`）的换行/连字符摊平进正文。
- 方案：CDP Network 层被动劫持，不注入页面 JS、不暂停请求——
  `Network.requestWillBeSent` 命中 `chat/completion` → goroutine 武装
  `Network.streamResourceContent` → `Network.dataReceived.data`（base64 增量，
  新版 Chrome）→ `Network.loadingFinished/Failed` 收尾。
- 踩坑：**事件处理器运行在 CDP reader goroutine 上，绝不能在事件回调里同步
  CallSession**（响应仍由同一 reader 读取 → 死锁）；武装必须 goroutine 化。
- 乱序防护：武装完成前的增量进 preFeed 队列，`onBuffered` 先喂 bufferedData 再
  冲刷队列；`onFinish` 也要先冲刷 preFeed（武装响应可能慢于流结束）。
- 兼容降级：旧版 Chrome 不支持 streamResourceContent 时 `onArmFailed` 放行
  preFeed，`dataReceived.data` 仍可能携带增量；全缺时快照为空，API 层回退 DOM。
- 多轮语义：每轮新请求 Epoch+1 并重置 sink（旧轮内容不再污染当前快照）；
  `loadingFailed` 放弃在途轮次但不标记 Done。
- **失败轮标记（2026-08-26 实测修复）**：`loadingFailed`/中止/响应体拉取失败
  不能只清 `active`——否则 Epoch>0 且空快照会永久霸占 extract(prefer_sse)，
  DOM 已渲染的回答读不到（「页面已答但 API 超时」根因）。chatStream 新增
  `failed` 标记：onFail 仅对当前 active requestId 置位、onRequestStart 清零、
  snapshot 带出；引擎据此回退 DOM observed。失败轮残留半截内容保留供审计。
- 解析器：SSE 按行解析（data: 行 + 空行成事件），跨 chunk 截断靠 tail 保留；
  宽容 JSON 覆盖 DeepSeek（text_delta / reasoning_delta / answer_finished）与
  OpenAI（choices[].delta + finish_reason）两种形态；`[DONE]` 标记完成。
- 真实 Chrome 集成测试用本地 httptest SSE 端点（与 DeepSeek 端点同构），
  沙箱内自动 skip，CI/本机可跑。

## 2026-08-25 R-A：规则选择器定位（findElementOpt / SelectorActionSession）

- `findElement` 原只做「文本/属性包含语义」评分（innerText/value/aria-label/data-testid/title/
  placeholder），纯 SVG 图标按钮、无文案输入框永远匹配不到 → 新增 `findElementOpt(ctx,
  semantic, selector)`：selector 非空时 `querySelectorAll(sel)`，集合内语义评分择优，
  零分元素回退第一个可见命中；无效选择器显式报错。
- `ExecuteActionWithSelector` 经 `SelectorActionSession` 可选接口暴露，引擎 `executePhysical`
  解析 KB 规则选择器后调用；模拟会话不实现该接口，自动降级纯语义。
- Chrome 对部分畸形选择器（如 `div[not-a-real`）宽容返回空集不抛错，测试须用实测抛错的
  选择器（`###`）锁定报错行为。

## 2026-08-25 深夜续：DeepSeek Web SSE 补丁式协议（真实抓包）

- **协议已升级**（旧 text_delta/reasoning_delta 形态不再出现）：事件序列 =
  ① ready `{"request_message_id":...}` → ② update_session → ③ 初始快照
  `{"v":{"response":{"fragments":[{"id":2,"type":"RESPONSE","content":"```"}]}}}`（首段
  正文随快照下发）→ ④ 首个显式补丁 `{"p":"response/fragments/-1/content","o":"APPEND",
  "v":"mark"}`（确立路径，-1=最后一个 fragment）→ ⑤ **隐式增量 `{"v":"down"}`**
  （省略 p/o，沿上一补丁路径继续 APPEND——漏掉它 = 快照永远只有首段）→ ⑥ 收尾
  `{"p":"response/status","o":"SET","v":"FINISHED"}` 或 BATCH `quasi_status=FINISHED`。
- 思考链/正文是不同 fragment（type=THINKING/RESPONSE），按 id 或 -1 路由；
  解析时按 type 把增量分别累计到 reasoning/content。
- `Network.dataReceived.data` 只有在**自己会话** `streamResourceContent` 武装后才带
  base64 增量；旁路会话不武装则 data 恒空（旧版 Chrome 降级语义）；事件按 reader
  goroutine 严格有序，不存在 loadingFinished 抢跑。
- **完成判定不能靠「文本稳定 1.5s」**：新版流式生成中途停顿可能 >1.5s，半截
  （实测 "Here is"/"**JavaScript"）会被误判为完整 → 引擎 extract 带
  `chat_stream_done`（ActionResultResponse=9，v1.5.2），API 在 SSE 接入时只信 done。

## 2026-08-27 VLM 兜底物理原语（ClickAt / CaptureScreenshotRegion）

- `ClickAt(x,y)`：elementFromPoint 解析命中元素 → JS 生成 CSS 路径（tag + class×3 +
  nth-of-type，遇带 id 祖先截断）→ 与语义点击共用 `clickPoint`（activateTarget +
  Fitts 贝塞尔 + mousePressed/Released + startMutationSampling）→ 返回路径供引擎学习。
- `CaptureScreenshotRegion(x,y,w,h)`：`Page.captureScreenshot` + `clip{scale:1}`，
  坐标钳制视口内；实际裁剪尺寸回传（VLM 精修换算用）。
- 踩坑：clip 截图尺寸 = 裁剪逻辑像素（非整页），VLM 精修返回的 ratio 必须乘以裁剪
  尺寸再加裁剪原点，不能直接当整页坐标。
- **判定层证据链（v1.5.4，2026-08-27）**：`elementTarget` 增 `Sel`（CSS 路径）；
  `findElementOpt` 命中即记 `lastHit`，`enrichHitSelector` 在动作执行前用
  `elementPathAt(点击点)` 补全真身路径（elementFromPoint，与 ClickAt 同源），
  `LastHitElement()` 经 `browser.HitElementSource` 供引擎回显；
  `ClickAt` 同样记录（selector+坐标）。注意：证据 selector 是「点击点真身」而非
  「定位到的容器」——drama-h5 点容器假阳性正是靠它暴露（命中 chapter-grid 而非
  chapter-item）。
- **UA 指纹纪律（2026-08-27）**：`cdp_session.go` 只在 `cfg.UserAgent != ""` 时才
  `Emulation.setUserAgentOverride`；引擎侧已删除默认 "UWS/1.4 Engine" 填充，附着模式
  保留真实浏览器 UA。反爬实证：被覆盖 UA 的引擎 Tab 在百度/smzdm 弹验证码，同一
  Chrome 普通 Tab 不弹（dom_probe 见 `navigator.userAgent` 差异）。新增 UA 覆盖必须走
  `launch_overrides.user_agent` 显式指定。

## 新窗口接管（TDD-ENG-21，2026-09-22 用户现场反馈批次 P0-1）

- **缺口**：#13 只在 `Target.targetCreated` 时记下新 target 供引擎回报 `NEW_WINDOW_OPENED`
  （`NewWindowOpened()` 一次性读清），**没人把它变成作用面**——微信快捷登录的授权弹窗因此
  只能人工点「允许」。改用 `SwitchTarget`：attach 新 target（flatten）→ `Runtime/Page/Network.enable`
  → 换 `sessionID/targetID` → 重放身份（`applyTargetIdentity`：UA/Client Hints、语言、时区、
  指纹 hook + Worker hook、视口、探针预置、预置 Cookie）→ `clearPageState` → `OffSession(旧)` +
  `subscribe` → 观看流迁移 → `Target.detachFromTarget(旧)`。
- **初始化路径已抽公共段**：`enableTargetDomains` / `applyTargetIdentity` 由 `initTarget` 与
  `SwitchTarget` 共用——**新页必须重放同一份身份**，否则同会话两个页面两套指纹当场穿帮。
- **旧页事件必须摘掉**：`cdpClient.OffSession(sessionID)` 一次注销该 session 全部处理器。
  不摘就是旧页的 `frameNavigated`/`Network.*` 继续写本会话的 `navErr`/`challenge`/聊天流状态机。
- **观看流绑在 sessionID 上**：`screencastStream` 的 command 闭包捕获旧 session，接管后必须
  停旧流、重建（`getScreencastStream` 重新捕获新 session）并用原 `options` 重启，否则出现
  「动作打新页、面板还演旧页」。参数因此记在 `screencastStream.options`。
- **`TargetID()` 语义变了**：返回**当前作用**的 target（接管后前移）；客户端手里的原钉扎值由
  `BrowserContext.PinnedTarget` 记住，引擎钉扎校验两者都接受。
- **R1 红线**：全程不调 `Target.activateTarget` / `Page.bringToFront`，新页在后台被操作。
- **验证**：`TestCDPSessionSwitchTarget`（真实 Chrome，`UWS_RUN_CHROME_E2E=1`）：新页可读可点、
  原 target 保留（`pageTargets ≥ 2`）、可双向切回。
