import Foundation
import XCTest
@testable import ViewerCore

/// Minimal Unix-domain-socket server used to prove the client against a real
/// socket instead of a mocked stream. It is deliberately tiny: listen, accept
/// one client, write whatever the test asks for.
final class UnixSocketTestServer {
    let path: String
    private let listenFD: Int32
    private var clientFD: Int32 = -1
    private let lock = NSLock()
    private let accepted = DispatchSemaphore(value: 0)

    init(path: String) throws {
        self.path = path
        unlink(path)
        listenFD = socket(AF_UNIX, SOCK_STREAM, 0)
        guard listenFD >= 0 else { throw NSError(domain: "test", code: 1) }
        var address = sockaddr_un()
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        address.sun_family = sa_family_t(AF_UNIX)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        let bytes = Array(path.utf8)
        guard bytes.count < capacity else { throw NSError(domain: "test", code: 2) }
        withUnsafeMutablePointer(to: &address.sun_path) { pointer in
            pointer.withMemoryRebound(to: CChar.self, capacity: capacity) { destination in
                for (index, byte) in bytes.enumerated() { destination[index] = CChar(bitPattern: byte) }
                destination[bytes.count] = 0
            }
        }
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(listenFD, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bound == 0, listen(listenFD, 4) == 0 else { throw NSError(domain: "test", code: 3, userInfo: [NSLocalizedDescriptionKey: "bind/listen failed: errno \(errno)"]) }
        Thread.detachNewThread { [weak self] in self?.acceptLoop() }
    }

    private func acceptLoop() {
        let fd = accept(listenFD, nil, nil)
        guard fd >= 0 else { return }
        lock.lock(); clientFD = fd; lock.unlock()
        accepted.signal()
    }

    func waitForConnection(timeout: TimeInterval) -> Bool {
        accepted.wait(timeout: .now() + timeout) == .success
    }

    @discardableResult
    func send(_ data: Data) -> Int {
        lock.lock(); let fd = clientFD; lock.unlock()
        guard fd >= 0 else { return -1 }
        return data.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
    }

    func stop() {
        lock.lock(); let fd = clientFD; clientFD = -1; lock.unlock()
        if fd >= 0 { close(fd) }
        close(listenFD)
        unlink(path)
    }
}

final class ViewerSocketClientTests: XCTestCase {
    /// Unix-socket `bind` is refused by the coding sandbox (errno 1). The
    /// transport is exercised for real in an unsandboxed run; inside a sandbox
    /// the socket cases skip instead of failing, so the pure cases still gate.
    private func makeServer(path: String) throws -> UnixSocketTestServer {
        do {
            return try UnixSocketTestServer(path: path)
        } catch {
            throw XCTSkip("unix sockets unavailable here: \(error)")
        }
    }

    func testLineBufferFramesPartialWrites() {
        var buffer = ViewerLineBuffer()

        XCTAssertTrue(buffer.append(Data(#"{"version":1"#.utf8)).isEmpty)
        let first = buffer.append(Data("\n{\"two\":2}\n\n".utf8))

        XCTAssertEqual(first.count, 2)
        XCTAssertEqual(String(decoding: first[0], as: UTF8.self), #"{"version":1"#)
        XCTAssertEqual(String(decoding: first[1], as: UTF8.self), #"{"two":2}"#)
    }

    func testDecodeRejectsForeignProtocolAndKeepsSessionIdentity() {
        XCTAssertNil(ViewerBridgeUpdate.decode(jsonLine: Data(#"{"version":2,"sequence":1,"session":{"id":"s"}}"#.utf8)))
        XCTAssertNil(ViewerBridgeUpdate.decode(jsonLine: Data(#"{"version":1,"sequence":1,"session":{"id":""}}"#.utf8)))
        let decoded = ViewerBridgeUpdate.decode(jsonLine: Data(packet(sequence: 7, sessionID: "agent-a").utf8))
        XCTAssertEqual(decoded?.sequence, 7)
        XCTAssertEqual(decoded?.session.id, "agent-a")
    }

    func testReceivesFramesForSeveralSessionsOverARealUnixSocket() throws {
        let path = NSTemporaryDirectory() + "uws-viewer-\(UUID().uuidString).sock"
        let server = try makeServer(path: path)
        defer { server.stop() }

        let received = expectation(description: "frames")
        received.expectedFulfillmentCount = 3
        var sessionOrder: [String] = []
        let client = ViewerSocketClient(path: path)
        client.onUpdate = { update in
            sessionOrder.append(update.session.id)
            received.fulfill()
        }
        client.start()
        XCTAssertTrue(server.waitForConnection(timeout: 5), "client never connected to \(path)")

        // Split one frame across two writes: the client must reframe, not choke.
        let third = Array(packet(sequence: 1, sessionID: "agent-c").utf8)
        server.send(Data(third[..<40]))
        server.send(Data(third[40...]) + Data("\n".utf8))
        server.send(Data((packet(sequence: 1, sessionID: "agent-a") + "\n").utf8))
        server.send(Data((packet(sequence: 1, sessionID: "agent-b") + "\n").utf8))

        wait(for: [received], timeout: 10)
        XCTAssertEqual(sessionOrder, ["agent-c", "agent-a", "agent-b"])
        client.stop()
    }

    func testReconnectsAfterTheEngineGoesAway() throws {
        let path = NSTemporaryDirectory() + "uws-viewer-\(UUID().uuidString).sock"
        let first = try makeServer(path: path)
        let received = expectation(description: "frames")
        var count = 0
        let client = ViewerSocketClient(path: path)
        client.onUpdate = { _ in
            count += 1
            if count == 2 { received.fulfill() }
        }
        client.start()
        XCTAssertTrue(first.waitForConnection(timeout: 5))
        first.send(Data((packet(sequence: 1, sessionID: "agent-a") + "\n").utf8))
        // Engine restart: drop the connection, then listen again on the same path.
        first.stop()
        Thread.sleep(forTimeInterval: 0.5)
        let second = try makeServer(path: path)
        defer { second.stop() }

        XCTAssertTrue(second.waitForConnection(timeout: 10), "client did not reconnect")
        second.send(Data((packet(sequence: 2, sessionID: "agent-a") + "\n").utf8))
        wait(for: [received], timeout: 10)
        client.stop()
    }

    private func packet(sequence: UInt64, sessionID: String) -> String {
        """
        {"version":1,"sequence":\(sequence),"session":{"id":"\(sessionID)","agentName":"\(sessionID)","pageTitle":"Inbox","url":"https://example.test/inbox"},"action":{"kind":"click","summary":"正在点击「登录」"}}
        """
    }
}
