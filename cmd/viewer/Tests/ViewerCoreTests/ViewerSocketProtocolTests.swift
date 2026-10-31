import CoreGraphics
import XCTest

@testable import ViewerCore

final class ViewerSocketProtocolTests: XCTestCase {
    /// 壳写出去的每一行都必须和引擎那侧的字面量对得上（Go 侧对应
    /// pkg/viewerhub/intent_test.go 的 shellExpandLine）。
    func testExpandIntentLineMatchesTheEngineContract() throws {
        let intent = ViewerSocketIntent(
            intent: .expand,
            session: "ctx-1",
            size: ViewerWindowSize(width: 960, height: 620)
        )
        let line = intent.encodedLine()
        XCTAssertEqual(line.last, 0x0A, "意图必须以换行结尾（JSON lines）")

        let object = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: line) as? [String: Any]
        )
        XCTAssertEqual(object["version"] as? Int, 1)
        XCTAssertEqual(object["kind"] as? String, "intent")
        XCTAssertEqual(object["intent"] as? String, "expand")
        XCTAssertEqual(object["session"] as? String, "ctx-1")
        // 壳只说「要多大」，不说「放哪儿」：位置由引擎在它那扇窗上原地决定。
        let size = try XCTUnwrap(object["size"] as? [String: Any])
        XCTAssertEqual(size["width"] as? Int, 960)
        XCTAssertEqual(size["height"] as? Int, 620)
        XCTAssertNil(object["windowBounds"])
    }

    /// 收回只需要会话身份：窗口搬回哪去是引擎自己记着的事。
    func testCollapseIntentCarriesNoGeometry() throws {
        let line = ViewerSocketIntent(intent: .collapse, session: "ctx-9").encodedLine()
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: line) as? [String: Any])
        XCTAssertEqual(object["intent"] as? String, "collapse")
        XCTAssertEqual(object["session"] as? String, "ctx-9")
        XCTAssertNil(object["size"])
    }

    /// 回执里的矩形是 Chrome 窗口坐标（主屏左上原点、y 向下），落点判定要用 AppKit
    /// 坐标（左下原点、y 向上）。
    func testScreenGeometryFlipsTheVerticalAxis() {
        let bounds = ViewerWindowBounds(left: 940, top: 24, width: 960, height: 620)
        XCTAssertEqual(
            ViewerScreenGeometry.appKitFrame(from: bounds, primaryScreenHeight: 960),
            CGRect(x: 940, y: 316, width: 960, height: 620)
        )
    }

    /// 引擎随帧下发的真窗口矩形（规范 §4.4 第 7 条）：附着模式（没有 `browserPID`）靠它
    /// 在屏上认窗 → 认进程，而且**不等用户点小窗**，所以它必须能解码成 Chrome 坐标空间
    /// 里的那扇窗矩形。
    func testDecodesTheWindowBoundsOnAFrameUpdate() throws {
        let payload = Data(#"""
        {"version":1,"sequence":52,"session":{"id":"ctx-1","agentName":"UWS task-1","pageTitle":"example.test","url":"https://example.test/inbox"},"windowBounds":{"left":72,"top":49,"width":1440,"height":887}}
        """#.utf8)
        guard case let .update(update)? = ViewerSocketMessage.decode(jsonLine: payload) else {
            return XCTFail("帧更新应当被解析")
        }
        XCTAssertEqual(update.windowBounds?.rect, CGRect(x: 72, y: 49, width: 1440, height: 887))
        XCTAssertNil(update.browserPID, "附着模式没有 browserPID：判据全靠这块矩形")

        // 老引擎的老行（没有这个字段）照旧解得开，且不该凭空造出矩形。
        let legacy = Data(#"{"version":1,"sequence":52,"session":{"id":"ctx-1","agentName":"UWS task-1","pageTitle":"example.test","url":"https://example.test/inbox"}}"#.utf8)
        guard case let .update(old)? = ViewerSocketMessage.decode(jsonLine: legacy) else {
            return XCTFail("没有 windowBounds 的帧也应当被解析")
        }
        XCTAssertNil(old.windowBounds)
    }

    func testDecodesTheEngineAck() throws {
        let payload = Data(#"""
        {"version":1,"kind":"intentAck","session":"ctx-1","intent":"expand","ok":true,"windowBounds":{"left":1100,"top":280,"width":960,"height":620},"browserPID":4242}
        """#.utf8)
        guard case let .intentResult(ack)? = ViewerSocketMessage.decode(jsonLine: payload) else {
            return XCTFail("intentAck 行应当被解析成回执")
        }
        XCTAssertTrue(ack.ok)
        XCTAssertEqual(ack.intent, "expand")
        XCTAssertEqual(ack.session, "ctx-1")
        XCTAssertEqual(ack.browserPID, 4242)
        XCTAssertEqual(ack.windowBounds?.top, 280)
    }

    func testStillDecodesFrameUpdatesWithoutAKindField() throws {
        let payload = Data(#"""
        {"version":1,"sequence":3,"session":{"id":"ctx-1","agentName":"A","pageTitle":"Inbox","url":"https://example.test"},"browserPID":4242}
        """#.utf8)
        guard case let .update(update)? = ViewerSocketMessage.decode(jsonLine: payload) else {
            return XCTFail("帧更新仍应被解析")
        }
        XCTAssertEqual(update.sequence, 3)
        XCTAssertEqual(update.browserPID, 4242)
    }

    func testRejectsForeignPayloads() {
        XCTAssertNil(ViewerSocketMessage.decode(jsonLine: Data("not json".utf8)))
        XCTAssertNil(ViewerSocketMessage.decode(jsonLine: Data(#"{"kind":"intentAck","ok":true}"#.utf8)))
        XCTAssertNil(ViewerSocketMessage.decode(jsonLine: Data(#"{"version":9,"sequence":1,"session":{"id":"s"}}"#.utf8)))
    }

    /// 互斥不变量（2026-09-22 用户口径）：前台应用是这个会话的浏览器时小窗必须消失。
    /// 引擎拉起的窗口按 pid 认——别的应用（哪怕也叫 Chrome）不算。
    func testTheEngineLaunchedBrowserIsRecognisedByItsPID() {
        XCTAssertTrue(
            ViewerPanelPresence.isHostBrowser(
                frontmostPID: 4242, hostPID: 4242, bundleIdentifier: "com.apple.finder", localizedName: "访达"
            )
        )
        XCTAssertFalse(
            ViewerPanelPresence.isHostBrowser(
                frontmostPID: 999, hostPID: 4242, bundleIdentifier: "com.google.Chrome", localizedName: "Google Chrome"
            ),
            "同一个叫 Chrome 的应用在别的 pid 上：不是我们那扇真窗口"
        )
        XCTAssertFalse(
            ViewerPanelPresence.isHostBrowser(
                frontmostPID: nil, hostPID: 4242, bundleIdentifier: nil, localizedName: nil
            )
        )
    }

    /// 附着模式（CDP 附到用户自己开的 Chrome）没有 hostPID：只能按应用类别认——这是
    /// 刻意选的偏向，宁可多藏一次小窗，也不能让真窗口在前台时小窗还压在它上面。
    func testAnAttachedBrowserIsRecognisedByCategory() {
        for bundle in ["com.google.Chrome", "org.chromium.Chromium", "com.brave.Browser", "com.microsoft.edgemac"] {
            XCTAssertTrue(
                ViewerPanelPresence.isHostBrowser(
                    frontmostPID: 777, hostPID: nil, bundleIdentifier: bundle, localizedName: nil
                ),
                bundle
            )
        }
        // 认不出 bundle 的老版本/开发版按名字兜底。
        XCTAssertTrue(
            ViewerPanelPresence.isHostBrowser(
                frontmostPID: 777, hostPID: nil, bundleIdentifier: nil, localizedName: "Google Chrome Canary"
            )
        )
        XCTAssertFalse(
            ViewerPanelPresence.isHostBrowser(
                frontmostPID: 777, hostPID: nil, bundleIdentifier: "com.apple.Safari", localizedName: "Safari"
            )
        )
    }

    /// 前台轮询的三种决策：浏览器在台前就藏；浏览器让开就把「因为浏览器在台前才藏起来的
    /// 那块」亮回来；其余不动（用户 ✕ 掉的面板不该被复活）。
    func testPresenceDecisionsCoverTheThreeCases() {
        XCTAssertEqual(
            ViewerPanelPresence.decide(browserFrontmost: true, panelVisible: true, hiddenByBrowserFront: false),
            .hide
        )
        XCTAssertEqual(
            ViewerPanelPresence.decide(browserFrontmost: true, panelVisible: false, hiddenByBrowserFront: true),
            .unchanged
        )
        XCTAssertEqual(
            ViewerPanelPresence.decide(browserFrontmost: false, panelVisible: false, hiddenByBrowserFront: true),
            .show
        )
        XCTAssertEqual(
            ViewerPanelPresence.decide(browserFrontmost: false, panelVisible: true, hiddenByBrowserFront: false),
            .unchanged
        )
        XCTAssertEqual(
            ViewerPanelPresence.decide(browserFrontmost: false, panelVisible: false, hiddenByBrowserFront: false),
            .unchanged,
            "不是被浏览器挤下去的（用户 ✕ 过、或者根本没绑会话）：不许亮回来"
        )
    }

    /// 展开意图带 `deferRaise`：壳要「先飞动画、末段再抬」，引擎这一趟只摆窗口不激活
    /// （2026-09-24 第二轮现场口径，规范 §4.4 第 3 条）。字段名必须和 Go 侧
    /// pkg/viewerhub/intent.go 的 `deferRaise` 对上。
    func testExpandIntentCanDeferTheRaise() throws {
        let line = ViewerSocketIntent(intent: .expand, session: "ctx-1", deferRaise: true).encodedLine()
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: line) as? [String: Any])
        XCTAssertEqual(object["intent"] as? String, "expand")
        XCTAssertEqual(object["deferRaise"] as? Bool, true)
        XCTAssertNil(object["size"], "展开不再带尺寸：搬哪儿由引擎原地决定")
    }

    /// 老壳（不带 `deferRaise`）写的行必须还是老样子：引擎那边据此保持「收到 expand 就地抬」。
    func testExpandIntentWithoutDeferRaiseStaysBackwardsCompatible() throws {
        let line = ViewerSocketIntent(intent: .expand, session: "ctx-1").encodedLine()
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: line) as? [String: Any])
        XCTAssertEqual(object["intent"] as? String, "expand")
        XCTAssertNil(object["deferRaise"])
    }

    /// 末段那一条：`raise` 只说「现在把这扇真窗口抬到前台」，不带尺寸、不带 `deferRaise`。
    func testRaiseIntentCarriesOnlySessionIdentity() throws {
        let line = ViewerSocketIntent(intent: .raise, session: "ctx-1").encodedLine()
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: line) as? [String: Any])
        XCTAssertEqual(object["intent"] as? String, "raise")
        XCTAssertEqual(object["session"] as? String, "ctx-1")
        XCTAssertNil(object["size"])
        XCTAssertNil(object["deferRaise"])
    }

    /// 判据从焦点改成**可见性**（规范 §4.4 第 5 条）：真窗口看得见就让位，哪怕它不是前台
    /// 应用；真窗口看不见了才把小窗亮回来；算不出可见性（拿不到窗口列表）才退回焦点判据。
    func testGivesWayToAVisibleRealWindowEvenWhenItIsNotFrontmost() {
        XCTAssertEqual(
            ViewerPanelPresence.decide(
                realWindowVisible: true, browserFrontmost: false, panelVisible: true, hiddenByRealWindow: false
            ),
            .hide,
            "浏览器失焦、真窗口却明摆在屏幕上：小窗不该再叠上去"
        )
        XCTAssertEqual(
            ViewerPanelPresence.decide(
                realWindowVisible: false, browserFrontmost: true, panelVisible: false, hiddenByRealWindow: true
            ),
            .show,
            "真窗口看不见了（被挡住 / 最小化）：把让出去的小窗亮回来"
        )
        XCTAssertFalse(
            ViewerPanelPresence.givesWay(realWindowVisible: false, browserFrontmost: true),
            "可见性说了算：真窗口看不见就必须让小窗出现"
        )
        XCTAssertTrue(ViewerPanelPresence.givesWay(realWindowVisible: nil, browserFrontmost: true))
        XCTAssertFalse(ViewerPanelPresence.givesWay(realWindowVisible: nil, browserFrontmost: false))
    }
}
