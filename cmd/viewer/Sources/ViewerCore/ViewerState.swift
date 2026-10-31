import Foundation

/// Immutable metadata displayed above a session's page image.
public struct ViewerSession: Codable, Equatable, Sendable {
    public let id: String
    public let agentName: String
    public let pageTitle: String
    public let url: String
    public let action: String?

    public init(id: String, agentName: String, pageTitle: String, url: String, action: String? = nil) {
        self.id = id
        self.agentName = agentName
        self.pageTitle = pageTitle
        self.url = url
        self.action = action
    }
}

/// The two user gestures the shell emits.
///
/// The host decides what they mean. Expansion is not "draw a bigger picture":
/// since 2026-09-12 it asks the engine to bring the session's *real* window to
/// the expanded geometry (展开态 = 真页面：能点、能滚、能过人机验证), and
/// collapse asks for it back in its slot. The shell never activates anything
/// itself — it only reports what the user did.
public enum ViewerIntent: Equatable, Sendable {
    case requestExpansion(sessionID: String)
    case requestCollapse(sessionID: String)
}

/// The engine link as the shell displays it.
///
/// `unknown` exists so the synthetic demo mode (which never opens a socket) does
/// not claim anything about a link it never had; a real socket client reports
/// through `ViewerLinkState(_:)` instead.
public enum ViewerLinkState: Equatable, Sendable {
    case unknown
    case connected
    case connecting
    case retrying(after: TimeInterval)
    case offline

    public init(_ state: ViewerSocketClient.State) {
        switch state {
        case .idle: self = .offline
        case .connecting: self = .connecting
        case .connected: self = .connected
        case .retrying(let delay): self = .retrying(after: delay)
        }
    }
}

/// What the shell says while the engine link is down.
///
/// A frozen last frame is indistinguishable from a live one, so a broken link
/// must be *said* rather than shown: otherwise the user watches a still picture
/// and waits for it (2026-09-13 评审：`.retrying` 只 emit 没人消费).
public enum ViewerConnectionLabel {
    /// `nil` means "nothing to warn about, show the normal action line".
    public static func text(for link: ViewerLinkState) -> String? {
        switch link {
        case .unknown, .connected:
            return nil
        case .connecting:
            return "正在连接引擎…"
        case .retrying(let after):
            return "引擎未连接 · \(max(1, Int(after.rounded(.down))))s 后重试"
        case .offline:
            return "引擎未连接"
        }
    }
}

/// UI-independent state. Keeping this separate lets a future FrameSource and a
/// host application reuse the component without depending on AppKit lifecycle.
public final class ViewerState: @unchecked Sendable {
    public var session: ViewerSession
    public var onIntent: ((ViewerIntent) -> Void)?

    public init(session: ViewerSession) {
        self.session = session
    }

    public func requestExpansion() {
        onIntent?(.requestExpansion(sessionID: session.id))
    }

    public func requestCollapse() {
        onIntent?(.requestCollapse(sessionID: session.id))
    }
}
