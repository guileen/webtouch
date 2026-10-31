import CoreGraphics
import Foundation

/// The two user-visible presentations of the viewer shell.
///
/// Only an explicit user gesture may move `compact` -> `expanded` (R4), and the
/// compact panel can never take keyboard focus (R1).
public enum ViewerPresentation: String, Equatable, Sendable {
    case compact
    case expanded
}

/// Pure, AppKit-free geometry for both presentations.
///
/// Keeping the acceptance math (R3 top-right anchor, R4 expansion bounds,
/// on-screen clamping) outside the window layer lets it be unit tested without a
/// window server, which is how this prototype is verified in CI.
public enum ViewerGeometry {
    /// Matches the ChatGPT desktop mini window proportions used as the visual
    /// reference: a rounded page card of roughly 1.55:1.
    public static let compactSize = CGSize(width: 400, height: 258)
    public static let expandedSize = CGSize(width: 960, height: 620)
    public static let cornerRadius: CGFloat = 11
    public static let margin: CGFloat = 18
    public static let minimumSize = CGSize(width: 240, height: 160)
    public static let edgeSnapThreshold: CGFloat = 28

    /// 点开链路的动画时长（秒）：快、流畅、「嗖的一下」（2026-09-24 用户口径，规范 §4.4）。
    /// 「飞向真窗口」与「回落自绘放大面板」共用这一个值；动画里只插值 `frame`/`alpha`，
    /// 不做测量、更不做 CDP 往返。
    public static let expandDuration: TimeInterval = 0.18

    /// 真窗口到前台的时刻 = 动画**末段**：结束前 `raiseLeadTime`（2026-09-24 第二轮现场口径，
    /// 规范 §4.4 第 3 条）。先抬窗口再播动画会让用户看到「窗口先到、动画后到」两段脱节。
    public static let raiseLeadTime: TimeInterval = 0.03

    /// 从动画起步到「抬真窗口」这一下的延时（隔离环境实测窗口到前台约 20–30ms，正好落在末帧上）。
    ///
    /// **这一下必须排在非主线程**（`ViewerPanelController.raiseQueue`）：展开那一瞬间主线程在
    /// 解码引擎推进来的大帧，`DispatchQueue.main.asyncAfter` 真机实测被推迟到 0.19–0.68s，
    /// 窗口因此落在动画演完之后——又变回「两段脱节」（§4.4 第 3 条）。
    public static var raiseDelay: TimeInterval { max(0, expandDuration - raiseLeadTime) }

    /// 「飞向真窗口」动画的落点：**就是那扇真窗口自己的矩形**（2026-09-24 用户口径：
    /// 小窗的四个角向外飞到真窗口的四个角）。
    ///
    /// 这里刻意不做任何自造几何：2026-09-22 的实现把落点算成「中心对齐 + 缩到一半」
    /// （`start.width / 2`），用户看到的因此是一段**缩小**过程而不是飞出去。
    /// 回执没带矩形（或矩形为空）时不猜：落点 = 当前小窗矩形，动画退化为纯淡出。
    public static func flyAwayDestination(from start: CGRect, toward target: CGRect) -> CGRect {
        guard target.width > 0, target.height > 0 else { return start }
        return target
    }

    /// Compact sheet hugging the top-right corner of the visible frame, the
    /// position used by the reference screenshot.
    public static func compactFrame(inVisibleFrame visible: CGRect, size: CGSize = compactSize) -> CGRect {
        let width = min(max(size.width, minimumSize.width), max(visible.width, minimumSize.width))
        let height = min(max(size.height, minimumSize.height), max(visible.height, minimumSize.height))
        return clamped(
            CGRect(x: visible.maxX - margin - width, y: visible.maxY - margin - height, width: width, height: height),
            inVisibleFrame: visible
        )
    }

    /// Expand keeping the top-right corner of the compact sheet as the visual
    /// anchor, then clamp the result into the same visible frame.
    public static func expandedFrame(from compactFrame: CGRect, inVisibleFrame visible: CGRect) -> CGRect {
        let width = min(expandedSize.width, max(visible.width - margin * 2, minimumSize.width))
        let height = min(expandedSize.height, max(visible.height - margin * 2, minimumSize.height))
        let corner = CGPoint(x: compactFrame.maxX, y: compactFrame.maxY)
        return clamped(
            CGRect(x: corner.x - width, y: corner.y - height, width: width, height: height),
            inVisibleFrame: visible
        )
    }

