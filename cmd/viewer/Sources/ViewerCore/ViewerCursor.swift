import AppKit
import CoreGraphics
import QuartzCore

/// Pure math for the native virtual cursor.
///
/// Page coordinates arrive top-left origin (CDP viewport CSS pixels) while
/// CALayer geometry is bottom-left origin, so the vertical flip lives here
/// instead of in drawing code, where it is unit testable.
public enum ViewerCursorMath {
    public static func normalized(_ point: CGPoint) -> CGPoint {
        CGPoint(x: min(max(point.x, 0), 1), y: min(max(point.y, 0), 1))
    }

    public static func layerPoint(normalized point: CGPoint, in size: CGSize) -> CGPoint {
        layerPoint(normalized: point, in: CGRect(origin: .zero, size: size))
    }

    /// Same vertical flip, but inside the rect the page is actually painted in.
    ///
    /// The page layer uses `.resizeAspect`, so whenever the frame's aspect ratio
    /// and the panel's page area differ, the picture is letterboxed inside
    /// `imageLayer.bounds`. Mapping into the full bounds then offsets the arrow
    /// by one bar: a few points at the default sizes, dozens of points as soon as
    /// the identity profile asks for a phone-shaped viewport (2026-09-13 评审).
    public static func layerPoint(normalized point: CGPoint, in rect: CGRect) -> CGPoint {
        let clamped = normalized(point)
        return CGPoint(
            x: rect.minX + clamped.x * rect.width,
            y: rect.minY + (1 - clamped.y) * rect.height
        )
    }

    /// Short hops follow tightly; longer jumps ease over a longer window so the
    /// eye can track the movement without adding perceived lag (R7).
    public static func animationDuration(from: CGPoint, to: CGPoint) -> CFTimeInterval {
        let dx = to.x - from.x
        let dy = to.y - from.y
        let distance = (dx * dx + dy * dy).squareRoot()
        if distance > 0.35 { return 0.20 }
        if distance > 0.05 { return 0.10 }
        return 0.06
    }
}

/// The native, high-contrast virtual pointer drawn above the page image.
///
/// Requirement mapping: R2 (never injected into the page DOM), R3 (always
/// visible), R7 (smooth follow). The layer anchor point is the arrow tip, so
/// animating `position` is enough for a smooth follow.
public final class ViewerCursorLayer: CALayer {
    /// Arrow outline in unit space, y-up, tip at (0, 1).
    public static let arrowUnitPoints: [CGPoint] = [
        CGPoint(x: 0.00, y: 1.00),
        CGPoint(x: 0.00, y: 0.28),
        CGPoint(x: 0.17, y: 0.44),
        CGPoint(x: 0.28, y: 0.10),
        CGPoint(x: 0.40, y: 0.15),
        CGPoint(x: 0.29, y: 0.49),
        CGPoint(x: 0.52, y: 0.50),
    ]

    public var cursorHeight: CGFloat = 24 { didSet { updateBounds() } }
    public private(set) var isPressed = false
    public private(set) var normalizedPoint = CGPoint(x: 0.5, y: 0.5)
    /// 收到过真实指针包没有。
    ///
    /// `normalizedPoint` 的默认 (0.5, 0.5) 只是"还没有数据"时的占位，一旦画出来就是
    /// 一根停在**面板正中**的假鼠标——用户会以为自动化卡住了（2026-09-23 现场反馈：
    /// 「我看到的鼠标（小窗）仍然在屏幕中间」）。所以没有指针数据时整层隐藏，
    /// 只由真实指针包唤醒；换会话/换页面后重新回到隐藏态。
    public private(set) var hasPointer = false

    public override init() {
        super.init()
        anchorPoint = CGPoint(x: 0, y: 1)
        contentsScale = NSScreen.main?.backingScaleFactor ?? 2
        isOpaque = false
        isHidden = true
        updateBounds()
    }

    public required init?(coder: NSCoder) { nil }

