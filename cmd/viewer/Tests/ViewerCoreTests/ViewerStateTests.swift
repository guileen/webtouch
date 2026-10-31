import XCTest
@testable import ViewerCore

final class ViewerStateTests: XCTestCase {
    func testExpansionIsOnlyAnIntent() {
        let session = ViewerSession(id: "s-1", agentName: "Agent", pageTitle: "Page", url: "https://example.test")
        let state = ViewerState(session: session)
        var received: ViewerIntent?
        state.onIntent = { received = $0 }

        state.requestExpansion()

        XCTAssertEqual(received, .requestExpansion(sessionID: "s-1"))
    }
}
