import CoreGraphics
import Foundation

/// 屏上一扇窗口的「判定所需的全部事实」：属主进程、层级、矩形、透明度。
///
/// 刻意不带窗口名/标题：那需要「屏幕录制」权限，而遮挡判定不需要；壳只按
/// **pid + layer + 矩形** 算，权限面最小（规范 §4.4 第 6 条）。
public struct ViewerWindowSnapshot: Equatable, Sendable {
    public let pid: pid_t
    public let layer: Int
    public let frame: CGRect
    public let alpha: Double
    /// `CGWindowID`（`kCGWindowNumber`）：这扇窗的**身份**。认过一次就按它算，
    /// 免得同 pid 下别的窗口把判定带偏（规范 §4.4 第 6/7 条）。0 = 调用方没提供。
    public let number: Int

    public init(pid: pid_t, layer: Int, frame: CGRect, alpha: Double, number: Int = 0) {
        self.pid = pid
        self.layer = layer
        self.frame = frame
        self.alpha = alpha
        self.number = number
    }
}

/// 「小窗该不该出现」的判据：真窗口**你看不看得见**（2026-09-24 第二轮现场口径，规范 §4.4
/// 第 5/6 条）。旧口径盯的是**前台应用**，那只是可见性的一个子集：真窗口失焦但仍然在屏幕上
/// 时，旧口径会再叠一个小窗，用户看到的就是重复遮挡。
///
/// 全部判定都是纯函数（`CGWindowListCopyWindowInfo` 只在 `onScreenWindows()` 里露一次脸），
/// 所以没有窗口服务器的环境也能把规则钉死在单测里。
public enum ViewerWindowVisibility {
    /// 可见面积占比达到这个数就算「你看得见」→ 小窗不该出现（§4.4 第 5 条：偏向「看得见」，
    /// 宁可少显示小窗，也不要在真窗口旁边多贴一块）。
    public static let visibleRatioThreshold: Double = 0.6

    /// 采样网格（N×N）。遮挡比例只需近似——这是给「要不要贴一块小窗」用的，不是给像素级
    /// 视觉回归用的。
    public static let samplingGrid = 24

    /// 小于这个面积的窗口不算遮挡者：菜单栏图标、状态指示条那些 10×18、34×24 的小窗不该
    /// 把一扇真页面算成「被挡住」。
    public static let minimumCovererArea: CGFloat = 200 * 150

    /// 矩形匹配的容差（px）：引擎报的是 Chrome 的窗口外框，`CGWindowList` 报的是同一扇窗的框。
    /// 真机实测两者逐像素一致（`72,49 1440×887`），留几像素只为浮点与夹取。
    public static let boundsMatchTolerance: CGFloat = 4

    /// 按矩形认出是哪一扇窗。**附着模式的命门**：用户自己的 Chrome（`--attach`）没有
    /// `browserPID`，只剩展开回执里的那扇窗矩形可用——认出来就等于补上了 pid，第 5 条
    /// 判据这才在用户的真实用法里生效（规范 §4.4 第 6 条）。
    public static func matchingWindow(
        bounds: CGRect,
        windows: [ViewerWindowSnapshot],
        tolerance: CGFloat = boundsMatchTolerance,
        pid expectedPID: pid_t? = nil
    ) -> ViewerWindowSnapshot? {
        guard bounds.width >= 1, bounds.height >= 1 else { return nil }
        var best: (window: ViewerWindowSnapshot, distance: CGFloat)?
        for window in windows
        where window.layer == 0 && window.alpha > 0.01
            && (expectedPID == nil || window.pid == expectedPID) {
            let dx = abs(window.frame.minX - bounds.minX)
            let dy = abs(window.frame.minY - bounds.minY)
            let dw = abs(window.frame.width - bounds.width)
            let dh = abs(window.frame.height - bounds.height)
            guard dx <= tolerance, dy <= tolerance, dw <= tolerance, dh <= tolerance else { continue }
            let distance = dx + dy + dw + dh
            if best == nil || distance < best!.distance {
                best = (window, distance)
            }
        }
        return best?.window
    }

    /// `matchingWindow` 的省事版：只要「这扇窗是谁的」。
    public static func hostPID(
        matching bounds: CGRect,
        windows: [ViewerWindowSnapshot],
        tolerance: CGFloat = boundsMatchTolerance
    ) -> pid_t? {
        matchingWindow(bounds: bounds, windows: windows, tolerance: tolerance)?.pid
    }

