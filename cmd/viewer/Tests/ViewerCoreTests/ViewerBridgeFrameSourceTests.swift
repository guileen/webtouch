import XCTest
@testable import ViewerCore

final class ViewerBridgeFrameSourceTests: XCTestCase {
    func testAcceptsNewestSessionScopedFrameAndPreservesMetadata() throws {
        let source = LocalBridgeFrameSource()
        source.start()
        var received: ViewerFrame?
        source.onFrame = { received = $0 }

        XCTAssertTrue(source.receive(jsonLine: packet(sequence: 2, jpeg: "/9j/", pointerKind: "mousePressed")))

        let frame = try XCTUnwrap(received)
        XCTAssertEqual(frame.session.id, "agent-a/run-1")
        XCTAssertEqual(frame.session.agentName, "Agent A")
        XCTAssertEqual(frame.jpeg, Data([0xff, 0xd8, 0xff]))
        XCTAssertEqual(frame.metadata.deviceWidth, 1280)
        XCTAssertEqual(frame.metadata.deviceHeight, 720)
        XCTAssertEqual(frame.screencastSessionID, 9)
        XCTAssertEqual(frame.pointer?.kind, .mousePressed)
        XCTAssertEqual(frame.action?.summary, "text entered")
    }

    func testDropsOlderFrameForTheSameSessionButKeepsAnotherSessionIndependent() throws {
        let source = LocalBridgeFrameSource()
        source.start()
        var received = [ViewerFrame]()
        source.onFrame = { received.append($0) }

        XCTAssertTrue(source.receive(jsonLine: packet(sequence: 2, jpeg: "bmV3")))
        XCTAssertFalse(source.receive(jsonLine: packet(sequence: 1, jpeg: "b2xk")))
        XCTAssertTrue(source.receive(jsonLine: packet(sequence: 1, jpeg: "b3RoZXI=", sessionID: "agent-b/run-2")))

        XCTAssertEqual(received.map(\.session.id), ["agent-a/run-1", "agent-b/run-2"])
        XCTAssertEqual(received.map(\.jpeg), [Data("new".utf8), Data("other".utf8)])
        XCTAssertEqual(source.latestUpdate(sessionID: "agent-a/run-1")?.sequence, 2)
    }

    func testRejectsUnknownProtocolAndMissingSessionIdentity() {
        let source = LocalBridgeFrameSource()
        XCTAssertFalse(source.receive(jsonLine: Data(#"{"version":2,"sequence":1,"session":{"id":"s"}}"#.utf8)))
        XCTAssertFalse(source.receive(jsonLine: Data(#"{"version":1,"sequence":1,"session":{"id":""}}"#.utf8)))
    }

    private func packet(sequence: UInt64, jpeg: String, sessionID: String = "agent-a/run-1", pointerKind: String = "mouseMoved") -> Data {
        Data("""
        {"version":1,"sequence":\(sequence),"session":{"id":"\(sessionID)","agentName":"Agent A","pageTitle":"Inbox","url":"https://example.test/inbox"},"frame":{"jpeg":"\(jpeg)","metadata":{"offsetTop":0,"pageScaleFactor":1,"deviceWidth":1280,"deviceHeight":720,"scrollOffsetX":0,"scrollOffsetY":0,"timestamp":42},"cdpSessionID":"flattened-1","screencastSessionID":9,"targetID":"target-1"},"pointer":{"x":300,"y":120,"kind":"\(pointerKind)","button":"left","clickCount":1,"buttons":1},"action":{"kind":"input","summary":"text entered"}}
        """.utf8)
    }
}
