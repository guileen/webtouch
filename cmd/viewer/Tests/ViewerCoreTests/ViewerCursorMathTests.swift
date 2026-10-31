import CoreGraphics
import XCTest
@testable import ViewerCore

final class ViewerCursorMathTests: XCTestCase {
    func testLayerPointFlipsTheVerticalAxis() {
        let size = CGSize(width: 400, height: 258)

        XCTAssertEqual(ViewerCursorMath.layerPoint(normalized: CGPoint(x: 0, y: 0), in: size), CGPoint(x: 0, y: 258))
        XCTAssertEqual(ViewerCursorMath.layerPoint(normalized: CGPoint(x: 1, y: 1), in: size), CGPoint(x: 400, y: 0))
    }

    func testNormalizedClampsOutOfRangeInput() {
        XCTAssertEqual(ViewerCursorMath.normalized(CGPoint(x: -0.4, y: 1.7)), CGPoint(x: 0, y: 1))
    }

    func testAnimationDurationGrowsWithDistance() {
        let origin = CGPoint(x: 0.5, y: 0.5)
        let near = ViewerCursorMath.animationDuration(from: origin, to: CGPoint(x: 0.52, y: 0.5))
        let mid = ViewerCursorMath.animationDuration(from: origin, to: CGPoint(x: 0.7, y: 0.5))
        let far = ViewerCursorMath.animationDuration(from: origin, to: CGPoint(x: 0.95, y: 0.5))

        XCTAssertLessThan(near, mid)
        XCTAssertLessThan(mid, far)
        XCTAssertLessThanOrEqual(far, 0.20)
    }

    func testArrowOutlineFitsTheUnitBoxWithATopLeftTip() {
        let points = ViewerCursorLayer.arrowUnitPoints

        XCTAssertEqual(points.first, CGPoint(x: 0, y: 1))
        for point in points {
            XCTAssertTrue((0...1).contains(point.x), "x out of unit box: \(point)")
            XCTAssertTrue((0...1).contains(point.y), "y out of unit box: \(point)")
        }
    }

    func testArrowPathStaysInsideItsBounds() {
        let bounds = CGRect(x: 0, y: 0, width: 13.2, height: 24)
        let path = ViewerCursorLayer.arrowPath(in: bounds)

        XCTAssertFalse(path.isEmpty)
        XCTAssertTrue(bounds.insetBy(dx: -0.01, dy: -0.01).contains(path.boundingBox))
    }

    func testLayerPointMapsIntoThePaintedRect() {
        // 画面被信箱边压到 y=4..254：指针必须跟着画面走，而不是跟着图层走。
        let rect = CGRect(x: 0, y: 4, width: 400, height: 250)

        XCTAssertEqual(ViewerCursorMath.layerPoint(normalized: CGPoint(x: 0, y: 1), in: rect), CGPoint(x: 0, y: 4))
        XCTAssertEqual(ViewerCursorMath.layerPoint(normalized: CGPoint(x: 1, y: 0), in: rect), CGPoint(x: 400, y: 254))
        XCTAssertEqual(
            ViewerCursorMath.layerPoint(normalized: CGPoint(x: 0.5, y: 0.5), in: rect),
            CGPoint(x: 200, y: 129)
        )
    }

    func testLayerPointInSizeIsTheZeroOriginRectCase() {
        let size = CGSize(width: 400, height: 258)
        let rect = CGRect(origin: .zero, size: size)

        for point in [CGPoint(x: 0, y: 0), CGPoint(x: 0.25, y: 0.75), CGPoint(x: 1, y: 1)] {
            XCTAssertEqual(
                ViewerCursorMath.layerPoint(normalized: point, in: size),
                ViewerCursorMath.layerPoint(normalized: point, in: rect)
            )
        }
    }
}