    /// 屏上所有窗口，**前 → 后**（`CGWindowList` 的序；同一 layer 里越靠前越在上）。
    /// 取不到（API 失败/权限受限）时返回空数组——调用方据此退回焦点判据。
    public static func onScreenWindows() -> [ViewerWindowSnapshot] {
        let options: CGWindowListOption = [.optionOnScreenOnly, .excludeDesktopElements]
        guard let raw = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] else {
            return []
        }
        return raw.compactMap { dict in
            guard let pidNumber = dict[kCGWindowOwnerPID as String] as? NSNumber else { return nil }
            let layer = (dict[kCGWindowLayer as String] as? NSNumber)?.intValue ?? 0
            let alpha = (dict[kCGWindowAlpha as String] as? NSNumber)?.doubleValue ?? 0
            var frame = CGRect.zero
            if let bounds = dict[kCGWindowBounds as String] as? [String: Any] {
                frame = CGRect(
                    x: (bounds["X"] as? NSNumber)?.doubleValue ?? 0,
                    y: (bounds["Y"] as? NSNumber)?.doubleValue ?? 0,
                    width: (bounds["Width"] as? NSNumber)?.doubleValue ?? 0,
                    height: (bounds["Height"] as? NSNumber)?.doubleValue ?? 0
                )
            }
            return ViewerWindowSnapshot(
                pid: pid_t(pidNumber.int32Value),
                layer: layer,
                frame: frame,
                alpha: alpha,
                number: (dict[kCGWindowNumber as String] as? NSNumber)?.intValue ?? 0
            )
        }
    }

    /// 目标窗：**认过窗的会话按那扇窗本身算**（`windowNumber` 是认窗时记下的 `CGWindowID`）；
    /// 还没认过（`nil`）才退回「该 pid 名下 `layer 0` 面积最大」。
    ///
    /// 为什么必须按窗口号：用户的 Chrome 常常同时开着好几扇同 pid 的大窗。2026-09-24 现场那台
    /// 机器 `pid=16742` 有两扇 ~887k px² 的窗——「盖住会话那扇、另一扇还露着」时，按面积最大
    /// 的那扇算会得出「看得见」，小窗反而不出现（规范 §4.4 第 6 条）。
    /// `nil` = 屏上没有它（最小化 / 已关 / 在别的 Space）→ 真窗口**看不见** → 小窗该出现。
    public static func targetWindow(
        windowNumber: Int?, browserPID: pid_t, windows: [ViewerWindowSnapshot]
    ) -> ViewerWindowSnapshot? {
        if let windowNumber {
            return windows.first {
                $0.number == windowNumber && $0.layer == 0 && $0.alpha > 0.01
                    && $0.frame.width >= 1 && $0.frame.height >= 1
            }
        }
        return realWindow(browserPID: browserPID, windows: windows)
    }

    /// 目标窗 = 该会话浏览器名下**面积最大**的那扇 `layer 0` 窗口（多窗时按主窗算）。
    /// `nil` = 屏上没有它的窗口（最小化 / 已关 / 在别的 Space）——那种情况下小窗**该**出现。
    public static func realWindow(
        browserPID: pid_t, windows: [ViewerWindowSnapshot]
    ) -> ViewerWindowSnapshot? {
        windows
            .filter {
                $0.pid == browserPID && $0.layer == 0 && $0.alpha > 0.01
                    && $0.frame.width >= 1 && $0.frame.height >= 1
            }
            .max { $0.frame.width * $0.frame.height < $1.frame.width * $1.frame.height }
    }

    /// 目标窗被挡掉的比例。
    ///
    /// 遮挡者 = **同一 layer** 里排在目标窗**之前**、且面积够大的窗口（`selfPID` 自己的窗口
    /// 一律不算）。两条实测教训写在规范 §4.4 第 6 条：① 不能按「层级高于目标」收集遮挡者——
    /// ChatGPT 的一扇 layer 3、1128×2079 的大窗会把**前台**窗口算成 66.7% 被挡；② 桌面小组件、
    /// 菜单栏图标必须在面积门槛上滤掉。
    public static func coverageRatio(
        target: ViewerWindowSnapshot,
        windows: [ViewerWindowSnapshot],
        selfPID: pid_t,
        grid: Int = samplingGrid
    ) -> Double {
        guard let index = windows.firstIndex(of: target) else { return 0 }
        let coverers = windows.prefix(index).filter {
            $0.layer == target.layer && $0.pid != selfPID && $0.alpha > 0.01
                && $0.frame.width * $0.frame.height >= minimumCovererArea
        }
        guard !coverers.isEmpty else { return 0 }
        let n = max(4, grid)
        let stepX = target.frame.width / CGFloat(n)
        let stepY = target.frame.height / CGFloat(n)
        var covered = 0
        for ix in 0..<n {
            for iy in 0..<n {
                let point = CGPoint(
                    x: target.frame.minX + (CGFloat(ix) + 0.5) * stepX,
                    y: target.frame.minY + (CGFloat(iy) + 0.5) * stepY
                )
                if coverers.contains(where: { $0.frame.contains(point) }) {
                    covered += 1
                }
            }
        }
        return Double(covered) / Double(n * n)
    }

    /// 真窗口此刻的可见面积占比。`nil` = 屏上没有它的窗口（小窗该出现）。
    public static func visibleRatio(
        browserPID: pid_t,
        selfPID: pid_t,
        windows: [ViewerWindowSnapshot],
        grid: Int = samplingGrid
    ) -> Double? {
        visibleRatio(
            windowNumber: nil, browserPID: browserPID, selfPID: selfPID, windows: windows, grid: grid
        )
    }

    /// 真窗口此刻的可见面积占比；`windowNumber` 非空时按**那扇窗**算（见 `targetWindow`）。
    public static func visibleRatio(
        windowNumber: Int?,
        browserPID: pid_t,
        selfPID: pid_t,
        windows: [ViewerWindowSnapshot],
        grid: Int = samplingGrid
    ) -> Double? {
        guard let target = targetWindow(
            windowNumber: windowNumber, browserPID: browserPID, windows: windows
        ) else { return nil }
        let covered = coverageRatio(target: target, windows: windows, selfPID: selfPID, grid: grid)
        return max(0, min(1, 1 - covered))
    }

    /// 「你看得见这扇真窗口吗」。`nil` = 屏上没有它的窗口 → 调用方按「看不见」处理
    /// （小窗出现）——见 `ViewerPanelController.pollPresence`。
    public static func isRealWindowVisible(
        browserPID: pid_t,
        selfPID: pid_t,
        windows: [ViewerWindowSnapshot],
        grid: Int = samplingGrid
    ) -> Bool? {
        visibleRatio(browserPID: browserPID, selfPID: selfPID, windows: windows, grid: grid)
            .map { $0 >= visibleRatioThreshold }
    }
}
