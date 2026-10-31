import AppKit
import QuartzCore

/// The compact/expanded viewer shell.
///
/// Requirement mapping: R1 (never steals keyboard focus, in either
/// presentation) is enforced by non-activating panels that cannot become key;
/// R3 (always visible, by default and not as an opt-in) by `.floating` plus
/// `.canJoinAllSpaces`; R4 (click to expand/collapse) by the canvas click path;
/// R6 (no WebView) by drawing page JPEGs and every overlay natively.
///
/// AppKit behaviour involving other applications, Mission Control, Stage Manager
/// and full-screen Spaces still needs real-machine verification.
public final class ViewerPanelController: NSObject, NSWindowDelegate {
    public let compactPanel: NSPanel
    public let canvas: ViewerCanvasView
    public let expandedCanvas: ViewerCanvasView
    public let state: ViewerState
    public private(set) var presentation: ViewerPresentation = .compact

    /// Kept for P1 call sites: the compact panel is the canonical `panel`.
    public var panel: NSPanel { compactPanel }

    private let expandedPanel: NSPanel
    /// 小窗的槽位（用户放下的位置就是它的家，规范 §4.4 第 9 条）。展开飞走/让位隐藏/收回/
    /// 换绑会话都不改它；只有用户再拖一次、或窗口被 macOS 夹取/缩放才更新。
    private var slot: ViewerPanelSlot
    private var snapWorkItem: DispatchWorkItem?
    private var isProgrammaticMove = false
    private var isUserDragging = false
    private var currentImage: NSImage?

    /// 用户点了小窗上的「关闭」：壳自己收起面板并上报宿主。它不结束任务、不关真
    /// 窗口，只表达「不想再盯着这个会话了」。
    public var onDismiss: (() -> Void)?

    /// 承载这个会话页面的浏览器进程（引擎的帧更新 / 回执里的 `browserPID`）。附着模式下
    /// 没有它，前台判定只能按应用类别认（`ViewerPanelPresence`）。
    private var hostPID: Int?
    /// 认窗时记下的 `CGWindowID`：**按那扇窗**算可见性，而不是「该 pid 名下面积最大的窗」
    /// （用户的 Chrome 常常同时开着好几扇同 pid 的大窗，规范 §4.4 第 6 条）。
    private var hostWindowNumber: Int?
    /// 展开回执里那扇真窗口的矩形（Chrome 坐标空间）：附着模式认进程用，见 `resolveHostPIDIfNeeded`。
    private var hostWindowBounds: CGRect?
    /// 这块小窗是因为「浏览器在台前」才被藏起来的（2026-09-22 互斥不变量）：只有它能让
    /// 轮询把小窗亮回来——用户 ✕ 掉的面板、没绑会话的面板都不算。
    private var hiddenByRealWindow = false
    /// 动画末段那一次「抬真窗口」的待办（`flyAwayAndHide(raiseAtEnd:)` 排的）。
    private var raiseWorkItem: DispatchWorkItem?
    /// 「末段抬升」的执行队列：**故意不在主线程**。2026-09-24 真机读数：展开那一瞬间主线程
    /// 在解码/绘制引擎推进来的大帧，`DispatchQueue.main.asyncAfter` 被排到 0.19/0.26/0.35/
    /// 0.52/0.68s（设计 0.15s），用户看到的就是「小窗先飞完、真窗口后到」——两段脱节（§4.4
    /// 第 3 条要修的正是这个）。抬升那一下只有两件事：给引擎发 `raise`（socket 自带锁，可跨
    /// 线程写）、按 pid 激活（AppKit，调用方自己丢回主线程），所以它只受这一条延时的控制。
    private let raiseQueue = DispatchQueue(label: "uws.viewer.raise", qos: .userInitiated)
    /// 展开链路在飞的时候先别按前台判定藏：这一瞬间「浏览器变前台」是引擎替用户点的那
    /// 一下（抬到最前），不是用户切走了工作；小窗要留着播「飞向它」的动画。
    public var suppressPresenceHide = false
    /// 重新亮起来之前问一句「这块面板现在还该存在吗」（宿主传「还绑着会话」）。用户 ✕ 掉
    /// 之后画面照流，轮询不能把它复活。
    public var canShow: (() -> Bool)?
    /// 把浏览器在前台时的小窗让位/亮回来做成硬不变量：0.4s 轮询一次前台应用。
    private var presenceTimer: Timer?
    private static let presenceInterval: TimeInterval = 0.4

