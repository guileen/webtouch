# UWS Viewer —— 原生观看小窗（macOS AppKit）

一个隔离的 macOS 13+ Swift Package：证明观看壳的 AppKit 边界。
只用 `NSPanel`、`NSView`、`CALayer` 与 Core Graphics，**零 WebView、零 SwiftUI、零 Chrome/CDP 客户端、
零截屏、零网络传输**。真实 Chrome 页面由引擎侧帧源提供（页面 JPEG + 归一化指针对），本包只负责画出与展开。

不启动 GUI 的验证：

```sh
swift build
swift test
```

## 当前形态（S2 自绘壳，2026-09-12）

- **小窗态**：400×258 圆角 11 的无边框 `NSPanel`，贴可见屏右上角；
  `.nonactivatingPanel` + `canBecomeKey=false` ⇒ 不抢键盘焦点（R1）；
  `level=.floating` + `.canJoinAllSpaces` + `.fullScreenAuxiliary` ⇒ 始终可见（R3）。
- **拖动与吸附**：按住小窗拖动，松手后自动吸到最近的横向边缘。
- **独立虚拟光标**：`ViewerCursorLayer` 自绘箭头（白描边 + 蓝光晕 + 按压缩放），
  由归一化坐标驱动、带距离自适应补间；点击有涟漪。**不向页面 DOM 注入任何元素**（R2）。
  **没有指针数据就不显示**：默认位置 (0.5, 0.5) 只是占位，先画出来就是一根停在面板
  正中的假鼠标（2026-09-23 现场反馈「鼠标（小窗）仍然在屏幕中间」）；现在只由真实
  指针包唤醒，换会话/换页面后自动收回隐藏态（`reveal()` / `resetPointer()`）。
- **状态标签**：小窗左下角半透明胶囊显示「Agent 名 · 正在做什么」。
- **点击展开 = 真窗口到最前（2026-09-22 用户口径）**：点小窗主体 → `ViewerIntent.requestExpansion` →
  经同一根 UDS 发 `expand` 意图给引擎；引擎把这个会话**自己的真窗口**抬到最前（**不搬位置、不缩放**），
  回 `intentAck ok=true` + 真实矩形，壳随即播一段「飞向它」的动画（**0.18s `.easeOut`**，落点**就是回执矩形本身** ——
  整块从当前小窗矩形放到真窗口矩形，四角向外飞；动画里只插值 `frame`/`alpha`，不测量、不查 CDP）后
  `orderOut`——用户摸到的是真页面本身（能点、能滚、能过人机验证），不是放大的图片。
  **「到最前」是两步，且都在动画末段（2026-09-24 第二轮口径，规范 §4.4 第 3 条）**：`expand` 意图带
  `deferRaise: true`，引擎**只把这扇真窗口摆好、不激活**（`PrepareViewerWindow`）；壳飞到动画**末段**
  （`expandDuration` 的最后一个 `0.03s`）才做那两下：① 发 `raise` 意图 → 引擎执行 `Target.activateTarget`
  （把这扇窗口与该 Tab 设为活动）；② 按回执里的 `browserPID` 用 `NSRunningApplication.activate` 抬这个进程
  （`ViewerAppActivation.swift`；日志 `uws-viewer activate browser: pid=… ok=…`）。**先抬窗口再播动画**会被看成
  「窗口先到、小窗动画后到」两段脱节，所以抬升排在末段。
  **订正（2026-09-24 复核）**：上一版这里写「`Target.activateTarget` 在 macOS 上不保证把 App 抬到最前」——
  已被隔离取证**证伪**（自建会话 3/3 试次里引擎那一步本身就把前台切了过去，0.017–0.026s）。正确表述：引擎
  那一步在隔离环境**够用**，壳这一步是**真实场景兜底**（前台是同 bundle 的另一个 Chrome 实例时，只有按 pid
  才指得到本会话的那一只）。
  **末段这一下必须排在非主线程**（`ViewerPanelController.raiseQueue`）：展开瞬间主线程在解码引擎推进来的大帧，
  `DispatchQueue.main.asyncAfter` 真机实测被推迟到 0.19–0.68s（设计值 0.15s），窗口会落到动画演完之后。
  **只按 pid、绝不按 bundle id**：按 bundle id 会命中用户自己的 Chrome，2026-09-13 评审已拒绝（规范 §7.4）；
  附着模式拿不到 pid（回 0）、无头会话没有窗口，都不动手，交给引擎那一步。
  位置控制在真机上是缺陷源（#113：停靠被显示器布局夹回可见区、只剩 40px 边），所以壳不再报尺寸、
  引擎也不再搬窗；`ViewerWindowSize` 留在 wire 协议上只为不跟老壳断连，引擎忽略它。
  唯一的例外是「窗口整块停在屏外」（引擎默认停靠 `left=20000`）：引擎按工作区左上角把它搬进屏内
  并记下原位（`pkg/browser/viewer_window.go`），这时候收回会真的还原位置。
  引擎抬不起来（无头会话 / 真窗口已被关掉）或 5s 没回执 → 壳回落到自绘放大面板，保证「点开一定有
  东西」；真窗口被用户手动关掉时引擎推 `sessionClosed`，壳收起面板而不是停在最后一张截图上。
  **超时的语义是「只回落、不撤销」（2026-09-22 现场批次 #2c）**：超时窗口是 5s，与引擎
  `viewerIntentTimeout` 同量级（一次展开是 5~6 次 CDP 往返，1.2s 在忙页面上必超时，那正是
  「点开只是一张点不动、滚不动的图」的根因）。超时后壳先画自绘放大面板，但**不**补发 `collapse`；
  迟到的 `intentAck ok=true` 会把自绘面板**升级**成真窗口。只有「用户等回执时点了收回」才替他把
  已经抬起来的窗口放回去。这条链路的状态机是纯逻辑 `ViewerExpansionMachine`（可 `swift test`）。
