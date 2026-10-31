import CoreGraphics
import XCTest

@testable import ViewerCore

/// 「小窗该不该出现」的判据 = 真窗口**你看不看得见**（2026-09-24 第二轮现场口径，
/// 规范 §4.4 第 5/6 条）。全是纯函数：没有窗口服务器的机器也能把规则钉死。
final class ViewerWindowVisibilityTests: XCTestCase {
    private let browserPID: pid_t = 4242
    private let selfPID: pid_t = 999

    private func window(
        pid: pid_t,
        layer: Int = 0,
        x: CGFloat = 0,
        y: CGFloat = 0,
        width: CGFloat = 480,
        height: CGFloat = 320,
        alpha: Double = 1,
        number: Int = 0
    ) -> ViewerWindowSnapshot {
        ViewerWindowSnapshot(
            pid: pid,
            layer: layer,
            frame: CGRect(x: x, y: y, width: width, height: height),
            alpha: alpha,
            number: number
        )
    }

    private func ratio(_ windows: [ViewerWindowSnapshot]) -> Double? {
        ViewerWindowVisibility.visibleRatio(browserPID: browserPID, selfPID: selfPID, windows: windows)
    }

    private func visible(_ windows: [ViewerWindowSnapshot]) -> Bool? {
        ViewerWindowVisibility.isRealWindowVisible(browserPID: browserPID, selfPID: selfPID, windows: windows)
    }

    /// 遮挡者**只能同层**。反例来自实测：按「层级更高即遮挡者」收集，会把 ChatGPT 那扇
    /// layer 3、1128×2079 的大窗算成挡住前台窗口 66.7%（规范 §4.4 第 6 条）。
    func testOnlySameLayerCoverersCount() {
        let target = window(pid: browserPID)
        let higherLayer = window(pid: 777, layer: 3, width: 1128, height: 2079)
        XCTAssertEqual(
            ratio([higherLayer, target]), 1.0,
            "layer 3 的大窗压在 target 之前也不是遮挡者：层不同，坐标空间也不同"
        )

        let sameLayer = window(pid: 777)
        XCTAssertEqual(ratio([sameLayer, target]), 0.0, "同层且排在目标之前 = 真遮挡者")
    }

    /// 数组序 = 前 → 后（`CGWindowList` 的序）：排在目标**之后**的同层窗口在它下面，
    /// 挡不住它。
    func testWindowsBehindTheTargetAreNotCoverers() {
        let target = window(pid: browserPID)
        let behind = window(pid: 777)
        XCTAssertEqual(ratio([target, behind]), 1.0)
    }

    /// 壳自己的窗口、以及菜单栏图标那种小窗，都不该把真页面算成「被挡住」。
    func testSelfWindowsAndTinyWindowsAreNotCoverers() {
        let target = window(pid: browserPID)
        XCTAssertEqual(ratio([window(pid: selfPID), target]), 1.0, "壳自己的面板不算遮挡者")

        let indicator = window(pid: 777, width: 18, height: 10)
        XCTAssertEqual(
            ratio([indicator, target]), 1.0,
            "10×18 的状态指示条要按面积门槛滤掉（规范 §4.4 第 6 条）"
        )
    }

    /// 阈值 0.6：挡掉一半（看得见 50%）算「看不见」；挡掉三成（看得见 ≈70%）算「看得见」。
    func testThresholdIsSixtyPercentVisible() throws {
        let target = window(pid: browserPID)
        let half = window(pid: 777, width: 240, height: 320)
        XCTAssertEqual(ratio([half, target]), 0.5)
        XCTAssertEqual(visible([half, target]), false)

        let third = window(pid: 777, width: 144, height: 320)
        let thirdVisible = ratio([third, target])
        XCTAssertGreaterThanOrEqual(try XCTUnwrap(thirdVisible), 0.6)
        XCTAssertEqual(visible([third, target]), true)
    }

