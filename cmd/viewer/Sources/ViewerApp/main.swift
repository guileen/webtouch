import AppKit
import ViewerCore

/// Drives the prototype without a producer: a smooth virtual-cursor path plus
/// periodic clicks, so the shell can be judged visually before any screencast
/// transport exists.
final class DemoDriver {
    private let controller: ViewerPanelController
    private var timer: Timer?
    private var lastTick = Date()
    private var elapsed: TimeInterval = 0
    private var nextClickAt: TimeInterval = 3.4
    private var nextActionAt: TimeInterval = 2.6
    private var pressedUntil: TimeInterval = 0
    private var actionIndex = 0

    private let actions = [
        "等待页面加载…",
        "正在载入章节 1…",
        "正在滚动到第 2 屏",
        "正在输入搜索词",
        "在「Register」上悬停",
    ]

    init(controller: ViewerPanelController) {
        self.controller = controller
    }

    func start() {
        lastTick = Date()
        let timer = Timer(timeInterval: 1.0 / 30.0, repeats: true) { [weak self] _ in self?.tick() }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    private func tick() {
        let now = Date()
        elapsed += now.timeIntervalSince(lastTick)
        lastTick = now

        let x = 0.5 + 0.34 * sin(elapsed * 0.62)
        let y = 0.46 + 0.24 * sin(elapsed * 1.07 + 1.1)
        let point = CGPoint(x: x, y: y)
        controller.canvas.moveCursor(to: point, animated: true)
        controller.expandedCanvas.moveCursor(to: point, animated: true)

        if elapsed >= nextClickAt {
            nextClickAt = elapsed + 3.4
            pressedUntil = elapsed + 0.14
            controller.canvas.showVirtualCursor(x: x, y: y, clicked: true)
            controller.expandedCanvas.showVirtualCursor(x: x, y: y, clicked: true)
        }

        let pressed = elapsed < pressedUntil
        controller.canvas.setCursorPressed(pressed)
        controller.expandedCanvas.setCursorPressed(pressed)

        if elapsed >= nextActionAt {
            nextActionAt = elapsed + 2.6
            actionIndex = (actionIndex + 1) % actions.count
            controller.setAction(actions[actionIndex])
        }
    }
}

/// Which frame path this launch uses: the engine socket (real Chrome pages) or
/// the built-in synthetic demo page.
enum ViewerAppMode {
    case demo
    case socket

    static func resolve(_ environment: [String: String], fileManager: FileManager = .default) -> ViewerAppMode {
        switch environment["UWS_VIEWER_MODE"]?.lowercased() {
        case "demo": return .demo
        case "socket", "live": return .socket
        default: break
        }
        if let path = environment["UWS_VIEWER_SOCKET"], !path.isEmpty { return .socket }
        // Auto-detect a running engine, so starting the app "just works" once
        // the engine is up while `UWS_VIEWER_MODE=demo` stays deterministic for
        // visual evidence capture.
        return fileManager.fileExists(atPath: ViewerSocketClient.defaultPath) ? .socket : .demo
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    private var controller: ViewerPanelController?
    private var driver: DemoDriver?
    private var host: ViewerHostController?
    private var client: ViewerSocketClient?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let environment = ProcessInfo.processInfo.environment
        if ViewerAppMode.resolve(environment) == .socket {
            startSocketMode(environment)
        } else {
            startDemoMode(environment)
        }
    }

    // MARK: - Live engine frames