- **小窗出现判据 = 真窗口「你看不看得见」（2026-09-24 第二轮口径，规范 §4.4 第 5/6 条）**：
  展开之后屏幕上只管留一个。壳 0.4s 轮询一次窗口列表（`ViewerWindowVisibility`，纯函数、可单测）：
  算真窗口此刻的**可见面积占比**——**≥ 60% 判「看得见」→ 小窗 `orderOut` 让位**；最小化 / 在别的 Space /
  被挡到 60% 以下 → 小窗亮回来继续直播（日志 `panel shown|hidden: 真窗口看不见|看得见（可见 X%）`）。
  真窗口只是**失焦但仍在屏幕上**时不再叠小窗：旧口径盯「前台」，而那只是可见性的一个子集，
  用户看到的正是重复遮挡。遮挡者只算**同 layer（0）且排在目标窗之前**的窗口（跨层算会把 ChatGPT 的
  `layer 3` 大窗误算成遮挡），面积 < 200×150 的一律不算；取不到窗口列表才退回旧的前台判据。
  引擎拉起的窗口按 `browserPID` 认；**附着模式（`--attach`，`browserPID=0`）按展开回执里的 `windowBounds`
  在屏上认窗**（各边容差 4px）补出 pid 并记住，此后按 pid 走——这是让新判据在你自己的 Chrome 上生效的关键
  （真机读数 `host resolved from window bounds: pid=16742`）。**收回胶囊已删除**
  （`ViewerCapsule.swift` 与它的 placement 测试随之退役）——新口径下收回就是「切回去用别的应用」，
  或者 ✕ 真关这块面板。为了让「飞向它」的动画演得出来，展开链路在飞的时候前台轮询先让开
  （`suppressPresenceHide`）。
- **焦点纪律（两形态一致，2026-09-12 按用户现场反馈修正）**：展开窗同样是
  `.nonactivatingPanel` + `canBecomeKey=false`，展开只 `orderFront`、绝不 `makeKeyAndOrderFront`
  或 `NSApp.activate`；因此置顶是默认强制的，但用户正在别处打字时**不会失焦**。
  展开窗自己的控件用 `FirstMouseButton`，在非 key 窗口也能一次点中。
