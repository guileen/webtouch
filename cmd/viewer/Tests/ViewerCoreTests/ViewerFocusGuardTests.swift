import CoreGraphics
import XCTest
@testable import ViewerCore

final class ViewerFocusGuardTests: XCTestCase {
    func testStaysExpandedUntilTheFocusIsActuallyLost() {
        var guardUnit = ViewerFocusGuard(graceInterval: 0.7)
        guardUnit.noteExpanded(at: 100)

        // 2026-09-12 user feedback: 点击放大后，回到其他窗口/点击别处才自动缩小。
        XCTAssertNil(guardUnit.collapseReason(for: .otherApplicationActivated, at: 100.3))
        XCTAssertEqual(guardUnit.collapseReason(for: .otherApplicationActivated, at: 101), .otherApplicationActivated)
        XCTAssertEqual(guardUnit.collapseReason(for: .clickOutsideShell, at: 120), .clickOutsideShell)
    }

    func testCollapsedShellIgnoresFocusNoise() {
        var guardUnit = ViewerFocusGuard()
        XCTAssertFalse(guardUnit.isExpanded)
        XCTAssertNil(guardUnit.collapseReason(for: .clickOutsideShell, at: 500))

        guardUnit.noteExpanded(at: 10)
        guardUnit.noteCollapsed()
        XCTAssertFalse(guardUnit.isExpanded)
        XCTAssertNil(guardUnit.collapseReason(for: .otherApplicationActivated, at: 500))
    }

    func testInsideShellHitTestUsesScreenCoordinates() {
        let expanded = CGRect(x: 100, y: 200, width: 960, height: 620)
        let compact = CGRect(x: 1022, y: 624, width: 400, height: 258)

        XCTAssertTrue(ViewerFocusGuard.isInsideShell(CGPoint(x: 120, y: 240), frames: [expanded, compact]))
        XCTAssertTrue(ViewerFocusGuard.isInsideShell(CGPoint(x: 1100, y: 700), frames: [expanded, compact]))
        XCTAssertFalse(ViewerFocusGuard.isInsideShell(CGPoint(x: 50, y: 240), frames: [expanded, compact]))
    }
}
