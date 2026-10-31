import CoreGraphics
import Foundation

/// 观看壳 ↔ 引擎的反向协议（v1，2026-09-12）。
///
/// 一根 Unix socket 上现在有两个方向：
///
///   引擎 → 壳：帧更新（`ViewerBridgeUpdate`）
///   壳 → 引擎：用户意图（`ViewerSocketIntent`）+ 引擎回执（`ViewerIntentResult`）
///
/// 意图不是自动化输入：它只说「用户要把这个会话的页面放大看」或「用户切回自己的
/// 工作了」。引擎据此搬动**这个会话自己的**真窗口——展开态要的是真页面（能点、能滚、
/// 能过人机验证），不是一张放大的图片。

/// 一扇真窗口的位置与大小，坐标空间与 Chrome 的 `Browser.setWindowBounds` 一致：
/// DIP，原点是主屏左上角，y 向下增长。
public struct ViewerWindowBounds: Codable, Equatable, Sendable {
    public let left: Int
    public let top: Int
    public let width: Int
    public let height: Int

    public init(left: Int, top: Int, width: Int, height: Int) {
        self.left = left
        self.top = top
        self.width = width
        self.height = height
    }

    public var rect: CGRect {
        CGRect(x: CGFloat(left), y: CGFloat(top), width: CGFloat(width), height: CGFloat(height))
    }
}

/// 壳要的展开尺寸。壳只说「要多大」，不说「放哪儿」：绝对屏幕坐标要跨过 AppKit
/// （左下原点、多屏排布、独立 Space）和 Chrome 窗口坐标（主屏左上原点、y 向下）两套
/// 空间，2026-09-12 真机复跑就是按壳给的坐标把窗口搬到另一块屏的上方，用户看到一扇
/// 点不动的窗。引擎自己知道那扇窗在哪儿，原地放大最稳。
public struct ViewerWindowSize: Codable, Equatable, Sendable {
    public let width: Int
    public let height: Int

    public init(width: Int, height: Int) {
        self.width = width
        self.height = height
    }
}

/// 引擎窗口坐标（主屏左上原点、y 向下）→ AppKit 屏幕坐标（左下原点、y 向上）的纯换算。
/// 只用在回执上：壳拿引擎实际生效的矩形做「用户点的是不是这扇真页面」的落点判定。
public enum ViewerScreenGeometry {
    public static func appKitFrame(from bounds: ViewerWindowBounds, primaryScreenHeight: CGFloat) -> CGRect {
        CGRect(
            x: CGFloat(bounds.left),
            y: primaryScreenHeight - CGFloat(bounds.top + bounds.height),
            width: CGFloat(bounds.width),
            height: CGFloat(bounds.height)
        )
    }
}

/// 一条 壳 → 引擎 的意图。
public struct ViewerSocketIntent: Codable, Equatable, Sendable {
    public enum Name: String, Codable, Sendable {
        case expand
        /// 动画**末段**：壳说「现在把这扇真窗口抬到前台」。`expand` 带了 `deferRaise` 时，
        /// 引擎那一趟只摆窗口不激活，这一条就是补上的激活那一步（2026-09-24 口径 §4.4 第 3 条）。
        case raise
        case collapse
        /// 用户点了小窗上的 ✕：壳已经真丢掉了这块面板，引擎据此把这个会话静默回收
        /// （停推流、保留会话与真窗口）。它不搬动任何窗口。
        case close
    }

    public let version: Int
    public let kind: String
    public let intent: Name
    public let session: String
    public let size: ViewerWindowSize?
    /// 只有 `expand` 会带：壳要「先把动画飞完、末段再抬」，引擎这一趟就只摆窗口、不激活
    /// （2026-09-24 第二轮现场口径：窗口先到前台、小窗动画后到＝两段脱节）。
    public let deferRaise: Bool?

    public init(intent: Name, session: String, size: ViewerWindowSize? = nil, deferRaise: Bool? = nil) {
        self.version = 1
        self.kind = "intent"
        self.intent = intent
        self.session = session
        self.size = size
        self.deferRaise = deferRaise
    }

    /// 一行 JSON（含换行）：引擎按 JSON lines 读。
    public func encodedLine() -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys] // 字节序稳定，便于取证与跨语言对拍
        guard let payload = try? encoder.encode(self) else { return Data() }
        return payload + Data([0x0A])
    }
}

/// 引擎对一条意图的回执。`ok=false` 也必须回——壳据此回落到自绘放大面板，而不是
/// 一直等在一个没有答案的状态里（「点开没反应」正是用户现场最不能接受的那种失败）。
public struct ViewerIntentResult: Codable, Equatable, Sendable {
    public let version: Int
    public let kind: String
    public let session: String
    public let intent: String
    public let ok: Bool
    public let reason: String?
    public let windowBounds: ViewerWindowBounds?
    public let browserPID: Int?
}

