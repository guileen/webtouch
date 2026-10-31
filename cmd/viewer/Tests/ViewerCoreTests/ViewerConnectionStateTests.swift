import XCTest
@testable import ViewerCore

/// 断线必须被说出来：`.retrying` 之前只有 emit、没有消费，用户看到的是「停在
/// 最后一帧的画面上」，分不出是卡住还是刚断（2026-09-13 评审）。
final class ViewerConnectionStateTests: XCTestCase {
    func testConnectedAndUnknownAreSilent() {
        XCTAssertNil(ViewerConnectionLabel.text(for: .connected))
        XCTAssertNil(ViewerConnectionLabel.text(for: .unknown))
    }

    func testRetryingSaysHowLongUntilTheNextTry() {
        XCTAssertEqual(ViewerConnectionLabel.text(for: .retrying(after: 1.6)), "引擎未连接 · 1s 后重试")
        XCTAssertEqual(ViewerConnectionLabel.text(for: .retrying(after: 0.4)), "引擎未连接 · 1s 后重试")
    }

    func testConnectingAndOfflineHaveTheirOwnText() {
        XCTAssertEqual(ViewerConnectionLabel.text(for: .connecting), "正在连接引擎…")
        XCTAssertEqual(ViewerConnectionLabel.text(for: .offline), "引擎未连接")
    }

    func testSocketStatesMapOntoLinkStates() {
        XCTAssertEqual(ViewerLinkState(.idle), .offline)
        XCTAssertEqual(ViewerLinkState(.connecting), .connecting)
        XCTAssertEqual(ViewerLinkState(.connected), .connected)
        XCTAssertEqual(ViewerLinkState(.retrying(after: 2.5)), .retrying(after: 2.5))
    }
}
