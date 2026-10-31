import AppKit
import CoreGraphics
import QuartzCore

/// One native canvas used by both presentations of the shell.
///
/// Everything on screen is drawn with AppKit/Core Graphics/CALayer: the page
/// image, the rounded card, the status pill, the expand affordance and the
/// virtual cursor. There is no WebView, no HTML and no page DOM injection (R6).
public final class ViewerCanvasView: NSView {
    public enum Mode {
        case compact
        case expanded
    }

    private var session: ViewerSession
    private let mode: Mode

    private let imageLayer = CALayer()
    private let cursorLayer = ViewerCursorLayer()
    private let rippleLayer = CAShapeLayer()
    private let barLayer = CALayer()
    private let titleLayer = CATextLayer()
    private let barActionLayer = CATextLayer()
    private let pillLayer = CALayer()
    private let pillTextLayer = CATextLayer()
    private let affordanceLayer = CAShapeLayer()
    private var collapseButton: NSButton?
    private var dismissButton: NSButton?

    public var onExpandRequested: (() -> Void)?
    public var onCollapseRequested: (() -> Void)?
    /// 小窗上的「关闭」：用户不想再盯着这个会话了。壳只收起自己的面板，绝不碰任务
    /// 与真窗口（2026-09-16 用户现场：uws 测试结束后小窗一直挂着，且没有关闭入口）。
    public var onDismissRequested: (() -> Void)?
    /// Fired after a user drag ends, so the host can snap the compact panel.
    public var onFrameChanged: (() -> Void)?
    /// Fired once when a press turns into a drag, so the host can stop any
    /// in-flight snap before it fights the pointer.
    public var onDragBegan: (() -> Void)?

    public private(set) var cursorNormalized = CGPoint(x: 0.5, y: 0.5)
    /// 虚拟光标当前是否可见：只有真实指针包把它唤醒后才该为 true（2026-09-23 现场反馈）。
    public var isCursorVisible: Bool { !cursorLayer.isHidden }
    private var actionText: String?
    private var isHovered = false
    private var trackingArea: NSTrackingArea?
    private var dragStart: NSPoint?
    private var dragWindowOrigin: NSPoint?
    private var dragMouseOrigin: NSPoint?
    private var didDrag = false
    /// Pixel size of the last frame, so overlays can be mapped into the rect the
    /// picture is actually painted in instead of the whole layer (2026-09-13).
    private var pageImageSize: CGSize?
    /// Engine link as last reported by the host; `.unknown` = nobody told us.
    private var linkState: ViewerLinkState = .unknown

