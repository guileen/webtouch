import CoreGraphics
import XCTest
@testable import ViewerCore

/// 指针归一路由与会话元数据。多会话的「该显示谁」规则在 `ViewerPanelRouterTests`：
/// 2026-09-22 现场批次 #2 之后屏幕上只有一块小窗，级联槽位（`ViewerSessionRegistry`）
/// 随那次改造退役。
final class ViewerPointerRoutingTests: XCTestCase {

    func testPointerIsNormalizedAgainstTheReportedViewport() {
        let metadata = ViewerFrameMetadata(
            offsetTop: 0, pageScaleFactor: 1, deviceWidth: 1280, deviceHeight: 720,
            scrollOffsetX: 0, scrollOffsetY: 0, timestamp: 1
        )
        let pointer = ViewerPointer(x: 640, y: 180, kind: .mousePressed, button: "left", clickCount: 1, buttons: 1)

        let point = ViewerPointerRouting.normalized(pointer, metadata: metadata)
        XCTAssertEqual(point?.x ?? -1, 0.5, accuracy: 0.0001)
        XCTAssertEqual(point?.y ?? -1, 0.25, accuracy: 0.0001)
        XCTAssertTrue(ViewerPointerRouting.isPress(.mousePressed))
        XCTAssertFalse(ViewerPointerRouting.isPress(.mouseMoved))
    }

    func testPointerOutsideTheViewportIsClampedAndMetadataIsRequired() {
        let metadata = ViewerFrameMetadata(
            offsetTop: 0, pageScaleFactor: 1, deviceWidth: 100, deviceHeight: 100,
            scrollOffsetX: 0, scrollOffsetY: 0, timestamp: 1
        )
        let wild = ViewerPointer(x: 900, y: -50, kind: .mouseMoved, button: nil, clickCount: nil, buttons: nil)

        XCTAssertEqual(ViewerPointerRouting.normalized(wild, metadata: metadata)?.x ?? -1, 1, accuracy: 0.0001)
        XCTAssertEqual(ViewerPointerRouting.normalized(wild, metadata: metadata)?.y ?? -1, 0, accuracy: 0.0001)
        XCTAssertNil(ViewerPointerRouting.normalized(wild, metadata: nil))
    }

    func testSessionMetadataIsLive() {
        let session = ViewerSession(id: "s-1", agentName: "Agent A", pageTitle: "First", url: "https://example.test/1")
        let state = ViewerState(session: session)
        state.session = ViewerSession(id: "s-1", agentName: "Agent A", pageTitle: "Second", url: "https://example.test/2")

        XCTAssertEqual(state.session.pageTitle, "Second")
    }
}
