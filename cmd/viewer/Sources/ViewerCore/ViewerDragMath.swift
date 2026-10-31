import CoreGraphics

/// Pure maths for moving one of the shell's windows with the pointer.
///
/// `NSWindow.frame.origin` and `NSEvent.mouseLocation` live in the same screen
/// space (bottom-left origin), so "follow the pointer" is a plain subtraction:
/// the window keeps the offset from the pointer it had when the press started.
///
/// Measuring the pointer *inside the window* instead is wrong, because that
/// offset is relative to a window that is itself moving — which is exactly the
/// bug reported on 2026-09-12 ("鼠标移动了 100，窗口只移动了 40"): every event
/// re-applied only the last increment against the original origin, so the sheet
/// drifted behind the pointer at a fraction of its speed.
public enum ViewerDragMath {
    /// How far the pointer must travel before a press counts as a drag rather
    /// than a click (R4: click expands, drag moves).
    public static let dragThreshold: CGFloat = 3

    public static func origin(windowOrigin: CGPoint, mouseAtPress: CGPoint, mouseNow: CGPoint) -> CGPoint {
        CGPoint(
            x: windowOrigin.x + (mouseNow.x - mouseAtPress.x),
            y: windowOrigin.y + (mouseNow.y - mouseAtPress.y)
        )
    }

    public static func travelled(mouseAtPress: CGPoint, mouseNow: CGPoint) -> CGFloat {
        let dx = mouseNow.x - mouseAtPress.x
        let dy = mouseNow.y - mouseAtPress.y
        return (dx * dx + dy * dy).squareRoot()
    }
}
