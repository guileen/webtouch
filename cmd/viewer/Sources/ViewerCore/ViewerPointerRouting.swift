import CoreGraphics
import Foundation

/// Maps an engine-observed CDP pointer (viewport CSS pixels) onto the shell's
/// normalized drawing space.
///
/// The viewer never reads or moves the system pointer and never injects a DOM
/// element (R2/R6); it only draws what the engine reports. Keeping the mapping
/// pure lets it be unit tested without a window server.
public enum ViewerPointerRouting {
    public static func normalized(_ pointer: ViewerPointer, metadata: ViewerFrameMetadata?) -> CGPoint? {
        guard let metadata, metadata.deviceWidth > 0, metadata.deviceHeight > 0 else { return nil }
        let x = pointer.x / Double(metadata.deviceWidth)
        let y = pointer.y / Double(metadata.deviceHeight)
        return CGPoint(x: min(max(x, 0), 1), y: min(max(y, 0), 1))
    }

    /// True for the pointer kinds that should flash a click ripple.
    public static func isPress(_ kind: ViewerPointerKind) -> Bool {
        kind == .mousePressed
    }
}
