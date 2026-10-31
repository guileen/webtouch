import CoreGraphics
import XCTest
@testable import ViewerCore

/// 「点开 = 小窗的四个角向外飞到那扇真窗口」这条动画契约（2026-09-24 用户口径，规范 §4.4）。
final class ViewerFlyAwayTests: XCTestCase {
    /// 小窗当前位置（右上角锚）。
    private let compact = CGRect(x: 1000, y: 620, width: 400, height: 258)
    /// 引擎回执里的真窗口矩形（已换算成 AppKit 坐标）。
    private let realWindow = CGRect(x: 72, y: 40, width: 1440, height: 887)

    func testFlyAwayDestinationIsTheRealWindowRectItself() {
        let destination = ViewerGeometry.flyAwayDestination(from: compact, toward: realWindow)

        XCTAssertEqual(destination, realWindow, "落点必须是引擎回执里的真实矩形本身，不能自造几何")
    }

    func testFlyAwayGrowsOutwardsInsteadOfShrinking() {
        let destination = ViewerGeometry.flyAwayDestination(from: compact, toward: realWindow)

        XCTAssertGreaterThan(destination.width, compact.width)
        XCTAssertGreaterThan(destination.height, compact.height)
        // 四个角都在向外：2026-09-22 那版把落点写成 start/2（缩小），这里正面反证。
        XCTAssertLessThan(destination.minX, compact.minX)
        XCTAssertLessThan(destination.minY, compact.minY)
        XCTAssertGreaterThan(destination.maxX, compact.maxX)
        XCTAssertGreaterThan(destination.maxY, compact.maxY)
    }

    func testFlyAwayFadesInPlaceWhenTheEngineHasNoRect() {
        XCTAssertEqual(ViewerGeometry.flyAwayDestination(from: compact, toward: .zero), compact)
        XCTAssertEqual(
            ViewerGeometry.flyAwayDestination(from: compact, toward: CGRect(x: 10, y: 10, width: 0, height: 300)),
            compact,
            "空矩形不猜：落点回落到小窗自己，动画退化为纯淡出"
        )
    }

    func testExpandDurationMatchesTheSpecifiedQuickValue() {
        XCTAssertEqual(ViewerGeometry.expandDuration, 0.18, accuracy: 0.001)
    }

    /// 抬升的时刻：动画**末段**（§4.4 第 3 条）——设计值 0.18 - 0.03 = 0.15s。
    /// 这条断言盯的是口径本身：改 `expandDuration`/`raiseLeadTime` 而忘了它们的关系会红。
    func testRaiseDelayLandsInTheLastSegmentOfTheAnimation() {
        XCTAssertEqual(ViewerGeometry.raiseLeadTime, 0.03, accuracy: 0.001)
        XCTAssertEqual(ViewerGeometry.raiseDelay, 0.15, accuracy: 0.001)
        XCTAssertLessThan(ViewerGeometry.raiseDelay, ViewerGeometry.expandDuration, "抬升必须在动画演完之前")
    }

    func testActivationPIDOnlyAcceptsPositiveBrowserPIDs() {
        XCTAssertNil(ViewerAppActivation.activationPID(browserPID: nil), "附着模式没有 pid，不动手")
        XCTAssertNil(ViewerAppActivation.activationPID(browserPID: 0))
        XCTAssertNil(ViewerAppActivation.activationPID(browserPID: -1))
        XCTAssertEqual(ViewerAppActivation.activationPID(browserPID: 16742), 16742)
    }
}
