import XCTest
@testable import ViewerCore

/// 2026-09-22 现场批次 #2c：点开小窗后只是一张**点不动、滚不动**的图。
///
/// 根因是壳侧 1.2s 的超时比引擎一次抬起（5~6 次 CDP 往返，引擎侧
/// `viewerIntentTimeout=5s`）短得多：忙页面上壳先回落成自绘放大面板，**随后**引擎的
/// 成功回执才到，而旧代码在回落分支里又主动补发一条 collapse，把真窗口搬了回去
/// ⇒ 用户永远看不到真窗口。下面断言的是修好之后的分支。
///
/// 2026-09-22 口径更新后，展开 = 引擎把那扇真窗口**抬到最前**（不搬位置、不缩放），
/// 收尾只有两种：`.handOverToRealWindow`（小窗飞走让位）与 `.restoreRealWindow`
/// （用户已经不要了，替他把抬起来的窗口放回去）。「真页面在屏幕上」不再是一个壳要
/// 追踪的状态，所以 `.live` / `.collapsing` 与 collapse 回执整条链路都删了。
final class ViewerExpansionMachineTests: XCTestCase {
    /// 超时只回落、**不**撤销：引擎那边的窗口可能已经在抬了。
    func testATimedOutExpansionFallsBackWithoutUndoingTheWindow() {
        var machine = ViewerExpansionMachine()
        XCTAssertTrue(machine.beginExpansion(session: "ctx-1"))
        XCTAssertEqual(machine.phase, .expanding)

        XCTAssertEqual(machine.expansionTimedOut(), .showDrawnPanel)
        XCTAssertEqual(machine.phase, .fellBack)
    }

    /// 迟到的 ok=true 把自绘放大面板**升级**成真窗口（旧代码在这里反而把窗口搬回去）。
    func testALateSuccessfulAckUpgradesTheDrawnPanelToTheRealWindow() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.expansionTimedOut()

        XCTAssertEqual(
            machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: true),
            .handOverToRealWindow
        )
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 成功回执即收尾：这条展开链路结束，会话号也交回去（下一次展开可以是别人）。
    func testASuccessfulHandOverEndsTheLink() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")

        XCTAssertEqual(
            machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: false),
            .handOverToRealWindow
        )
        XCTAssertEqual(machine.phase, .idle)
        XCTAssertNil(machine.session)
    }

    /// 用户已经把自绘面板收回了，迟到的回执不能再把它弹开：真窗口确实被抬到最前了，
    /// 替他放回去。
    func testALateSuccessfulAckAfterTheUserCollapsedPutsTheWindowBack() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.expansionTimedOut()

        XCTAssertEqual(
            machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: false),
            .restoreRealWindow
        )
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 面板已经换绑到别的会话了：同样不能把真窗口抬到别人头上，把窗口放回去。
    func testALateSuccessfulAckAfterThePanelMovedOnPutsTheWindowBack() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.expansionTimedOut()

        XCTAssertEqual(
            machine.resolveExpansion(ok: true, sessionStillShown: false, drawnPanelOnScreen: false),
            .restoreRealWindow
        )
    }

    /// 引擎说抬不起来（无头会话 / 那扇真窗口已经被关掉）：立刻回落自绘面板，不干等超时。
    func testAFailedExpansionFallsBackToTheDrawnPanel() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")

        XCTAssertEqual(
            machine.resolveExpansion(ok: false, sessionStillShown: true, drawnPanelOnScreen: false),
            .showDrawnPanel
        )
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 用户在等回执的半路点了收回：还是得把（可能已经抬起来的）真窗口放回去。
    func testAUserCancelledExpansionStillPutsTheRealWindowBack() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")

        XCTAssertEqual(machine.requestCollapse(), .cancelled)
        XCTAssertEqual(machine.phase, .cancelled)

        XCTAssertEqual(
            machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: false),
            .restoreRealWindow
        )
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 取消之后迟到的失败回执：引擎根本没动过窗口，什么都不用做。
    func testALosingAckForACancelledExpansionDoesNothing() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.requestCollapse()

        XCTAssertEqual(
            machine.resolveExpansion(ok: false, sessionStillShown: true, drawnPanelOnScreen: false),
            .nothing
        )
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 自绘放大面板上的「收回」：那是壳内的事，不劳引擎。
    func testCollapsingTheDrawnPanelIsTheShellsOwnBusiness() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.expansionTimedOut()

        XCTAssertEqual(machine.requestCollapse(), .collapseDrawnPanel)
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 真窗口已经到最前、小窗飞走了：这一下收回没有对应的壳内动作（真的没东西可收）。
    func testCollapseAfterAHandOverIsIgnored() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: false)

        XCTAssertEqual(machine.requestCollapse(), .ignored)
        XCTAssertEqual(machine.phase, .idle)
    }

    /// 一次只展开一块面板：已经在展开链路上时，新的展开请求被拒（宿主据此不重复发意图）。
    func testASecondExpansionIsRefusedWhileOneIsInFlight() {
        var machine = ViewerExpansionMachine()
        XCTAssertTrue(machine.beginExpansion(session: "ctx-1"))
        XCTAssertFalse(machine.beginExpansion(session: "ctx-2"))
        XCTAssertEqual(machine.session, "ctx-1")
    }

    /// 展开链路收尾后，下一个会话可以重新展开。
    func testTheNextSessionCanExpandOnceTheLinkIsIdle() {
        var machine = ViewerExpansionMachine()
        _ = machine.beginExpansion(session: "ctx-1")
        _ = machine.resolveExpansion(ok: true, sessionStillShown: true, drawnPanelOnScreen: false)

        XCTAssertTrue(machine.beginExpansion(session: "ctx-2"))
        XCTAssertEqual(machine.session, "ctx-2")
    }
}
