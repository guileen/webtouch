import CoreGraphics
import XCTest
@testable import ViewerCore

/// 「小窗的槽位＝用户放下的位置」这条契约（规范 §4.4 第 9 条，2026-09-24 用户现场缺口：
/// 把面板拖到屏幕中间后点开，飞走重现身时回到了**移动前**的位置）。
final class ViewerPanelSlotTests: XCTestCase {
    private let visible = CGRect(x: 0, y: 0, width: 1440, height: 900)
    private let compact = CGSize(width: 400, height: 258)

    /// 落到屏幕中间（离任何边都远）＝不需要吸附：位置要原样入账。
    /// 这正是当时的回归点——记账挂在「吸附确实改了坐标」的分支上，于是这次放下根本没记账。
    func testMidScreenDropIsRecordedEvenWhenNoSnapIsNeeded() {
        let dropped = CGRect(x: 520, y: 330, width: compact.width, height: compact.height)
        var slot = ViewerPanelSlot(frame: ViewerGeometry.compactFrame(inVisibleFrame: visible))

        let target = slot.noteUserDrop(dropped, inVisibleFrame: visible)

        XCTAssertEqual(target, dropped, "不需要吸附时不该挪动窗口")
        XCTAssertEqual(slot.frame, dropped, "没有吸附也要记账：用户放下的位置就是它的家")
    }

    /// 贴着右边放下：窗口会被吸附到边距上，槽位记的是**吸附后**的位置（也就是用户看到的位置）。
    func testEdgeDropIsRecordedAsTheSnappedPosition() {
        let dropped = CGRect(x: 1440 - 400 - 6, y: 330, width: compact.width, height: compact.height)
        var slot = ViewerPanelSlot(frame: .zero)

        let target = slot.noteUserDrop(dropped, inVisibleFrame: visible)

        XCTAssertEqual(target.minX, visible.maxX - ViewerGeometry.margin - compact.width)
        XCTAssertEqual(slot.frame, target)
        XCTAssertNotEqual(slot.frame, dropped)
    }

    /// 一多半拖出屏外：夹回可见区，槽位跟着夹取结果走。
    /// 注意 `snapped` 的既有口径是「贴边只对上/左/右生效」，下边只夹取不贴边——这里如实钉住它，
    /// 免得把「窗口该在哪儿」写成另一套想象。
    func testOffscreenDropIsClampedBeforeItBecomesTheSlot() {
        let dropped = CGRect(x: 1300, y: 700, width: compact.width, height: compact.height)
        var slot = ViewerPanelSlot(frame: .zero)

        slot.noteUserDrop(dropped, inVisibleFrame: visible)

        XCTAssertTrue(visible.contains(slot.frame))
        XCTAssertEqual(slot.frame.minX, visible.maxX - ViewerGeometry.margin - compact.width)
        XCTAssertEqual(slot.frame.minY, visible.maxY - compact.height)
    }

    /// 系统/程序造成的位移（macOS 夹取、缩放）也要跟：否则下一次回到旧位还是错的。
    func testProgrammaticFrameFollowsTheWindow() {
        var slot = ViewerPanelSlot(frame: CGRect(x: 0, y: 0, width: 400, height: 258))
        let moved = CGRect(x: -120, y: 40, width: 400, height: 258)

        slot.noteProgrammaticFrame(moved)

        XCTAssertEqual(slot.frame, moved)
    }

    /// 用户现场那条复现路径：默认槽位 → 用户拖到中间 → 展开飞走 → 重现时回的是**移动后**的位置。
    func testSlotSurvivesTheFlyAwayRoundTrip() {
        let defaultFrame = ViewerGeometry.compactFrame(inVisibleFrame: visible)
        let dropped = CGRect(x: 300, y: 500, width: compact.width, height: compact.height)
        var slot = ViewerPanelSlot(frame: defaultFrame)

        slot.noteUserDrop(dropped, inVisibleFrame: visible)

        XCTAssertEqual(slot.frame, dropped, "飞走前后槽位不变，回去的必须是移动后的位置")
        XCTAssertNotEqual(slot.frame, defaultFrame)
    }
}
