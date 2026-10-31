import Foundation

/// A passive page-only frame delivered to the native Viewer. `jpeg` comes from
/// the existing UWS screencast slot; the Viewer owns any image decoding and
/// overlay coordinate normalization.
public struct ViewerFrame: Equatable, Sendable {
    public let session: ViewerSession
    public let jpeg: Data
    public let metadata: ViewerFrameMetadata
    public let cdpSessionID: String
    public let screencastSessionID: Int64
    public let targetID: String
    public let pointer: ViewerPointer?
    public let action: ViewerAction?

    public init(session: ViewerSession, jpeg: Data, metadata: ViewerFrameMetadata, cdpSessionID: String, screencastSessionID: Int64, targetID: String, pointer: ViewerPointer? = nil, action: ViewerAction? = nil) {
        self.session = session
        self.jpeg = jpeg
        self.metadata = metadata
        self.cdpSessionID = cdpSessionID
        self.screencastSessionID = screencastSessionID
        self.targetID = targetID
        self.pointer = pointer
        self.action = action
    }
}

/// P1's smallest native integration seam. It is receive-only: no Chrome, CDP,
/// socket, screen-capture, GUI activation, or input-forwarding lives here.
public protocol FrameSource: AnyObject {
    var onFrame: ((ViewerFrame) -> Void)? { get set }
    func start()
    func stop()
}

/// P1's deterministic stand-in for a future page frame stream.
public final class PlaceholderFrameSource: FrameSource {
    public var onFrame: ((ViewerFrame) -> Void)?

    public init() {}

    public func start() {}
    public func stop() {}

    public func publish(_ frame: ViewerFrame) {
        onFrame?(frame)
    }
}

/// The immutable JSON-line contract emitted by `browser.ViewerBridge`.
/// `frame` is optional so a producer may report redacted action state before a
/// screencast frame exists; only packets containing a frame invoke `onFrame`.
public struct ViewerBridgeUpdate: Codable, Equatable, Sendable {
    public let version: Int
    public let sequence: UInt64
    public let session: ViewerSession
    public let frame: ViewerBridgeFrame?
    public let pointer: ViewerPointer?
    public let action: ViewerAction?
    /// 承载这个会话真页面的浏览器进程（缺省 = 未知，例如附着模式）。展开态要用它
    /// 判断用户点的是「我们那扇真页面」还是「自己的应用」。
    public let browserPID: Int?
    /// 这个会话那扇真窗口此刻的矩形（Chrome 坐标空间，与 `CGWindowList` 同一套左上原点）。
    /// 附着模式（`browserPID` 缺省）靠它在屏上**认窗 → 认进程**，而且**不等用户点小窗**
    /// ——引擎从第一帧起就随帧下发（规范 §4.4 第 7 条）。
    public let windowBounds: ViewerWindowBounds?
}

public struct ViewerBridgeFrame: Codable, Equatable, Sendable {
    public let jpeg: Data
    public let metadata: ViewerFrameMetadata
    public let cdpSessionID: String
    public let screencastSessionID: Int64
    public let targetID: String
}

public struct ViewerFrameMetadata: Codable, Equatable, Sendable {
    public let offsetTop: Double
    public let pageScaleFactor: Double
    public let deviceWidth: Int
    public let deviceHeight: Int
    public let scrollOffsetX: Double
    public let scrollOffsetY: Double
    public let timestamp: Double
}

public enum ViewerPointerKind: String, Codable, Equatable, Sendable {
    case mouseMoved
    case mousePressed
    case mouseReleased
}

public struct ViewerPointer: Codable, Equatable, Sendable {
    public let x: Double
    public let y: Double
    public let kind: ViewerPointerKind
    public let button: String?
    public let clickCount: Int?
    public let buttons: Int?
}

/// A redacted action label. It is never a command channel and must not carry
/// typed text, cookies, or page content.
public struct ViewerAction: Codable, Equatable, Sendable {
    public let kind: String
    public let summary: String?
}

/// A transport-neutral receiver for one JSON line from the local host. A later
/// Unix-domain socket or local WebSocket can feed this method, but P1 does not
/// select, create, or connect any transport.
public final class LocalBridgeFrameSource: FrameSource {
    public var onFrame: ((ViewerFrame) -> Void)?
    public var onUpdate: ((ViewerBridgeUpdate) -> Void)?

    private let lock = NSLock()
    private var running = false
    private var latestBySession = [String: ViewerBridgeUpdate]()

    public init() {}

    public func start() {
        lock.lock()
        running = true
        lock.unlock()
    }

    public func stop() {
        lock.lock()
        running = false
        lock.unlock()
    }

    @discardableResult
    public func receive(jsonLine: Data) -> Bool {
        guard let update = try? JSONDecoder().decode(ViewerBridgeUpdate.self, from: jsonLine),
              update.version == 1,
              !update.session.id.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else { return false }

        lock.lock()
        guard update.sequence > (latestBySession[update.session.id]?.sequence ?? 0) else {
            lock.unlock()
            return false
        }
        latestBySession[update.session.id] = update
        let shouldEmitFrame = running && update.frame != nil
        lock.unlock()

        onUpdate?(update)
        guard shouldEmitFrame, let frame = update.frame else { return true }
        onFrame?(ViewerFrame(
            session: update.session,
            jpeg: frame.jpeg,
            metadata: frame.metadata,
            cdpSessionID: frame.cdpSessionID,
            screencastSessionID: frame.screencastSessionID,
            targetID: frame.targetID,
            pointer: update.pointer,
            action: update.action
        ))
        return true
    }

    public func latestUpdate(sessionID: String) -> ViewerBridgeUpdate? {
        lock.lock()
        defer { lock.unlock() }
        return latestBySession[sessionID]
    }
}