    /// R4b (2026-09-12 user feedback): an expanded shell is a review state, so
    /// it folds itself back the moment the user returns to their own work.
    private let autoCollapseEnabled: Bool
    private var focusGuard = ViewerFocusGuard()
    private var focusObservers: [NSObjectProtocol] = []
    private var globalClickMonitor: Any?
    private var now: () -> TimeInterval = { ProcessInfo.processInfo.systemUptime }

    public init(state: ViewerState, frame: NSRect? = nil, autoCollapse: Bool = true) {
        self.state = state
        self.autoCollapseEnabled = autoCollapse
        let visible = NSScreen.main?.visibleFrame ?? CGRect(x: 0, y: 0, width: 1440, height: 900)
        let startFrame = frame ?? ViewerGeometry.compactFrame(inVisibleFrame: visible)
        self.slot = ViewerPanelSlot(frame: startFrame)
        self.canvas = ViewerCanvasView(session: state.session, mode: .compact)
        self.expandedCanvas = ViewerCanvasView(session: state.session, mode: .expanded)
        self.compactPanel = PassiveViewerPanel(
            contentRect: startFrame,
            styleMask: [.borderless, .nonactivatingPanel],
            backing: .buffered,
            defer: false
        )
        self.expandedPanel = ExpandedViewerPanel(
            contentRect: startFrame,
            styleMask: [.titled, .closable, .resizable, .fullSizeContentView, .nonactivatingPanel],
            backing: .buffered,
            defer: false
        )
        super.init()

        compactPanel.isFloatingPanel = true
        compactPanel.level = .floating
        compactPanel.hidesOnDeactivate = false
        compactPanel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .stationary]
        compactPanel.isMovableByWindowBackground = false
        compactPanel.hasShadow = true
        compactPanel.backgroundColor = .clear
        compactPanel.isOpaque = false
        compactPanel.contentView = canvas
        compactPanel.delegate = self
        compactPanel.identifier = NSUserInterfaceItemIdentifier("uws-viewer-compact")
        compactPanel.minSize = ViewerGeometry.minimumSize

