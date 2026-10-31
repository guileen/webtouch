import Foundation

/// Incremental newline framing for the engine's JSON-line bridge protocol.
public struct ViewerLineBuffer {
    private var pending = Data()

    public init() {}

    public mutating func append(_ data: Data) -> [Data] {
        pending.append(data)
        var lines: [Data] = []
        while let index = pending.firstIndex(of: 0x0A) {
            let line = pending[pending.startIndex..<index]
            if !line.isEmpty { lines.append(Data(line)) }
            pending = pending[pending.index(after: index)...]
        }
        return lines
    }
}

extension ViewerBridgeUpdate {
    /// One decoder shared by the socket client and the tests. It mirrors
    /// `LocalBridgeFrameSource.receive(jsonLine:)`: protocol version 1 and a
    /// non-empty session identity are the only admission rules.
    public static func decode(jsonLine: Data) -> ViewerBridgeUpdate? {
        guard let update = try? JSONDecoder().decode(ViewerBridgeUpdate.self, from: jsonLine),
              update.version == 1,
              !update.session.id.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else { return nil }
        return update
    }
}

/// Subscribes to the engine's viewer socket (Unix domain socket, JSON lines).
///
/// The engine daemon is the listener; the viewer is a client. That order means
/// the shell can be started or quit at any time without the engine noticing:
/// with no subscriber, frames simply are not read. Reconnection is automatic
/// and cheap, because the socket may appear later than the viewer.
public final class ViewerSocketClient {
    public enum State: Equatable, Sendable {
        case idle
        case connecting
        case connected
        case retrying(after: TimeInterval)
    }

    public var onUpdate: ((ViewerBridgeUpdate) -> Void)?
    /// 引擎对一条意图的回执（展开/收回到底成没成）。
    public var onIntentResult: ((ViewerIntentResult) -> Void)?
    public var onStateChange: ((State) -> Void)?

    /// `UWS_VIEWER_SOCKET` wins; otherwise the engine default `~/.uws/viewer.sock`.
    public static var defaultPath: String {
        if let path = ProcessInfo.processInfo.environment["UWS_VIEWER_SOCKET"], !path.isEmpty {
            return path
        }
        return FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".uws/viewer.sock")
            .path
    }

    public let path: String

    private let queue = DispatchQueue(label: "uws.viewer.socket")
    private let lock = NSLock()
    /// 写方向独立加锁：一个意图必须整行出去，不能和别的写交叉。
    private let writeLock = NSLock()
    private var descriptor: Int32 = -1
    private var running = false
    private var retryDelay: TimeInterval = 1.0

    public init(path: String = ViewerSocketClient.defaultPath) {
        self.path = path
    }

    public func start() {
        lock.lock()
        guard !running else { lock.unlock(); return }
        running = true
        lock.unlock()
        emit(.connecting)
        queue.async { [weak self] in self?.run() }
    }

    public func stop() {
        lock.lock()
        running = false
        let open = descriptor
        lock.unlock()
        if open >= 0 { shutdown(open, SHUT_RDWR) }
    }

    /// 把一条意图写给引擎。返回 false 表示此刻没有连接（引擎会在壳重连后照常推流，
    /// 一条没发出去的意图只会让这次点击回落到自绘放大面板，不会留下半条消息）。
    @discardableResult
    public func send(_ intent: ViewerSocketIntent) -> Bool {
        let line = intent.encodedLine()
        guard !line.isEmpty else { return false }
        lock.lock()
        let fd = descriptor
        lock.unlock()
        guard fd >= 0 else { return false }
        writeLock.lock()
        defer { writeLock.unlock() }
        return line.withUnsafeBytes { buffer -> Bool in
            guard let base = buffer.baseAddress else { return false }
            var written = 0
            while written < buffer.count {
                let count = write(fd, base.advanced(by: written), buffer.count - written)
                if count > 0 {
                    written += count
                    continue
                }
                if count < 0 && errno == EINTR { continue }
                return false
            }
            return true
        }
    }

    // MARK: - Loop

    private var isRunning: Bool {
        lock.lock(); defer { lock.unlock() }
        return running
    }

    private func run() {
        while isRunning {
            guard let connected = connectSocket() else {
                emit(.retrying(after: retryDelay))
                Thread.sleep(forTimeInterval: retryDelay)
                retryDelay = min(retryDelay * 1.6, 5)
                continue
            }
            lock.lock(); descriptor = connected; lock.unlock()
            retryDelay = 1.0
            emit(.connected)
            readLoop(connected)
            close(connected)
            lock.lock(); descriptor = -1; lock.unlock()
        }
        emit(.idle)
    }

    private func readLoop(_ fd: Int32) {
        var buffer = [UInt8](repeating: 0, count: 1 << 16)
        var lines = ViewerLineBuffer()
        while isRunning {
            let count = read(fd, &buffer, buffer.count)
            if count > 0 {
                for line in lines.append(Data(bytes: buffer, count: count)) {
                    guard let message = ViewerSocketMessage.decode(jsonLine: line) else { continue }
                    DispatchQueue.main.async { [weak self] in
                        switch message {
                        case .update(let update): self?.onUpdate?(update)
                        case .intentResult(let result): self?.onIntentResult?(result)
                        }
                    }
                }
                continue
            }
            if count == 0 { return }
            if errno == EINTR { continue }
            return
        }
    }

    private func emit(_ state: State) {
        DispatchQueue.main.async { [weak self] in self?.onStateChange?(state) }
    }

    private func connectSocket() -> Int32? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        var address = sockaddr_un()
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        address.sun_family = sa_family_t(AF_UNIX)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        let bytes = Array(path.utf8)
        guard bytes.count < capacity else { close(fd); return nil }
        withUnsafeMutablePointer(to: &address.sun_path) { pointer in
            pointer.withMemoryRebound(to: CChar.self, capacity: capacity) { destination in
                for (index, byte) in bytes.enumerated() { destination[index] = CChar(bitPattern: byte) }
                destination[bytes.count] = 0
            }
        }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0 else { close(fd); return nil }
        return fd
    }
}
