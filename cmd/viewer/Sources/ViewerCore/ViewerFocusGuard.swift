import CoreGraphics
import Foundation

/// The two ways the user leaves the expanded shell, both of which must fold it
/// back into the compact sheet.
///
/// User feedback (2026-09-12): "当鼠标点击时，它放大得很好；当我重新回到其他窗口、
/// 它失去焦点时，它应该自动缩小。" So collapse is driven by *focus loss*, never by a
/// timer: the shell is a review surface, and staying expanded while the user
/// works somewhere else would cover their screen for no reason.
public enum ViewerFocusLoss: String, Equatable, Sendable {
    /// Another application came to the front — the user clicked back into their
    /// own work, or the real Chrome window was raised on purpose (「切真窗口」).
    case otherApplicationActivated
    /// A click landed outside every one of the shell's own windows.
    case clickOutsideShell
}

/// Pure decision rule for auto-collapse.
///
/// Deliberately AppKit-free so the rule — including the grace period that
/// protects the very click which expanded the shell — is unit testable without
/// a window server.
public struct ViewerFocusGuard: Equatable, Sendable {
    /// Expansion and collapse are animated; a collapse fired by the same
    /// gesture that expanded the shell would look like a flicker.
    public var graceInterval: TimeInterval
    private var expandedAt: TimeInterval?

    public init(graceInterval: TimeInterval = 0.7) {
        self.graceInterval = graceInterval
    }

    public var isExpanded: Bool { expandedAt != nil }

    public mutating func noteExpanded(at now: TimeInterval) { expandedAt = now }
    public mutating func noteCollapsed() { expandedAt = nil }

    /// Returns the reason to collapse, or nil when the shell must stay as is.
    public func collapseReason(for loss: ViewerFocusLoss, at now: TimeInterval) -> ViewerFocusLoss? {
        guard let expandedAt, now - expandedAt >= graceInterval else { return nil }
        return loss
    }

    /// True when a screen-space point lies inside one of the shell's own
    /// windows. `NSEvent.mouseLocation` and `NSWindow.frame` share the
    /// bottom-left screen origin, so a plain containment test is exact.
    public static func isInsideShell(_ point: CGPoint, frames: [CGRect]) -> Bool {
        frames.contains { $0.contains(point) }
    }
}
