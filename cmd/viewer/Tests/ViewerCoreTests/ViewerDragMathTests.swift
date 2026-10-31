import CoreGraphics
import XCTest

@testable import ViewerCore

final class ViewerDragMathTests: XCTestCase {
    func testWindowFollowsThePointerOneForOne() {
        let origin = CGPoint(x: 1000, y: 800)
        let moved = ViewerDragMath.origin(
            windowOrigin: origin,
            mouseAtPress: CGPoint(x: 400, y: 300),
            mouseNow: CGPoint(x: 500, y: 260)
        )
        XCTAssertEqual(moved.x, origin.x + 100)
        XCTAssertEqual(moved.y, origin.y - 40)
    }

    /// Regression: the old implementation measured the pointer in window
    /// coordinates, so a steady 100pt drag ended up moving the window only a
    /// fraction of that (field report: "moved 100, window moved 40").
    func testAFullDragAppliesTheWholeDistanceNotTheLastStep() {
        let origin = CGPoint(x: 120, y: 640)
        let press = CGPoint(x: 900, y: 600)
        var applied = origin
        for step in stride(from: 10, through: 100, by: 10) {
            applied = ViewerDragMath.origin(
                windowOrigin: origin,
                mouseAtPress: press,
                mouseNow: CGPoint(x: press.x + CGFloat(step), y: press.y)
            )
        }
        XCTAssertEqual(applied.x, origin.x + 100)
        XCTAssertEqual(applied.y, origin.y)
    }

    func testPressBelowThresholdStaysAClick() {
        XCTAssertEqual(ViewerDragMath.travelled(mouseAtPress: .zero, mouseNow: CGPoint(x: 3, y: 4)), 5)
        XCTAssertFalse(
            ViewerDragMath.travelled(mouseAtPress: .zero, mouseNow: CGPoint(x: 2, y: 2)) > ViewerDragMath.dragThreshold
        )
        XCTAssertTrue(
            ViewerDragMath.travelled(mouseAtPress: .zero, mouseNow: CGPoint(x: 4, y: 4)) > ViewerDragMath.dragThreshold
        )
    }
}