    /// One shell per UWS session, fed by the engine's JSON-line socket. This is
    /// the production path: real Chrome renders, the engine reports frames,
    /// pointer transitions and redacted action labels, and the viewer only draws.
    private func startSocketMode(_ environment: [String: String]) {
        let host = ViewerHostController()
        // Evidence-only hook: synthesise one real click on the first attached
        // session so a recorded expansion exercises the true mouseDown/mouseUp
        // path instead of a private shortcut. Product path never sets this.
        let clickDelay = Double(environment["UWS_VIEWER_AUTOCLICK_AFTER"] ?? "")
        var clickedOnce = false
        host.onRegister = { sessionID, controller in
            NSLog("uws-viewer session attached: %@", sessionID)
            guard let delay = clickDelay, delay > 0, !clickedOnce else { return }
            clickedOnce = true
            DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak controller] in
                controller?.simulateCompactClick()
            }
        }
        let path = environment["UWS_VIEWER_SOCKET"].flatMap { $0.isEmpty ? nil : $0 } ?? ViewerSocketClient.defaultPath
        let client = ViewerSocketClient(path: path)
        client.onUpdate = { [weak host] update in host?.apply(update: update) }
        client.onIntentResult = { [weak host] result in host?.apply(intentResult: result) }
        // 反向边：用户点小窗 → 意图发给引擎 → 引擎把这个会话的真窗口搬到展开几何。
        host.onIntent = { [weak client] intent in client?.send(intent) ?? false }
        client.onStateChange = { [weak host] state in
            NSLog("uws-viewer socket %@ (%@)", String(describing: state), path)
            // 断线必须被「说出来」：停在最后一帧的画面上，用户分不清是卡住还是刚断。
            host?.setConnectionState(state)
        }
        client.start()
        self.host = host
        self.client = client
        NSLog("uws-viewer live mode: waiting for engine frames on %@", path)
    }

    // MARK: - Synthetic demo page

    private func startDemoMode(_ environment: [String: String]) {
        let session = ViewerSession(
            id: environment["UWS_VIEWER_SESSION"] ?? "demo-session",
            agentName: environment["UWS_VIEWER_AGENT"] ?? "Demo Agent",
            pageTitle: environment["UWS_VIEWER_TITLE"] ?? "Chapter 1",
            url: environment["UWS_VIEWER_URL"] ?? "https://example.test/chapter-1",
            action: "等待页面加载…"
        )
        let state = ViewerState(session: session)
        let controller = ViewerPanelController(state: state)
        state.onIntent = { [weak controller] intent in
            switch intent {
            case .requestExpansion:
                controller?.expand(animated: true)
            case .requestCollapse:
                controller?.collapse(animated: true)
            }
        }

        controller.display(frame: DemoPage.image())
        controller.showCompact()
        if let delay = Double(environment["UWS_VIEWER_AUTOCLICK_AFTER"] ?? ""), delay > 0 {
            DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak controller] in
                controller?.simulateCompactClick()
            }
        }
        self.controller = controller

        if environment["UWS_VIEWER_ANIMATE"] != "0" {
            let driver = DemoDriver(controller: controller)
            driver.start()
            self.driver = driver
        }
        NSLog(
            "uws-viewer-prototype ready (compact %.0fx%.0f, fullSizeContentView expanded)",
            ViewerGeometry.compactSize.width,
            ViewerGeometry.compactSize.height
        )
    }
}

/// 单实例守卫（2026-09-15 用户定案：小窗常驻）。
///
/// 观看壳一块屏上只应有一套面板：多会话共用一块、跟随最近活跃的会话切换视图
/// （2026-09-22 现场批次 #2；「多任务多面板并排」是退役的旧说法）。Agent 每次开工
/// 都会 `uws viewer app` 顺手拉一次，重复拉起必须**复用**已在跑的壳，而不是叠出
/// 第二套小窗。约定与 Go 侧 `uws viewer app` 一致：与 socket 同目录的 `viewer.lock`
/// 上打 `flock`；锁随进程存活，进程退出即释放。
///
/// 拿不到锁文件时不拦（宁可多开也不能不显示）；demo 模式（可视化取证录制）不参与
/// 互斥，保证证据可重复采集。
func acquireViewerSingleInstanceLock(socketPath: String) -> Bool {
    let lockURL = URL(fileURLWithPath: socketPath)
        .deletingLastPathComponent()
        .appendingPathComponent("viewer.lock")
    try? FileManager.default.createDirectory(
        at: lockURL.deletingLastPathComponent(), withIntermediateDirectories: true)
    let fd = open(lockURL.path, O_CREAT | O_RDWR, 0o600)
    guard fd >= 0 else { return true }
    if flock(fd, LOCK_EX | LOCK_NB) != 0 {
        close(fd)
        return false
    }
    return true
}

let launchEnvironment = ProcessInfo.processInfo.environment
if ViewerAppMode.resolve(launchEnvironment) == .socket {
    let socketPath = launchEnvironment["UWS_VIEWER_SOCKET"].flatMap { $0.isEmpty ? nil : $0 }
        ?? ViewerSocketClient.defaultPath
    if !acquireViewerSingleInstanceLock(socketPath: socketPath) {
        NSLog("uws-viewer: 已有观看壳在运行（单实例锁），本次启动直接退出")
        exit(0)
    }
}

let application = NSApplication.shared
let delegate = AppDelegate()
application.delegate = delegate
application.setActivationPolicy(.accessory)
application.run()