    public init(session: ViewerSession, mode: Mode = .compact) {
        self.session = session
        self.mode = mode
        super.init(frame: .zero)

        let scale = NSScreen.main?.backingScaleFactor ?? 2
        wantsLayer = true
        let root = CALayer()
        root.cornerRadius = ViewerGeometry.cornerRadius
        root.cornerCurve = .continuous
        root.masksToBounds = true
        root.backgroundColor = NSColor.white.cgColor
        root.borderWidth = 1
        root.borderColor = NSColor.black.withAlphaComponent(0.16).cgColor
        layer = root

        imageLayer.contentsGravity = .resizeAspect
        imageLayer.backgroundColor = NSColor.white.cgColor
        imageLayer.contentsScale = scale
        root.addSublayer(imageLayer)

        rippleLayer.bounds = CGRect(x: 0, y: 0, width: 24, height: 24)
        rippleLayer.fillColor = NSColor.clear.cgColor
        rippleLayer.strokeColor = NSColor.systemBlue.withAlphaComponent(0.9).cgColor
        rippleLayer.lineWidth = 2
        rippleLayer.opacity = 0
        root.addSublayer(rippleLayer)
        root.addSublayer(cursorLayer)

        barLayer.backgroundColor = NSColor(calibratedWhite: 0.09, alpha: 0.92).cgColor
        titleLayer.contentsScale = scale
        titleLayer.font = NSFont.systemFont(ofSize: 12.5, weight: .semibold)
        titleLayer.fontSize = 12.5
        titleLayer.foregroundColor = NSColor.white.withAlphaComponent(0.92).cgColor
        titleLayer.truncationMode = .end
        titleLayer.string = "\(session.agentName) · \(session.pageTitle)"
        barActionLayer.contentsScale = scale
        barActionLayer.font = NSFont.systemFont(ofSize: 11.5, weight: .regular)
        barActionLayer.fontSize = 11.5
        barActionLayer.foregroundColor = NSColor.white.withAlphaComponent(0.7).cgColor
        barActionLayer.alignmentMode = .right
        barActionLayer.truncationMode = .start
        barLayer.addSublayer(titleLayer)
        barLayer.addSublayer(barActionLayer)
        root.addSublayer(barLayer)

        pillLayer.backgroundColor = NSColor(calibratedWhite: 0.08, alpha: 0.55).cgColor
        pillLayer.cornerRadius = 7
        pillTextLayer.contentsScale = scale
        pillTextLayer.font = NSFont.systemFont(ofSize: 10.5, weight: .medium)
        pillTextLayer.fontSize = 10.5
        pillTextLayer.foregroundColor = NSColor.white.withAlphaComponent(0.95).cgColor
        pillTextLayer.truncationMode = .end
        pillTextLayer.string = statusText
        pillLayer.addSublayer(pillTextLayer)
        root.addSublayer(pillLayer)

        affordanceLayer.fillColor = NSColor.clear.cgColor
        affordanceLayer.strokeColor = NSColor.white.withAlphaComponent(0.9).cgColor
        affordanceLayer.lineWidth = 2
        affordanceLayer.lineCap = .round
        root.addSublayer(affordanceLayer)

        if mode == .compact {
            // 小窗唯一的「关掉它」入口。刻意常显而不是悬停才出现：用户已经抱怨过
            // 「关不掉」，藏起来的关闭键等于没有。图标是实心圆 + 叉，落在任何页面
            // 画面上都读得出来；点击只收面板，不动任务、不动那扇真窗口。
            let button = FirstMouseButton(title: "", target: self, action: #selector(dismissTapped))
            button.isBordered = false
            button.imagePosition = .imageOnly
            button.image = NSImage(
                systemSymbolName: "xmark.circle.fill",
                accessibilityDescription: "关闭小窗（不影响任务）"
            )
            button.contentTintColor = .white
            button.toolTip = "关闭小窗（不影响任务与真窗口）"
            addSubview(button)
            dismissButton = button
        }

        if mode == .expanded {
            let button = FirstMouseButton(title: "", target: self, action: #selector(collapseTapped))
            button.isBordered = false
            button.imagePosition = .imageOnly
            button.image = NSImage(
                systemSymbolName: "arrow.down.right.and.arrow.up.left",
                accessibilityDescription: "收回小窗"
            )
            button.contentTintColor = .white
            button.toolTip = "收回小窗"
            addSubview(button)
            collapseButton = button
        }

        updateChromeVisibility()
    }

    public required init?(coder: NSCoder) { nil }

    // MARK: - Public API

    public func display(frame image: NSImage) {
        // 画面每秒要换十来次，而 CALayer 给 contents 赋值会套一层隐式动画
        // （默认 0.25s 交叉淡入）：每帧都在和上一帧叠影，看过去就是「画面在闪」。
        // 实时画面只要最新帧，不要过渡。
        CATransaction.begin()
        CATransaction.setDisableActions(true)
        let imageRef = image.cgImage(forProposedRect: nil, context: nil, hints: nil)
        imageLayer.contents = imageRef
        CATransaction.commit()
        if let imageRef {
            pageImageSize = CGSize(width: CGFloat(imageRef.width), height: CGFloat(imageRef.height))
        }
        layoutCursor()
    }

    public func display(jpeg: Data) {
        guard let image = NSImage(data: jpeg) else { return }
        display(frame: image)
    }

    /// Page-space normalized coordinates; the native layer owns the drawing and
    /// never reads or moves the system pointer.
    public func showVirtualCursor(x: CGFloat, y: CGFloat, clicked: Bool) {
        moveCursor(to: CGPoint(x: x, y: y), animated: true)
        // 真实指针包才让光标露面（默认位置＝面板正中，先画出来就是一根假鼠标）。
        cursorLayer.reveal()
        if clicked { animateRipple(at: cursorNormalized) }
    }

    public func moveCursor(to normalized: CGPoint, animated: Bool) {
        cursorNormalized = ViewerCursorMath.normalized(normalized)
        cursorLayer.move(to: cursorNormalized, in: presentedPageRect, animated: animated)
    }

    /// The host reports the engine link, so the shell can say "正在重连" instead of
    /// presenting the last frame as if it were still live.
    public func setConnectionState(_ link: ViewerLinkState) {
        guard linkState != link else { return }
        linkState = link
        updateStatusText()
    }

    /// The rect inside the page layer the last frame was painted into.
    private var presentedPageRect: CGRect {
        guard let pageImageSize else { return imageLayer.bounds }
        return ViewerGeometry.aspectFitRect(contentSize: pageImageSize, in: imageLayer.bounds)
    }

    private func layoutCursor(animated: Bool = false) {
        cursorLayer.move(to: cursorNormalized, in: presentedPageRect, animated: animated)
    }

    public func setCursorPressed(_ pressed: Bool, animated: Bool = true) {
        cursorLayer.setPressed(pressed, animated: animated)
    }

    public func setAction(_ text: String?) {
        actionText = text
        updateStatusText()
    }

    /// Engine headers change while a session runs (navigation, new tab title),
    /// so the shell keeps its labels live instead of freezing the first frame's.
    public func update(session: ViewerSession) {
        let switched = session.id != self.session.id
        self.session = session
        titleLayer.string = "\(session.agentName) · \(session.pageTitle)"
        updateStatusText()
        // 换会话＝换页面：上一页的光标位置对新页面没有意义，收回隐藏态等真实指针包。
        if switched { cursorLayer.resetPointer() }
    }

    // MARK: - Layout

    public override func layout() {
        super.layout()
        guard let root = layer else { return }
        let barHeight: CGFloat = mode == .expanded ? 40 : 0
        let pageHeight = max(0, bounds.height - barHeight)
        imageLayer.frame = CGRect(x: 0, y: 0, width: bounds.width, height: pageHeight)
        rippleLayer.bounds = CGRect(x: 0, y: 0, width: 24, height: 24)
        layoutCursor()

        barLayer.frame = CGRect(x: 0, y: pageHeight, width: bounds.width, height: barHeight)
        titleLayer.frame = CGRect(x: 88, y: (barHeight - 16) / 2, width: max(0, bounds.width - 88 - 250), height: 16)
        barActionLayer.frame = CGRect(x: bounds.width - 300, y: (barHeight - 15) / 2, width: 250, height: 15)
        collapseButton?.frame = CGRect(x: bounds.width - 34, y: pageHeight + 10, width: 20, height: 20)
        dismissButton?.frame = CGRect(x: bounds.width - 30, y: bounds.height - 30, width: 22, height: 22)

        layoutPill()
        layoutAffordance(pageHeight: pageHeight)
        root.setNeedsLayout()
    }

    private func layoutPill() {
        let attributed = NSAttributedString(string: statusText, attributes: [.font: NSFont.systemFont(ofSize: 10.5, weight: .medium)])
        let textWidth = ceil(attributed.size().width)
        let width = min(textWidth + 18, max(0, bounds.width - 24))
        let frame = CGRect(x: 12, y: 12, width: width, height: 20)
        pillLayer.frame = frame
        pillTextLayer.frame = CGRect(x: 9, y: 3.5, width: max(0, width - 18), height: 13)
    }

    private func layoutAffordance(pageHeight: CGFloat) {
        let length: CGFloat = 9
        let inset: CGFloat = 10
        let path = CGMutablePath()
        path.move(to: CGPoint(x: bounds.width - inset, y: pageHeight - inset - length))
        path.addLine(to: CGPoint(x: bounds.width - inset, y: pageHeight - inset))
        path.addLine(to: CGPoint(x: bounds.width - inset - length, y: pageHeight - inset))
        path.move(to: CGPoint(x: inset + length, y: inset))
        path.addLine(to: CGPoint(x: inset, y: inset))
        path.addLine(to: CGPoint(x: inset, y: inset + length))
        affordanceLayer.path = path
        affordanceLayer.frame = bounds
    }

    private func updateChromeVisibility() {
        let showsBar = mode == .expanded
        barLayer.isHidden = !showsBar
        pillLayer.isHidden = showsBar
        affordanceLayer.isHidden = showsBar || !isHovered
        layer?.borderColor = (isHovered && mode == .compact
            ? NSColor.white.withAlphaComponent(0.55)
            : NSColor.black.withAlphaComponent(0.16)).cgColor
    }

    private func updateStatusText() {
        let text = statusText
        pillTextLayer.string = text
        barActionLayer.string = text
        layoutPill()
    }

    private var statusText: String {
        if let warning = ViewerConnectionLabel.text(for: linkState) {
            return "\(session.agentName) · \(warning)"
        }
        let action = actionText ?? session.action ?? ""
        if action.isEmpty { return "\(session.agentName) · 观看中" }
        return "\(session.agentName) · \(action)"
    }

    // MARK: - Ripple

    private func animateRipple(at normalized: CGPoint) {
        let point = ViewerCursorMath.layerPoint(normalized: normalized, in: presentedPageRect)
        rippleLayer.removeAllAnimations()
        rippleLayer.position = point
        rippleLayer.path = CGPath(ellipseIn: CGRect(x: 0, y: 0, width: 24, height: 24), transform: nil)
        rippleLayer.opacity = 1

        let scale = CABasicAnimation(keyPath: "transform.scale")
        scale.fromValue = 0.4
        scale.toValue = 2.6
        let fade = CABasicAnimation(keyPath: "opacity")
        fade.fromValue = 0.95
        fade.toValue = 0
        let group = CAAnimationGroup()
        group.animations = [scale, fade]
        group.duration = 0.36
        group.timingFunction = CAMediaTimingFunction(name: .easeOut)
        rippleLayer.add(group, forKey: "ripple")
        rippleLayer.opacity = 0
    }

    // MARK: - Mouse

    public override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }

    public override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let trackingArea { removeTrackingArea(trackingArea) }
        let area = NSTrackingArea(
            rect: bounds,
            options: [.mouseEnteredAndExited, .mouseMoved, .activeAlways, .inVisibleRect],
            owner: self,
            userInfo: nil
        )
        addTrackingArea(area)
        trackingArea = area
    }

