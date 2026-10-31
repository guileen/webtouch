import XCTest
@testable import ViewerCore

/// 2026-09-22 现场批次 #2：一次 AutoDL 实战就堆出 ctx-1…ctx-7 七块小窗，用户「看不过来」。
///
/// 需求（`docs/viewer-requirements-2026-09-11.md` §3.6）本来就写死「默认全局单小窗，
/// 跟随最近活跃会话自动切换视图；多会话并列多窗只是待定项」，是**实现**走成了「每会话
/// 一块面板」。这里断言的就是那条原始规则：屏幕上只有一块面板，它跟随最近有动作的会话。
final class ViewerPanelRouterTests: XCTestCase {
    /// 静默期（还没有任何动作）也得有东西可看：第一块面板跟随第一个出现的会话。
    func testTheFirstSessionWeEverSeeGetsTheOnlyPanel() {
        var router = ViewerPanelRouter()
        XCTAssertEqual(router.route(sessionID: "ctx-1", hasAction: false), "ctx-1")
        XCTAssertEqual(router.boundSessionID, "ctx-1")
    }

    /// 别的会话光有画面不算「活跃」：否则两个 Agent 一起跑，面板会在两个人之间来回翻。
    func testFramesFromAQuietSessionDoNotStealThePanel() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)

        XCTAssertNil(router.route(sessionID: "ctx-2", hasAction: false))
        XCTAssertEqual(router.boundSessionID, "ctx-1")
        XCTAssertEqual(router.route(sessionID: "ctx-1", hasAction: false), "ctx-1")
    }

    /// 「跟随最近活跃会话」：谁刚干了点什么，面板就切到谁。
    func testANewActionMovesTheOnlyPanelToThatSession() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)

        XCTAssertEqual(router.route(sessionID: "ctx-2", hasAction: true), "ctx-2")
        XCTAssertEqual(router.boundSessionID, "ctx-2")
        XCTAssertEqual(router.route(sessionID: "ctx-1", hasAction: true), "ctx-1")
    }

    /// 用户按下 ✕：面板**真的**丢掉，并且不会被下一帧弹回来——画面/标签变化正是他
    /// 关掉它的理由（2026-09-16 现场「关不掉」的老毛病不能在单面板改造里回来）。
    func testClosedPanelStaysGoneWhileFramesKeepArriving() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)

        router.dismiss(sessionID: "ctx-1")
        XCTAssertNil(router.boundSessionID)
        XCTAssertTrue(router.isSuppressed)
        XCTAssertNil(router.route(sessionID: "ctx-1", hasAction: false))
        XCTAssertNil(router.route(sessionID: "ctx-1", hasAction: false))
    }

    /// 关掉之后 Agent 又真的动了：面板回来（✕ 不是「永久屏蔽这个会话」）。
    func testANewActionBringsTheClosedPanelBack() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)
        router.dismiss(sessionID: "ctx-1")

        XCTAssertEqual(router.route(sessionID: "ctx-1", hasAction: true), "ctx-1")
        XCTAssertFalse(router.isSuppressed)
        XCTAssertTrue(router.dismissedSessionIDs.isEmpty)
        XCTAssertEqual(router.route(sessionID: "ctx-1", hasAction: false), "ctx-1")
    }

    /// 关掉的是唯一那块面板：屏幕上暂时什么都不放，别的会话光有画面也不亮起来。
    /// 「关掉」= 我不想再盯着小窗了，不是「换一个继续盯着」。
    func testClosingThePanelKeepsTheScreenEmptyUntilSomeNewAction() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)
        router.dismiss(sessionID: "ctx-1")

        XCTAssertNil(router.route(sessionID: "ctx-2", hasAction: false))
        XCTAssertEqual(router.route(sessionID: "ctx-2", hasAction: true), "ctx-2")
    }

    /// 会话真的结束了（引擎推 sessionClosed）：这条记录没有意义了。
    func testClosingASessionForgetsItAndDropsItsPanel() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)
        router.dismiss(sessionID: "ctx-1")

        router.forget(sessionID: "ctx-1")
        XCTAssertTrue(router.dismissedSessionIDs.isEmpty)
        XCTAssertNil(router.boundSessionID)
        // 「我不想看了」是用户的意思，不是这条会话记录的意思：屏幕保持空着，
        // 直到**某个**会话又有新动作。
        XCTAssertNil(router.route(sessionID: "ctx-2", hasAction: false))
        XCTAssertEqual(router.route(sessionID: "ctx-2", hasAction: true), "ctx-2")
    }

    /// 面板正放着 ctx-1 时 ctx-1 结束：面板跟着走，不放一张没人维护的陈旧画面。
    func testSessionClosedDropsThePanelThatWasShowingIt() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: true)

        router.forget(sessionID: "ctx-1")
        XCTAssertNil(router.boundSessionID)
        // bound 已清：下一个会话照常拿到这块面板。
        XCTAssertEqual(router.route(sessionID: "ctx-2", hasAction: false), "ctx-2")
    }

    /// 引擎重连 = 上一段链路的会话全部作废。
    func testResetClearsTheBindingAndTheDismissals() {
        var router = ViewerPanelRouter()
        _ = router.route(sessionID: "ctx-1", hasAction: false)
        router.dismiss(sessionID: "ctx-1")

        router.reset()
        XCTAssertNil(router.boundSessionID)
        XCTAssertFalse(router.isSuppressed)
        XCTAssertTrue(router.dismissedSessionIDs.isEmpty)
    }
}