    /// CALayer copies itself through this initializer whenever an implicit
    /// animation is committed; without it AppKit traps on the pointer layer.
    public override init(layer: Any) {
        super.init(layer: layer)
        guard let other = layer as? ViewerCursorLayer else { return }
        cursorHeight = other.cursorHeight
        normalizedPoint = other.normalizedPoint
        isPressed = other.isPressed
        hasPointer = other.hasPointer
        isHidden = other.isHidden
    }

    public static func arrowPath(in bounds: CGRect) -> CGPath {
        let height = bounds.height
        let path = CGMutablePath()
        for (index, point) in arrowUnitPoints.enumerated() {
            let scaled = CGPoint(x: bounds.minX + point.x * height, y: bounds.minY + point.y * height)
            if index == 0 {
                path.move(to: scaled)
            } else {
                path.addLine(to: scaled)
            }
        }
        path.closeSubpath()
        return path
    }

    /// `size` is the page area the normalized point refers to.
    public func move(to point: CGPoint, in size: CGSize, animated: Bool) {
        move(to: point, in: CGRect(origin: .zero, size: size), animated: animated)
    }

    /// `rect` is the rect the page is painted into (letterbox aware).
    public func move(to point: CGPoint, in rect: CGRect, animated: Bool) {
        let next = ViewerCursorMath.normalized(point)
        normalizedPoint = next
        let target = ViewerCursorMath.layerPoint(normalized: next, in: rect)
        CATransaction.begin()
        CATransaction.setDisableActions(!animated)
        CATransaction.setAnimationDuration(ViewerCursorMath.animationDuration(from: position, to: target))
        CATransaction.setAnimationTimingFunction(CAMediaTimingFunction(name: .easeOut))
        position = target
        CATransaction.commit()
    }

    /// 真实指针包到达：这是唯一能让光标露面的入口。
    ///
    /// 与 `move(to:in:animated:)` 分开是故意的——布局/换帧也会调 move（那是纯定位），
    /// 不能顺带把一根还没有数据的光标显出来。
    public func reveal() {
        hasPointer = true
        isHidden = false
    }

    /// 换会话/换页面：上一页的位置已经没意义，重新回到"没有数据"。
    public func resetPointer() {
        hasPointer = false
        isHidden = true
    }

    public func setPressed(_ pressed: Bool, animated: Bool) {
        guard isPressed != pressed else { return }
        isPressed = pressed
        CATransaction.begin()
        CATransaction.setDisableActions(!animated)
        CATransaction.setAnimationDuration(0.08)
        transform = pressed ? CATransform3DMakeScale(0.88, 0.88, 1) : CATransform3DIdentity
        CATransaction.commit()
        setNeedsDisplay()
    }

    public override func draw(in context: CGContext) {
        let path = ViewerCursorLayer.arrowPath(in: bounds)

        context.saveGState()
        context.setStrokeColor(NSColor.systemBlue.withAlphaComponent(isPressed ? 0.75 : 0.40).cgColor)
        context.setLineWidth(4)
        context.setLineJoin(.round)
        context.addPath(path)
        context.strokePath()
        context.restoreGState()

        context.saveGState()
        context.setShadow(offset: CGSize(width: 0, height: -1.5), blur: 4, color: NSColor.black.withAlphaComponent(0.45).cgColor)
        context.setFillColor(NSColor(calibratedWhite: 0.07, alpha: 0.96).cgColor)
        context.addPath(path)
        context.fillPath()
        context.restoreGState()

        context.setStrokeColor(NSColor.white.withAlphaComponent(0.96).cgColor)
        context.setLineWidth(1.4)
        context.setLineJoin(.round)
        context.addPath(path)
        context.strokePath()
    }

    private func updateBounds() {
        bounds = CGRect(x: 0, y: 0, width: cursorHeight * 0.55, height: cursorHeight)
        setNeedsDisplay()
    }
}