    /// 屏上没有真窗口（最小化 / 已关 / 在别的 Space）→ `nil`，调用方按「看不见」处理，
    /// 小窗**该**出现。alpha≈0 的窗等同于没有。
    func testMissingRealWindowIsNilSoThePanelAppears() {
        let otherApp = window(pid: 777)
        XCTAssertNil(ViewerWindowVisibility.realWindow(browserPID: browserPID, windows: [otherApp]))
        XCTAssertNil(ratio([otherApp]))
        XCTAssertNil(visible([otherApp]))

        XCTAssertNil(
            ViewerWindowVisibility.realWindow(browserPID: browserPID, windows: [window(pid: browserPID, alpha: 0)]),
            "alpha≈0 的窗不算「看得见」"
        )
    }

    /// 同一会话开多扇窗时按**面积最大**的 layer 0 窗算（主窗）。
    func testRealWindowIsTheLargestLayerZeroWindowOfThatPID() {
        let small = window(pid: browserPID, width: 300, height: 200)
        let big = window(pid: browserPID, x: 100, y: 100, width: 900, height: 700)
        XCTAssertEqual(
            ViewerWindowVisibility.realWindow(browserPID: browserPID, windows: [small, big]), big
        )
    }

    /// 附着模式（引擎报 `browserPID=0`）靠展开回执里的矩形认出「这扇窗是谁的」——这条不成立，
    /// 第 5 条判据在用户自己的 Chrome 上就失效（规范 §4.4 第 6 条）。
    func testWindowIsIdentifiedByItsRectSoAttachedSessionsGetAPID() {
        let windows = [window(pid: 777, x: 12, y: 34, width: 900, height: 640)]
        XCTAssertEqual(
            ViewerWindowVisibility.hostPID(
                matching: CGRect(x: 12, y: 34, width: 900, height: 640), windows: windows
            ),
            777
        )
    }

    /// 容差 4px（真机实测两侧逐像素一致，留的只是余量）：3px 内认得出，差得多就**不认**
    /// ——宁可退回焦点判据，也不能把别人的窗口认成本会话的真窗口。
    func testRectMatchToleranceIsTightOnPurpose() {
        let windows = [window(pid: 777, x: 100, y: 50, width: 900, height: 640)]
        XCTAssertEqual(
            ViewerWindowVisibility.hostPID(
                matching: CGRect(x: 103, y: 52, width: 898, height: 638), windows: windows
            ),
            777
        )
        XCTAssertNil(
            ViewerWindowVisibility.hostPID(
                matching: CGRect(x: 120, y: 50, width: 900, height: 640), windows: windows
            )
        )
    }

    /// 认窗只在 `layer 0` 里找，且两个候选都在容差内时取**最近**的那个。
    func testRectMatchIgnoresFloatLayersAndTakesTheClosest() {
        let onScreen = window(pid: 777, x: 0, y: 0, width: 900, height: 640)
        let overlay = window(pid: 555, layer: 3, x: 0, y: 0, width: 900, height: 640)
        XCTAssertEqual(
            ViewerWindowVisibility.hostPID(matching: onScreen.frame, windows: [overlay]), nil,
            "layer 3 的面板不是真窗口"
        )
        let near = ViewerWindowSnapshot(
            pid: 888, layer: 0, frame: onScreen.frame.offsetBy(dx: 2, dy: 0), alpha: 1
        )
        XCTAssertEqual(
            ViewerWindowVisibility.hostPID(matching: onScreen.frame, windows: [near, onScreen]), 777
        )
    }

    /// 矩形为空/屏上没这扇窗 → `nil`（调用方保持「不知道 pid」，退回焦点判据）。
    func testRectMatchReturnsNilWhenNothingMatches() {
        XCTAssertNil(
            ViewerWindowVisibility.hostPID(matching: .zero, windows: [window(pid: 777)])
        )
        XCTAssertNil(
            ViewerWindowVisibility.hostPID(
                matching: CGRect(x: 500, y: 500, width: 300, height: 200), windows: [window(pid: 777)]
            )
        )
    }