- **演示帧**：`UWS_VIEWER_ANIMATE=1`（默认）由 `DemoPage` 程序化画一张标注 `DEMO FRAME` 的假页面，
  用于在真实帧源接线前校验观感；`UWS_VIEWER_ANIMATE=0` 只显示静态帧。

  **只留一块小窗（2026-09-22 现场批次 #2）**：屏幕上只有一块面板，它跟随**最近有动作**的会话
  自动切换（`ViewerPanelRouter`：只有帧没有动作的会话抢不走面板，否则多 Agent 并发时会来回翻）。
  多会话在引擎侧仍各有 target/窗口/画面流，壳只是同时只显示一块。级联槽位（`ViewerSessionRegistry`）
  随这次改造退役；面板位置就是用户拖到哪儿是哪儿。
- **关得掉的小窗（2026-09-16，用户现场「几个中台测试的小窗关不掉」；2026-09-22 升级为真关）**：
  小窗右上角常显 `✕ 关闭`（`xmark.circle.fill`，`FirstMouseButton`，点它不抢焦点），点击**真的**
  丢弃这块面板与会话绑定，并发一条 `close` 意图给引擎——引擎立刻静默回收该会话（停推流 + 一条
  `sessionClosed`）。不结束任务、不关那扇真窗口、不影响 `--session` 续页；关掉之后画面照旧在流，
  但除非该会话有**新的动作事件**（画面/标签变化不算），否则不再弹回来。引擎侧配套两条：
  会话静默（默认 5 分钟无动作/无指针，`UWS_VIEWER_IDLE_CLOSE_SEC` 覆盖）后推一次 `sessionClosed`
  收面板而**不**结束会话；用户把真窗口展开看着时（`SetQuietHold`）绝不自动收。壳在引擎重连时
  清空全部旧面板（`purgePanels`），不留一张没人维护的陈旧画面。
- **单实例常驻（2026-09-15）**：壳在与 socket 同目录的 `viewer.lock` 上打 `flock`（与
  `uws viewer app` 同一约定），重复拉起只复用已在跑的壳、不叠第二套小窗；`demo` 模式不参与互斥，
  保证可视化取证可重复采集。壳默认连引擎的 `~/.uws/viewer.sock`（引擎默认开推流）。
- **取景与断线（2026-09-13）**：画面以 `.resizeAspect` 落在页面区，虚拟光标/涟漪按**画面真正落到的矩形**
  换算（`ViewerGeometry.aspectFitRect`），否则信箱边会让「画出来的指针」与画面里的位置差一条边；
  引擎链路断开时胶囊/展开条显示「引擎未连接 · Ns 后重试」，不让最后一帧假装还在更新。

```sh
swift run uws-viewer-prototype            # 小窗 + 光标演示
UWS_VIEWER_ANIMATE=0 swift run uws-viewer-prototype
UWS_VIEWER_AGENT="Agent A" UWS_VIEWER_TITLE="Chapter 1" swift run uws-viewer-prototype
```

## 边界（谁负责什么）

- 本包负责：小窗/展开窗的 AppKit 行为、原生绘制、虚拟光标与涟漪、拖动吸附、焦点纪律，
  以及把用户手势编码成意图发给引擎（`ViewerSocketProtocol.swift`）。
- 引擎负责：`Page.startScreencast` 帧流、动作坐标事件、会话路由、背压，以及按意图搬动这个会话自己的
  真窗口（`../pkg/browser/viewer_window.go`）。
- 已接线：本地帧传输（Unix socket，JSON lines，见 `../pkg/viewerhub/`）与真 screencast 画面渲染；
  反向意图通道（`pkg/viewerhub/intent.go`）承载「展开 = 真窗口到最前 / 收回 / 关掉（`close`）」。
- 真机复验（2026-09-22 19:40/19:41，一次真实任务）：展开回执 `ok=true`（273ms）、真窗口从屏外被救援进屏内并抬到最前、
  小窗「飞向它」的动画落点后 `orderOut`、互斥不变量**双向**成立（浏览器在台前 → 小窗 hidden；切到访达 → 小窗亮回来）；
  展开后摸到的就是真页面本身（点击/滚动/人机验证本轮未单独取证）。
  **尚未复验**：多会话同时活跃的换绑跟随最近活跃会话、✕ 真关、静默会话复用不唤回面板、小窗落点在重启后的显示器归属
  （见 `docs/evidence/README.md` §5 阶段表 S4/S5）。
