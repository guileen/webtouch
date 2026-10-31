import AppKit
import Foundation

/// 「展开真的把浏览器抬到前台」的第二步（2026-09-24 用户口径，规范 §4.4 第 3 条）。
///
/// 引擎那一步 `Target.activateTarget` 是 Chrome **内部**的激活（把这扇窗口里的目标 Tab 与
/// 窗口设为活动）。**订正（2026-09-24 复核）**：上一版这里写「这一步在 macOS 上不足以把 App
/// 抬到最前」，已被隔离取证**证伪** —— 自建会话 3/3 试次里，点击后 0.017–0.026s 前台就切到
/// 该浏览器进程，早于壳这一步约 0.07s。所以引擎那一步在隔离环境里**够用**，壳这一步的定位是
/// **真实场景兜底**：前台是同 bundle 的**另一个 Chrome 实例**（用户自己的）时，只有按 pid 才
/// 指得到本会话的那一只。
///
/// 时刻（2026-09-24 第二轮口径）：这一步**不在**动画起步前做，而是由
/// `flyAwayAndHide(raiseAtEnd:)` 排在动画末段（结束前 `ViewerGeometry.raiseLeadTime`）——
/// 先抬窗口再播动画会演成「窗口先到前台、小窗动画后到」两段脱节。
///
/// 边界（R1 的唯一例外，且只有「用户点小窗」这一个手势会走到这里）：
/// - 只按 **`browserPID`** 激活，也就是本会话**自己那一个**浏览器进程；
/// - **绝不按 bundle id 激活**（会命中用户自己的 Chrome，2026-09-13 评审已拒绝，规范 §7.4）；
/// - 附着模式拿不到 pid（引擎回 0）、无头会话没有窗口：都不动手，交给引擎那一步。
///
/// 「该激活谁」拆成纯函数，好在没有窗口服务器的环境里也能断言。
public enum ViewerAppActivation {
    /// 该激活哪个进程：只有引擎报回来的正数 pid 才算数（附着模式与无头会话都是 0/nil）。
    public static func activationPID(browserPID: Int?) -> pid_t? {
        guard let browserPID, browserPID > 0 else { return nil }
        return pid_t(browserPID)
    }

    /// 把这个浏览器进程抬到最前。进程已经不在了就返回 `false`（不抛错、不阻塞）。
    @discardableResult
    public static func activate(processIdentifier pid: pid_t) -> Bool {
        guard let app = NSRunningApplication(processIdentifier: pid) else { return false }
        return app.activate(options: [.activateIgnoringOtherApps])
    }

    /// 展开链路的接线点：拿到 `ok=true` 回执、且这次展开确实要交棒给真窗口时按 pid 抬一次，
    /// 并把结果写进壳日志（规范 §4.4 第 4 条要求这条日志可读）。
    @discardableResult
    public static func activateBrowser(browserPID: Int?) -> Bool {
        guard let pid = activationPID(browserPID: browserPID) else { return false }
        let ok = activate(processIdentifier: pid)
        NSLog("uws-viewer activate browser: pid=%d ok=%@", pid, ok ? "true" : "false")
        return ok
    }
}
