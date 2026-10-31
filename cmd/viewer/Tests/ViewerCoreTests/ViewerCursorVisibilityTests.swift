import AppKit
import CoreGraphics
import XCTest
@testable import ViewerCore

/// 2026-09-23 用户现场反馈：「我看到的鼠标（小窗）仍然在屏幕中间」。
///
/// `ViewerCursorLayer` 的默认位置 (0.5, 0.5) 就是面板正中，而且它从一开始就可见——
/// 于是任何还没有真实指针数据的会话，小窗里都会画出一根"停在中间的假鼠标"，
/// 看起来像自动化卡住了。修法：没有指针数据时整层隐藏，只由真实指针包唤醒；
/// 换会话（换页面）后收回隐藏态。
final class ViewerCursorVisibilityTests: XCTestCase {
    private let rect = CGRect(x: 0, y: 0, width: 400, height: 258)

    private func session(_ id: String) -> ViewerSession {
        ViewerSession(id: id, agentName: "Agent", pageTitle: "Page \(id)", url: "https://example.test/\(id)")
    }

    func testCursorStaysHiddenUntilARealPointerPacketArrives() {
        let layer = ViewerCursorLayer()

        XCTAssertTrue(layer.isHidden, "没有指针数据时不该画出默认位置的光标")
        XCTAssertFalse(layer.hasPointer)

        layer.reveal()
        layer.move(to: CGPoint(x: 0.25, y: 0.75), in: rect, animated: false)

        XCTAssertFalse(layer.isHidden)
        XCTAssertTrue(layer.hasPointer)
    }

    func testLayoutMovesDoNotRevealTheCursor() {
        let layer = ViewerCursorLayer()

        layer.move(to: CGPoint(x: 0.5, y: 0.5), in: rect, animated: false)

        XCTAssertTrue(layer.isHidden, "换帧/布局的纯定位不该把光标显出来")
    }

    func testResetPointerHidesTheCursorAgain() {
        let layer = ViewerCursorLayer()
        layer.reveal()

        layer.resetPointer()

        XCTAssertTrue(layer.isHidden)
        XCTAssertFalse(layer.hasPointer)
    }

    func testCanvasShowsCursorOnlyAfterAPointerPacket() {
        let canvas = ViewerCanvasView(session: session("s1"))

        XCTAssertFalse(canvas.isCursorVisible)

        canvas.showVirtualCursor(x: 0.2, y: 0.3, clicked: false)

        XCTAssertTrue(canvas.isCursorVisible)
        XCTAssertEqual(canvas.cursorNormalized.x, 0.2, accuracy: 0.0001)
        XCTAssertEqual(canvas.cursorNormalized.y, 0.3, accuracy: 0.0001)
    }

    func testCanvasHidesTheCursorWhenThePanelIsReboundToAnotherSession() {
        let canvas = ViewerCanvasView(session: session("s1"))
        canvas.showVirtualCursor(x: 0.2, y: 0.3, clicked: false)

        canvas.update(session: session("s2"))

        XCTAssertFalse(canvas.isCursorVisible, "上一页的光标位置对新的会话没有意义")
    }

    func testCanvasKeepsTheCursorForTitleOnlyUpdates() {
        let canvas = ViewerCanvasView(session: session("s1"))
        canvas.showVirtualCursor(x: 0.2, y: 0.3, clicked: false)

        canvas.update(session: ViewerSession(id: "s1", agentName: "Agent", pageTitle: "换标题", url: "https://example.test/s1"))

        XCTAssertTrue(canvas.isCursorVisible, "同一会话只是标题变了，光标不该闪掉")
    }
}