        expandedPanel.title = state.session.pageTitle.isEmpty ? "UWS Viewer" : state.session.pageTitle
        expandedPanel.titlebarAppearsTransparent = true
        expandedPanel.titleVisibility = .hidden
        expandedPanel.standardWindowButton(.zoomButton)?.isHidden = true
        expandedPanel.standardWindowButton(.miniaturizeButton)?.isHidden = true
        expandedPanel.isReleasedWhenClosed = false
        expandedPanel.hidesOnDeactivate = false
        expandedPanel.isFloatingPanel = true
        expandedPanel.level = .floating
        expandedPanel.isMovableByWindowBackground = false
        expandedPanel.becomesKeyOnlyIfNeeded = false
        expandedPanel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .stationary, .ignoresCycle]
        expandedPanel.minSize = ViewerGeometry.minimumSize
        expandedPanel.contentView = expandedCanvas
        expandedPanel.delegate = self
        expandedPanel.identifier = NSUserInterfaceItemIdentifier("uws-viewer-expanded")

        // P1 contract: expansion stays an intent. The host application decides
        // what it means; the compact panel never expands on its own.
        canvas.onExpandRequested = { [weak state] in state?.requestExpansion() }
        canvas.onDismissRequested = { [weak self] in self?.dismiss() }
        // 自绘放大面板上的「收回」也要上报宿主：宿主才知道那次「点开」真的结束了
        // （否则迟到的成功回执会把面板重新弹成真页面——2026-09-22 现场批次 #2c）。
        expandedCanvas.onCollapseRequested = { [weak self] in self?.state.requestCollapse() }
        canvas.onDragBegan = { [weak self] in self?.beginUserDrag() }
        canvas.onFrameChanged = { [weak self] in self?.endUserDrag() }
        if autoCollapseEnabled { installFocusObservers() }
        startPresenceTracking()
    }

    /// Forwards the host-level engine link down to both drawings (compact pill
    /// and the self-drawn expanded bar).
    public func setConnectionState(_ link: ViewerLinkState) {
        canvas.setConnectionState(link)
        expandedCanvas.setConnectionState(link)
    }

    deinit {
        presenceTimer?.invalidate()
        if let globalClickMonitor { NSEvent.removeMonitor(globalClickMonitor) }
        let center = NSWorkspace.shared.notificationCenter
        for observer in focusObservers { center.removeObserver(observer) }
    }

    // MARK: - 互斥不变量：小窗直播 ⟺ 真窗口不在前台（2026-09-22 口径）

    /// 引擎告诉你这块面板在放谁的真页面（帧更新的 `browserPID`，或展开回执的
    /// `browserPID`）。附着模式拿不到就是 `nil`，前台判定退回按应用类别认。
    public func setHostPID(_ pid: Int?) {
        hostPID = pid
    }

    /// 这个会话那扇真窗口的矩形（Chrome 坐标空间，与 `CGWindowList` 同一套左上原点）。
    /// 两个来源：**引擎随帧下发的**（规范 §4.4 第 7 条——不等用户点小窗就生效）与展开回执里的。
    /// 附着模式（`browserPID=0`）靠它在屏上**认窗 → 认进程**，第 5 条判据才在用户自己的
    /// Chrome 上生效（规范 §4.4 第 6 条）。只用来认一次。
    public func setHostWindowBounds(_ bounds: CGRect?) {
        hostWindowBounds = bounds
    }

    private func startPresenceTracking() {
        presenceTimer?.invalidate()
        let timer = Timer(timeInterval: Self.presenceInterval, repeats: true) { [weak self] _ in
            self?.pollPresence()
        }
        // `.common`：拖小窗的时候主线程在 `.eventTracking`，默认模式下的定时器会被按住。
        RunLoop.main.add(timer, forMode: .common)
        presenceTimer = timer
    }

    /// 只在这一种形态下动：小窗（`.compact`）。展开态的自绘面板是用户点开的那张图，
    /// 该不该收回由焦点纪律和用户手势决定，不归这轮询管。
    ///
    /// 判据（2026-09-24 第二轮口径，规范 §4.4 第 5/6 条）：**真窗口你看不看得见**。在屏上、
    /// 且可见面积 ≥ 60% → 小窗让位；最小化/在别的 Space/被挡到 60% 以下 → 小窗直播。
    /// 拿不到窗口列表或附着模式没有 `browserPID` 时退回焦点判据（`givesWay`）。
    private func pollPresence() {
        guard presentation == .compact, !suppressPresenceHide else { return }
        let front = NSWorkspace.shared.frontmostApplication
        let browserFrontmost = ViewerPanelPresence.isHostBrowser(
            frontmostPID: front?.processIdentifier,
            hostPID: hostPID,
            bundleIdentifier: front?.bundleIdentifier,
            localizedName: front?.localizedName
        )
        let snapshots = ViewerWindowVisibility.onScreenWindows()
        let selfPID = ProcessInfo.processInfo.processIdentifier
        resolveHostWindowIfNeeded(snapshots: snapshots)
        var ratio: Double?
        let realWindowVisible: Bool?
        if snapshots.isEmpty || (hostPID ?? 0) <= 0 {
            // 算不出来（API 空/权限受限）或认不出「谁是这个会话的浏览器」（附着模式 pid=0）：
            // 退回焦点判据——宁可多藏一次小窗，也不能让真窗口被小窗压着。
            realWindowVisible = nil
        } else {
            ratio = ViewerWindowVisibility.visibleRatio(
                windowNumber: hostWindowNumber,
                browserPID: pid_t(hostPID ?? 0),
                selfPID: selfPID,
                windows: snapshots
            )
            // 屏上没有它的窗口（最小化/已关/在别的 Space）= 看不见 = 小窗该出现。
            realWindowVisible = (ratio ?? 0) >= ViewerWindowVisibility.visibleRatioThreshold
        }
        switch ViewerPanelPresence.decide(
            realWindowVisible: realWindowVisible,
            browserFrontmost: browserFrontmost,
            panelVisible: compactPanel.isVisible,
            hiddenByRealWindow: hiddenByRealWindow
        ) {
        case .unchanged:
            break
        case .hide:
            hiddenByRealWindow = true
            compactPanel.orderOut(nil)
            NSLog("uws-viewer panel hidden: 真窗口看得见（可见 %@，pid=%@）",
                  ratio.map { String(format: "%.0f%%", $0 * 100) } ?? "按前台判据",
                  front.map { String($0.processIdentifier) } ?? "-")
        case .show:
            hiddenByRealWindow = false
            guard canShow?() ?? true else { return }
            showCompact()
            NSLog("uws-viewer panel shown: 真窗口看不见（可见 %@）",
                  ratio.map { String(format: "%.0f%%", $0 * 100) } ?? "按前台判据")
        }
    }

    /// 补齐「这是哪一扇窗、谁的窗」：引擎（`--attach` 到用户自己的 Chrome）不知道谁是浏览器
    /// 进程，但它把**那扇窗的矩形**随帧给了壳（规范 §4.4 第 7 条）。按矩形认一次，记下
    /// **窗口号 + pid**，之后就按那扇窗走——用户把窗口拖到哪儿、缩放多少都不影响判定。
    /// 认不出（窗口已关、列表拿不到）就保持现状，退回焦点判据。
    private func resolveHostWindowIfNeeded(snapshots: [ViewerWindowSnapshot]) {
        if let number = hostWindowNumber,
           let window = snapshots.first(where: { $0.number == number && $0.layer == 0 }) {
            // 已经认过、那扇窗还在屏上：不必重新匹配（用户挪窗口也不影响）。
            if (hostPID ?? 0) <= 0 { hostPID = Int(window.pid) }
            return
        }
        guard let bounds = hostWindowBounds, !snapshots.isEmpty else { return }
        // 帧里已经带了 pid（引擎自建会话）时按 pid 限定候选：同 bundle 的另一个 Chrome
        // 实例可能恰好有一扇同样大小的窗。
        let matched = ViewerWindowVisibility.matchingWindow(
            bounds: bounds,
            windows: snapshots,
            pid: (hostPID ?? 0) > 0 ? pid_t(hostPID ?? 0) : nil
        )
        guard let window = matched else { return }
        hostWindowNumber = window.number
        guard (hostPID ?? 0) <= 0 else { return }
        hostPID = Int(window.pid)
        NSLog(
            "uws-viewer host resolved from window bounds: pid=%d window=%d bounds=%@",
            window.pid, window.number, NSStringFromRect(window.frame)
        )
    }

    // MARK: - Focus discipline (R1 + R4b)

    private func installFocusObservers() {
        // Clicks that land in *other* applications. Our own panels use
        // non-activating windows, so a click inside the shell never reaches a
        // global monitor; the containment check below is defence in depth.
        globalClickMonitor = NSEvent.addGlobalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown, .otherMouseDown]
        ) { [weak self] _ in
            self?.handleFocusLoss(.clickOutsideShell, at: NSEvent.mouseLocation)
        }
        // Clicks on a window of an application that is already frontmost do not
        // change the active application, so the click monitor above and this
        // notification are complementary, not redundant.
        let center = NSWorkspace.shared.notificationCenter
        let observer = center.addObserver(
            forName: NSWorkspace.didActivateApplicationNotification, object: nil, queue: .main
        ) { [weak self] notification in
            guard let application = notification.userInfo?[NSWorkspace.applicationUserInfoKey] as? NSRunningApplication,
                  application.processIdentifier != ProcessInfo.processInfo.processIdentifier
            else { return }
            self?.handleFocusLoss(.otherApplicationActivated, at: nil)
        }
        focusObservers.append(observer)
    }

    /// 壳自己的窗口矩形：落在壳里的点击不是「用户切走了」。
    private var shellFrames: [CGRect] {
        [expandedPanel.frame, compactPanel.frame]
    }

    private func handleFocusLoss(_ loss: ViewerFocusLoss, at point: CGPoint?) {
        guard presentation == .expanded else { return }
        if loss == .clickOutsideShell,
           let point,
           ViewerFocusGuard.isInsideShell(point, frames: shellFrames) {
            return
        }
        guard focusGuard.collapseReason(for: loss, at: now()) != nil else { return }
        collapse(animated: true)
    }

    // MARK: - Content

    public func display(frame image: NSImage) {
        currentImage = image
        canvas.display(frame: image)
        expandedCanvas.display(frame: image)
    }

    public func display(jpeg: Data) {
        guard let image = NSImage(data: jpeg) else { return }
        display(frame: image)
    }

    public func setAction(_ text: String?) {
        canvas.setAction(text)
        expandedCanvas.setAction(text)
    }

    /// Live session metadata: the engine rewrites title/URL as the agent
    /// navigates, and every shell showing that session follows.
    public func update(session: ViewerSession) {
        if state.session.id != session.id {
            // 换绑：上一块的「谁是真窗口」不能再算数——否则新会话会拿旧 pid 判可见性。
            hostPID = nil
            hostWindowNumber = nil
            hostWindowBounds = nil
        }
        state.session = session
        canvas.update(session: session)
        expandedCanvas.update(session: session)
        expandedPanel.title = session.pageTitle.isEmpty ? "UWS Viewer" : session.pageTitle
    }

    public func showVirtualCursor(x: CGFloat, y: CGFloat, clicked: Bool) {
        canvas.showVirtualCursor(x: x, y: y, clicked: clicked)
        expandedCanvas.showVirtualCursor(x: x, y: y, clicked: clicked)
    }

    // MARK: - Presentation

    /// `orderFrontRegardless` would violate R1 by raising the app. This panel
    /// explicitly cannot become key or main; `orderFront` still needs
    /// real-machine verification against the host application's activation policy.
    public func showCompact() {
        presentation = .compact
        compactPanel.orderFront(nil)
    }

    public func expand(animated: Bool = true) {
        guard presentation == .compact else { return }
        presentation = .expanded
        focusGuard.noteExpanded(at: now())

        let visible = compactPanel.screen?.visibleFrame ?? NSScreen.main?.visibleFrame ?? compactPanel.frame
        let compactFrame = compactPanel.frame
        slot.noteProgrammaticFrame(compactFrame)
        let target = ViewerGeometry.expandedFrame(from: compactFrame, inVisibleFrame: visible)

        compactPanel.orderOut(nil)
        expandedPanel.setFrame(compactFrame, display: true)
        // R1 holds in both presentations: the shell is raised with `orderFront`
        // only, never `makeKeyAndOrderFront` and never `NSApp.activate`, so the
        // user keeps typing in whatever application already had focus.
        expandedPanel.orderFront(nil)

        guard animated else {
            expandedPanel.setFrame(target, display: true)
            return
        }
        NSAnimationContext.runAnimationGroup { context in
            context.duration = ViewerGeometry.expandDuration
            context.timingFunction = CAMediaTimingFunction(name: .easeOut)
            expandedPanel.animator().setFrame(target, display: true)
        }
    }

    public func collapse(animated: Bool = true) {
        guard presentation == .expanded else { return }
        presentation = .compact
        focusGuard.noteCollapsed()

        let visible = expandedPanel.screen?.visibleFrame ?? NSScreen.main?.visibleFrame ?? compactPanel.frame
        let target = slot.frame

        guard animated else {
            expandedPanel.orderOut(nil)
            compactPanel.setFrame(target, display: true)
            compactPanel.orderFront(nil)
            return
        }
        NSAnimationContext.runAnimationGroup({ context in
            context.duration = 0.20
            context.timingFunction = CAMediaTimingFunction(name: .easeIn)
            expandedPanel.animator().setFrame(target, display: true)
        }, completionHandler: { [weak self] in
            guard let self else { return }
            self.expandedPanel.orderOut(nil)
            self.compactPanel.setFrame(target, display: true)
            self.compactPanel.orderFront(nil)
        })
    }

    /// 引擎确认把那扇真窗口抬到最前了：小窗播一段「飞向它」的动画，然后消失
    /// （2026-09-22 用户口径）。
    ///
    /// 动画落点就是引擎回执里的**真实矩形** `target` 本身（`ViewerGeometry.flyAwayDestination`）：
    /// 小窗的四个角**向外**飞到那扇真窗口的四个角（2026-09-24 用户口径；此前把落点算成
    /// 「中心对齐 + 缩到一半」，用户看到的是一段缩小过程）。演完就 `orderOut` 并把窗口搬回
    /// 自己的槽位、`alpha` 复位——小窗只是让位，不是被销毁（浏览器让开前台时 `pollPresence`
    /// 会把它亮回来）。`hiddenByRealWindow` 就是这笔账：这一下之后屏幕归真窗口，直到它看不见。
    ///
    /// 动画期间一律算程序化移动：`snapCompactPanel` 的吸附不能和动画抢方向盘。
    /// 全程只插值 `frame` 与 `alpha`：不测量、不查 CDP，时长按「嗖的一下」定
    /// （`ViewerGeometry.expandDuration`）。
    /// - Parameter raiseAtEnd: 动画**末段**（结束前 `ViewerGeometry.raiseLeadTime`）要做的那一下：
    ///   「把真窗口抬到前台」。2026-09-24 第二轮现场口径：先抬窗口再播动画 = 窗口先到、动画后到，
    ///   两段脱节；所以抬升由这里定时，收回/打断时 `cancelPendingRaise()` 把它取消。
    ///   **它跑在 `raiseQueue` 上（非主线程）**：只能做「发意图」这类不碰 AppKit 的事，要动
    ///   窗口/应用就自己丢回主线程——否则又会被主线程的解码队列推迟（见 `raiseQueue`）。
    public func flyAwayAndHide(
        toward target: CGRect,
        raiseAtEnd: (() -> Void)? = nil,
        completion: (() -> Void)? = nil
    ) {
        guard presentation == .compact else {
            completion?()
            return
        }
        snapWorkItem?.cancel()
        snapWorkItem = nil
        cancelPendingRaise()
        isProgrammaticMove = true
        if let raiseAtEnd {
            let item = DispatchWorkItem(block: raiseAtEnd)
            raiseWorkItem = item
            raiseQueue.asyncAfter(deadline: .now() + ViewerGeometry.raiseDelay, execute: item)
        }

        let start = compactPanel.frame
        let home = slot.frame
        let destination = ViewerGeometry.flyAwayDestination(from: start, toward: target)
        compactPanel.alphaValue = 1
        // R1：全程不 makeKeyAndOrderFront、不 NSApp.activate —— 引擎刚把那扇真窗口激活，
        // 我们不能把它抢回来。这里只动自己的面板。
        NSAnimationContext.runAnimationGroup({ context in
            context.duration = ViewerGeometry.expandDuration
            context.timingFunction = CAMediaTimingFunction(name: .easeOut)
            compactPanel.animator().setFrame(destination, display: true)
            compactPanel.animator().alphaValue = 0
        }, completionHandler: { [weak self] in
            guard let self else { return }
            self.compactPanel.orderOut(nil)
            self.compactPanel.setFrame(home, display: false)
            self.compactPanel.alphaValue = 1
            self.isProgrammaticMove = false
            self.hiddenByRealWindow = true
            self.cancelPendingRaise()
            completion?()
        })
    }

    /// 用户按下小窗的关闭键。
    private func dismiss() {
        hide()
        onDismiss?()
    }

    /// 这个会话的面板此刻在屏幕上吗（重新出现前用它判断要不要再亮一次）。
    public var isVisible: Bool { compactPanel.isVisible || expandedPanel.isVisible }

    /// 被用户关掉之后又有了新动作：把面板放回它原来的槽位。
    ///
    /// 两道闸：这块面板现在是「因为真窗口看得见才藏起来的」就不亮（等 `pollPresence`
    /// 在真窗口看不见时亮）；`canShow` 说不该存在（用户 ✕ 掉、没绑会话）也不亮。
    public func show() {
        guard !hiddenByRealWindow else { return }
        guard canShow?() ?? true else { return }
        switch presentation {
        case .compact: showCompact()
        case .expanded: expandedPanel.orderFront(nil)
        }
    }

    /// 收回/打断时把「末段抬升」取消掉：动画没演完就不是用户要的那次展开。
    public func cancelPendingRaise() {
        raiseWorkItem?.cancel()
        raiseWorkItem = nil
    }

    public func hide() {
        cancelPendingRaise()
        hiddenByRealWindow = false
        suppressPresenceHide = false
        compactPanel.alphaValue = 1
        compactPanel.orderOut(nil)
        expandedPanel.orderOut(nil)
    }

    /// Demo/evidence hook: synthesises a real left click through the compact
    /// canvas so a recorded expansion exercises the actual user-gesture path
    /// (mouseDown/mouseUp on the real view), not a private shortcut.
    public func simulateCompactClick() {
        let point = canvas.convert(CGPoint(x: canvas.bounds.midX, y: canvas.bounds.midY), to: nil)
        guard let window = canvas.window,
              let down = NSEvent.mouseEvent(
                  with: .leftMouseDown, location: point, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                  windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: 1, pressure: 1
              ),
              let up = NSEvent.mouseEvent(
                  with: .leftMouseUp, location: point, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime + 0.02,
                  windowNumber: window.windowNumber, context: nil, eventNumber: 1, clickCount: 1, pressure: 0
              )
        else { return }
        canvas.mouseDown(with: down)
        canvas.mouseUp(with: up)
    }

    // MARK: - Dragging

    /// 用户一抓住面板就停掉可能还在跑的吸附动画：否则那个动画会继续改窗口位置，
    /// 和手指抢方向盘（2026-09-12 用户现场反馈「拖动时位置在和我抗争」）。
    private func beginUserDrag() {
        snapWorkItem?.cancel()
        snapWorkItem = nil
        isUserDragging = true
        guard isProgrammaticMove else { return }
        isProgrammaticMove = false
        NSAnimationContext.beginGrouping()
        NSAnimationContext.current.duration = 0
        compactPanel.setFrame(compactPanel.frame, display: false)
        NSAnimationContext.endGrouping()
    }

    /// 拖完了才谈吸附：拖动过程中一律不吸附。
    private func endUserDrag() {
        isUserDragging = false
        // 松手这一下位置就已经是用户的了：**先记账**，别等 0.25s 后的吸附（吸附可能再把它
        // 挪到边距上，那时 `snapCompactPanel` 会再记一次）。规范 §4.4 第 9 条。
        slot.noteUserDrop(compactPanel.frame, inVisibleFrame: visibleFrameForSnapping)
        scheduleEdgeSnap()
    }

    /// 摆小窗时用的可见区（吸附/夹取都以它为准）。
    private var visibleFrameForSnapping: CGRect {
        compactPanel.screen?.visibleFrame ?? NSScreen.main?.visibleFrame ?? compactPanel.frame
    }

    private func scheduleEdgeSnap() {
        snapWorkItem?.cancel()
        let work = DispatchWorkItem { [weak self] in self?.snapCompactPanel() }
        snapWorkItem = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25, execute: work)
    }

    private func snapCompactPanel() {
        guard presentation == .compact, !isUserDragging else { return }
        // 记账走在这句早退**之前**：落点不需要吸附时窗口不动，但槽位必须跟着用户走
        // （2026-09-24 用户现场「移动后点开、再出现时回到移动前的位置」的根因，§4.4 第 9 条）。
        let target = slot.noteUserDrop(compactPanel.frame, inVisibleFrame: visibleFrameForSnapping)
        guard target != compactPanel.frame else { return }
        isProgrammaticMove = true
        NSAnimationContext.runAnimationGroup({ context in
            context.duration = 0.16
            context.timingFunction = CAMediaTimingFunction(name: .easeOut)
            compactPanel.animator().setFrame(target, display: true)
        }, completionHandler: { [weak self] in
            self?.isProgrammaticMove = false
        })
    }

    public func windowDidMove(_ notification: Notification) {
        guard (notification.object as? NSWindow) === compactPanel,
              !isProgrammaticMove,
              !isUserDragging
        else { return }
        scheduleEdgeSnap()
    }

    public func windowShouldClose(_ sender: NSWindow) -> Bool {
        guard sender === expandedPanel else { return true }
        collapse(animated: true)
        return false
    }

    public func windowDidResize(_ notification: Notification) {
        guard (notification.object as? NSWindow) === compactPanel else { return }
        slot.noteProgrammaticFrame(compactPanel.frame)
    }
}

/// Enforces R1 at the AppKit class boundary: the compact viewer can never take
/// keyboard focus or become the main window. Expansion uses a separate,
/// explicitly user-initiated normal-window state transition.
private final class PassiveViewerPanel: NSPanel {
    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }
}

/// The expanded presentation keeps the compact sheet's focus discipline: it is
/// always on top and interactive, but it can never become key or main, so it
/// cannot steal the user's typing focus (R1). Its own controls accept first
/// mouse, so they stay usable without activation.
private final class ExpandedViewerPanel: NSPanel {
    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }
}
