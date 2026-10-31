import CoreGraphics
import XCTest
@testable import ViewerCore

final class ViewerGeometryTests: XCTestCase {
    private let visible = CGRect(x: 0, y: 0, width: 1440, height: 900)

    func testCompactFrameHugsTopRightCornerWithMargin() {
        let frame = ViewerGeometry.compactFrame(inVisibleFrame: visible)

        XCTAssertEqual(frame.size, ViewerGeometry.compactSize)
        XCTAssertEqual(frame.maxX, visible.maxX - ViewerGeometry.margin)
        XCTAssertEqual(frame.maxY, visible.maxY - ViewerGeometry.margin)
    }

    func testCompactFrameStaysInsideASmallScreen() {
        let small = CGRect(x: 0, y: 0, width: 300, height: 220)
        let frame = ViewerGeometry.compactFrame(inVisibleFrame: small)

        XCTAssertTrue(small.contains(frame))
        XCTAssertGreaterThanOrEqual(frame.width, ViewerGeometry.minimumSize.width)
    }

    func testExpandedFrameKeepsTopRightAnchorAndFitsOnScreen() {
        let compact = ViewerGeometry.compactFrame(inVisibleFrame: visible)
        let expanded = ViewerGeometry.expandedFrame(from: compact, inVisibleFrame: visible)

        XCTAssertEqual(expanded.maxX, compact.maxX)
        XCTAssertEqual(expanded.maxY, compact.maxY)
        XCTAssertEqual(expanded.size, ViewerGeometry.expandedSize)
        XCTAssertTrue(visible.contains(expanded))
        XCTAssertGreaterThan(expanded.width, compact.width)
    }

    func testExpandedFrameClampsBackIntoASmallScreen() {
        let small = CGRect(x: 0, y: 0, width: 640, height: 420)
        let compact = ViewerGeometry.compactFrame(inVisibleFrame: small)
        let expanded = ViewerGeometry.expandedFrame(from: compact, inVisibleFrame: small)

        XCTAssertTrue(small.contains(expanded))
    }

    func testSnappedPullsTheFrameToTheNearestEdge() {
        let nearLeft = CGRect(x: 6, y: 300, width: 400, height: 258)
        let snapped = ViewerGeometry.snapped(nearLeft, inVisibleFrame: visible)
        XCTAssertEqual(snapped.minX, visible.minX + ViewerGeometry.margin)

        let middle = CGRect(x: 520, y: 300, width: 400, height: 258)
        XCTAssertEqual(ViewerGeometry.snapped(middle, inVisibleFrame: visible), middle)
    }

    func testSnappedNeverLeavesTheVisibleFrame() {
        let offscreen = CGRect(x: 1400, y: 880, width: 400, height: 258)
        let snapped = ViewerGeometry.snapped(offscreen, inVisibleFrame: visible)

        XCTAssertTrue(visible.contains(snapped))
    }

    func testAspectFitRectLetterboxesWideContent() {
        let bounds = CGRect(x: 0, y: 0, width: 400, height: 258)
        let rect = ViewerGeometry.aspectFitRect(contentSize: CGSize(width: 640, height: 400), in: bounds)

        // 1.6:1 的画面放进 1.55:1 的面板：按宽度贴合，上下各留一条 4pt 信箱边。
        XCTAssertEqual(rect.width, 400, accuracy: 0.001)
        XCTAssertEqual(rect.height, 250, accuracy: 0.001)
        XCTAssertEqual(rect.minY, 4, accuracy: 0.001)
        XCTAssertEqual(rect.midX, bounds.midX, accuracy: 0.001)
    }

    func testAspectFitRectPillarboxesATallPhoneViewport() {
        let bounds = CGRect(x: 0, y: 0, width: 400, height: 258)
        let rect = ViewerGeometry.aspectFitRect(contentSize: CGSize(width: 390, height: 844), in: bounds)

        // 手机档案视口比面板窄得多：修之前这条会让虚拟光标偏出几十个点。
        XCTAssertEqual(rect.height, 258, accuracy: 0.001)
        XCTAssertLessThan(rect.width, bounds.width)
        XCTAssertEqual(rect.midX, bounds.midX, accuracy: 0.001)
    }

    func testAspectFitRectKeepsMatchingAspectAsTheWholeBounds() {
        let bounds = CGRect(x: 0, y: 0, width: 320, height: 200)
        let rect = ViewerGeometry.aspectFitRect(contentSize: CGSize(width: 640, height: 400), in: bounds)

        XCTAssertEqual(rect, bounds)
    }

    func testAspectFitRectFallsBackToBoundsOnDegenerateInput() {
        let bounds = CGRect(x: 3, y: 5, width: 100, height: 50)

        XCTAssertEqual(ViewerGeometry.aspectFitRect(contentSize: .zero, in: bounds), bounds)
        XCTAssertEqual(ViewerGeometry.aspectFitRect(contentSize: CGSize(width: 10, height: 10), in: .zero), .zero)
    }
}