    /// 目标窗自己的矩形也要参与采样：完全盖住它的遮挡者 → 看得见 0%。
    func testFullyCoveredTargetIsNotVisible() {
        let target = window(pid: browserPID, x: 100, y: 100)
        let coverer = window(pid: 777, x: 0, y: 0, width: 1200, height: 900)
        XCTAssertEqual(ratio([coverer, target]), 0.0)
        XCTAssertEqual(visible([coverer, target]), false)
    }

    // MARK: - 认过窗就按那扇窗算（规范 §4.4 第 6/7 条）

    /// 用户那台机器的现场配置：同一个 pid 下两扇差不多大的窗（`CGWindowList` 里逐像素
    /// 量过 `[144,132 1230×721]` 与 `[49,25 1100×806]`），面积几乎一样（886,830 vs
    /// 886,600）——会话那扇被盖住、另一扇露着时，按「pid 名下面积最大」就会判成看得见
    /// （小窗不出现，正是用户报的 bug），按**认过的那扇窗**算才对。测试里把两扇窗摆开，
    /// 让「盖住哪一扇」没有歧义。
    func testRecognizedWindowBeatsTheLargestWindowOfTheSamePID() {
        let session = window(pid: browserPID, x: 49, y: 25, width: 1100, height: 806, number: 22)
        let other = window(pid: browserPID, x: 144, y: 932, width: 1230, height: 721, number: 11)
        let coverer = window(pid: 777, x: 0, y: 0, width: 1200, height: 900)
        let onScreen = [coverer, other, session]

        XCTAssertEqual(
            ViewerWindowVisibility.visibleRatio(
                windowNumber: 22, browserPID: browserPID, selfPID: selfPID, windows: onScreen
            ),
            0.0,
            "会话那扇被完全盖住 → 看不见 → 小窗该出现"
        )
        XCTAssertEqual(
            ViewerWindowVisibility.visibleRatio(
                windowNumber: 11, browserPID: browserPID, selfPID: selfPID, windows: onScreen
            ),
            1.0,
            "另一扇同 pid 的窗不受影响"
        )
        XCTAssertEqual(
            ratio(onScreen), 1.0,
            "还没认过窗时才退回「pid 名下面积最大」：那扇（1230×721 > 1100×806）没被盖住 ⇒ 看得见"
        )
    }

    /// 认过的那扇窗不在屏上（最小化 / 已关 / 在别的 Space）= 真窗口看不见 → 小窗该出现，
    /// **不能**漂到同 pid 的别的窗上（那会让「最小化后小窗出现」失效）。
    func testAMissingRecognizedWindowCountsAsInvisible() {
        let other = window(pid: browserPID, x: 144, y: 132, width: 1230, height: 721, number: 11)
        XCTAssertNil(
            ViewerWindowVisibility.visibleRatio(
                windowNumber: 22, browserPID: browserPID, selfPID: selfPID, windows: [other]
            )
        )
    }

    /// 帧里已经带了 pid（引擎自建会话）时，认窗只在该 pid 名下找：同 bundle 的另一个
    /// Chrome 实例可能恰好有一扇同样大小的窗。
    func testRectMatchCanBeLimitedToTheExpectedPID() {
        let bounds = CGRect(x: 49, y: 25, width: 1100, height: 806)
        let foreign = window(pid: 16742, x: 49, y: 25, width: 1100, height: 806, number: 1)
        let ours = window(pid: 4242, x: 49, y: 25, width: 1100, height: 806, number: 2)
        XCTAssertEqual(
            ViewerWindowVisibility.matchingWindow(bounds: bounds, windows: [foreign, ours])?.number, 1,
            "不限 pid 时取列表里第一个命中的"
        )
        XCTAssertEqual(
            ViewerWindowVisibility.matchingWindow(
                bounds: bounds, windows: [foreign, ours], pid: browserPID
            )?.number, 2,
            "给了 pid 就只认它名下的窗"
        )
    }
}