    /// Keep a dragged frame fully on screen, then pull it to the nearest
    /// horizontal edge when it lands close enough to feel intentional.
    public static func snapped(_ frame: CGRect, inVisibleFrame visible: CGRect, threshold: CGFloat = edgeSnapThreshold) -> CGRect {
        var result = clamped(frame, inVisibleFrame: visible)
        if result.minX - visible.minX <= threshold {
            result.origin.x = visible.minX + margin
        } else if visible.maxX - result.maxX <= threshold {
            result.origin.x = visible.maxX - margin - result.width
        }
        if result.minY - visible.minY <= threshold {
            result.origin.y = visible.minY + margin
        }
        return clamped(result, inVisibleFrame: visible)
    }

    public static func clamped(_ frame: CGRect, inVisibleFrame visible: CGRect) -> CGRect {
        guard visible.width > 0, visible.height > 0 else { return frame }
        var result = frame
        result.size.width = min(result.width, visible.width)
        result.size.height = min(result.height, visible.height)
        result.origin.x = min(max(result.minX, visible.minX), max(visible.maxX - result.width, visible.minX))
        result.origin.y = min(max(result.minY, visible.minY), max(visible.maxY - result.height, visible.minY))
        return result
    }

    /// The rect a `.resizeAspect` layer actually paints inside its own bounds.
    ///
    /// `contentsGravity = .resizeAspect` letterboxes whenever the frame's aspect
    /// ratio differs from the panel's page area — and it always differs a little
    /// (default compact 400×258 = 1.55 vs a 1440×900 viewport = 1.60) and a lot
    /// once the profile asks for a phone viewport. Pointer coordinates are
    /// normalized to the *page*, so overlays must map into this rect instead of
    /// the layer's full bounds (2026-09-13 评审：光标与涟漪按整块 bounds 画，
    /// 会让「画出来的指针」与「画面里的位置」差一条信箱边）。
    public static func aspectFitRect(contentSize: CGSize, in bounds: CGRect) -> CGRect {
        guard contentSize.width > 0, contentSize.height > 0, bounds.width > 0, bounds.height > 0 else {
            return bounds
        }
        let scale = min(bounds.width / contentSize.width, bounds.height / contentSize.height)
        let size = CGSize(width: contentSize.width * scale, height: contentSize.height * scale)
        return CGRect(
            x: bounds.midX - size.width / 2,
            y: bounds.midY - size.height / 2,
            width: size.width,
            height: size.height
        )
    }
}

/// 小窗的「槽位」：它自己的家在哪儿（规范 §4.4 第 9 条）。**唯一的写者是用户**——
/// 拖动结束即记账；吸附只决定「要不要顺手把落点夹进可见区 / 贴边」，与记不记账无关。
///
/// 2026-09-24 用户现场缺口：记账当时挂在「吸附确实改了坐标」这条分支上，于是把面板拖到屏幕
/// 中间（无需吸附）再点开，飞走重现身时小窗回到了**移动前**的位置。这个类型把那条规则钉在
/// 纯函数里（`ViewerPanelSlotTests`），壳只读 `frame`。
public struct ViewerPanelSlot: Equatable {
    /// 小窗被藏起来之后要回去的矩形。
    public private(set) var frame: CGRect

    public init(frame: CGRect) {
        self.frame = frame
    }

    /// 用户**放下**了面板：先记账、再谈吸附。返回值是吸附后的落点（调用方据此摆窗口），
    /// 它同时也是新的槽位——没有吸附也一样（`dropped` 原样入账）。
    @discardableResult
    public mutating func noteUserDrop(_ dropped: CGRect, inVisibleFrame visible: CGRect) -> CGRect {
        frame = ViewerGeometry.snapped(dropped, inVisibleFrame: visible)
        return frame
    }

    /// 程序或系统造成的位移（macOS 把窗口夹回屏内、窗口被缩放）：槽位跟着走，免得下次回到旧位。
    public mutating func noteProgrammaticFrame(_ frame: CGRect) {
        self.frame = frame
    }
}