    public override func mouseEntered(with event: NSEvent) {
        isHovered = true
        updateChromeVisibility()
    }

    public override func mouseExited(with event: NSEvent) {
        isHovered = false
        updateChromeVisibility()
    }

    public override func mouseDown(with event: NSEvent) {
        dragStart = event.locationInWindow
        dragWindowOrigin = window?.frame.origin
        // 拖窗一律用屏幕坐标：窗口坐标系里的位移会随窗口自身移动而缩水，实测
        // 「鼠标走 100、窗口只走 40」（2026-09-12 用户现场反馈）。
        dragMouseOrigin = NSEvent.mouseLocation
        didDrag = false
    }

    public override func mouseDragged(with event: NSEvent) {
        guard let start = dragStart,
              let origin = dragWindowOrigin,
              let mouseOrigin = dragMouseOrigin,
              let window,
              allowsWindowDrag(fromWindowPoint: start)
        else { return }
        let now = NSEvent.mouseLocation
        if !didDrag, ViewerDragMath.travelled(mouseAtPress: mouseOrigin, mouseNow: now) > ViewerDragMath.dragThreshold {
            didDrag = true
            onDragBegan?()
        }
        guard didDrag else { return }
        window.setFrameOrigin(
            ViewerDragMath.origin(windowOrigin: origin, mouseAtPress: mouseOrigin, mouseNow: now)
        )
    }