/// 一根 socket 上可能出现的两种包。
public enum ViewerSocketMessage: Equatable, Sendable {
    case update(ViewerBridgeUpdate)
    case intentResult(ViewerIntentResult)

    public static func decode(jsonLine: Data) -> ViewerSocketMessage? {
        struct Envelope: Decodable { let kind: String? }
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: jsonLine) else { return nil }
        if envelope.kind == "intentAck" {
            guard let ack = try? JSONDecoder().decode(ViewerIntentResult.self, from: jsonLine) else { return nil }
            return .intentResult(ack)
        }
        guard let update = ViewerBridgeUpdate.decode(jsonLine: jsonLine) else { return nil }
        return .update(update)
    }
}

/// 「直播小窗」与「真窗口」的互斥判定（2026-09-22 口径；**2026-09-24 第二轮改判据**）。
///
/// 用户点小窗 = 把那扇真窗口抬到最前。此后屏幕上任一时刻只能有一个。判据在第二轮从
/// **焦点**改成了**可见性**（规范 §4.4 第 5 条）：真窗口**你看得见**（在屏上、没被挡到
/// 可见面积 < 60%）就让位，看不看得见与前台无关——真窗口只是失焦但仍然明明白白摆在屏幕上
/// 时，再叠一个小窗只是重复遮挡。
///
/// 旧口径（「前台是浏览器 → 隐藏」）仍在，但降级为**兜底**：拿不到窗口列表、或附着模式
/// 没有 `browserPID` 时用它（`givesWay`）。
///
/// 刻意保持 AppKit 无关，规则本身可以在没有窗口服务器的环境里单测。
public enum ViewerPanelPresence {
    /// 哪些应用算「这个会话的浏览器」。
    ///
    /// 引擎拉起的窗口能按 `hostPID` 精确对上（帧更新与 `intentAck` 都带 `browserPID`）；
    /// 附着模式（CDP 附到用户自己开的 Chrome）没有这个 pid，只能按应用类别认。代价是
    /// 「附着模式认不准」：用户自己切到 Chrome 也能让小窗消失——这是刻意选的偏向，
    /// 宁可多藏一次小窗，也不能让「真窗口在前台时小窗还压在它上面」（互斥不变量）。
    public static let browserBundleIdentifiers: Set<String> = [
        "com.google.Chrome",
        "com.google.Chrome.beta",
        "com.google.Chrome.canary",
        "org.chromium.Chromium",
        "com.brave.Browser",
        "com.microsoft.edgemac",
    ]
    public static let browserNameHints = ["Chrome", "Chromium", "Brave", "Edge"]

    /// 前台这个应用是不是承载本会话页面的浏览器。
    public static func isHostBrowser(
        frontmostPID: pid_t?,
        hostPID: Int?,
        bundleIdentifier: String?,
        localizedName: String?
    ) -> Bool {
        if let hostPID, hostPID > 0 {
            // 引擎拉起的窗口：pid 说了算，别的应用（哪怕也叫 Chrome）不算。
            return frontmostPID.map { pid_t(hostPID) == $0 } ?? false
        }
        if let bundleIdentifier, browserBundleIdentifiers.contains(bundleIdentifier) {
            return true
        }
        if let localizedName {
            return browserNameHints.contains { localizedName.localizedCaseInsensitiveContains($0) }
        }
        return false
    }

    /// 前台轮询的决策。
    public enum Decision: Equatable {
        case unchanged
        /// 浏览器在前台：小窗必须消失（互斥不变量）。
        case hide
        /// 浏览器让开了：把「因为浏览器在台前才藏起来的那块」亮回来。
        case show
    }

    /// 该不该让位给真窗口：看得见就让位；`nil`（算不出来）退回焦点判据。
    public static func givesWay(realWindowVisible: Bool?, browserFrontmost: Bool) -> Bool {
        realWindowVisible ?? browserFrontmost
    }

    /// `hiddenByRealWindow` 是壳自己记的账：**只**把它藏起来的那块亮回来——用户 ✕ 掉的
    /// 小窗、没绑会话的面板（`canShow`）都不该被这轮询复活。
    public static func decide(
        realWindowVisible: Bool?,
        browserFrontmost: Bool,
        panelVisible: Bool,
        hiddenByRealWindow: Bool
    ) -> Decision {
        if givesWay(realWindowVisible: realWindowVisible, browserFrontmost: browserFrontmost) {
            return panelVisible ? .hide : .unchanged
        }
        return hiddenByRealWindow ? .show : .unchanged
    }

    /// 旧签名（只有焦点判据）＝ `realWindowVisible` 缺席时的同一条规则，留着是为了
    /// 让兜底路径与既有断言共用一份真值。
    public static func decide(
        browserFrontmost: Bool,
        panelVisible: Bool,
        hiddenByBrowserFront: Bool
    ) -> Decision {
        decide(
            realWindowVisible: nil,
            browserFrontmost: browserFrontmost,
            panelVisible: panelVisible,
            hiddenByRealWindow: hiddenByBrowserFront
        )
    }
}