    /// Compact: the whole card is a drag handle. Expanded: only the top bar is,
    /// so page-area clicks stay free for future input forwarding.
    private func allowsWindowDrag(fromWindowPoint point: NSPoint) -> Bool {
        switch mode {
        case .compact:
            return true
        case .expanded:
            let local = convert(point, from: nil)
            return local.y >= bounds.height - 40
        }
    }

    public override func mouseUp(with event: NSEvent) {
        defer {
            dragStart = nil
            dragWindowOrigin = nil
            dragMouseOrigin = nil
        }
        if didDrag {
            onFrameChanged?()
            return
        }
        if mode == .compact { onExpandRequested?() }
    }

    @objc private func collapseTapped() {
        onCollapseRequested?()
    }

    @objc private func dismissTapped() {
        onDismissRequested?()
    }


    // MARK: - Placeholder

    public static func placeholderImage(size: CGSize = ViewerGeometry.compactSize) -> NSImage {
        NSImage(size: size, flipped: false) { rect in
            NSColor(calibratedWhite: 0.97, alpha: 1).setFill()
            rect.fill()
            let text = "等待已批准的帧源"
            let attributes: [NSAttributedString.Key: Any] = [
                .font: NSFont.systemFont(ofSize: 13),
                .foregroundColor: NSColor.secondaryLabelColor,
            ]
            let measured = (text as NSString).size(withAttributes: attributes)
            (text as NSString).draw(
                at: NSPoint(x: rect.midX - measured.width / 2, y: rect.midY - measured.height / 2),
                withAttributes: attributes
            )
            return true
        }
    }
}

/// A control in a never-key window must accept the very first click, otherwise
/// the shell would have to activate itself (violating R1) before collapsing.
final class FirstMouseButton: NSButton {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}
