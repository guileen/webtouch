import AppKit

/// A procedurally drawn stand-in for a page frame.
///
/// The prototype has no approved producer yet, so the shell is demonstrated with
/// a clearly labelled mock page. It is drawn with AppKit only: no WebView, no
/// HTML, no network, no page assets.
enum DemoPage {
    static func image(size: CGSize = CGSize(width: 720, height: 464)) -> NSImage {
        NSImage(size: size, flipped: false) { rect in
            NSColor.white.setFill()
            rect.fill()

            let headerHeight: CGFloat = 58
            NSColor(calibratedRed: 0.969, green: 0.969, blue: 0.976, alpha: 1).setFill()
            NSRect(x: 0, y: rect.height - headerHeight, width: rect.width, height: headerHeight).fill()
            NSColor(calibratedWhite: 0.90, alpha: 1).setFill()
            NSRect(x: 0, y: rect.height - headerHeight, width: rect.width, height: 1).fill()

            let banner = NSAttributedString(string: "Register now to get 99 bonus coins", attributes: [
                .font: NSFont.systemFont(ofSize: 21, weight: .semibold),
                .foregroundColor: NSColor(calibratedRed: 0.17, green: 0.17, blue: 0.18, alpha: 1),
            ])
            banner.draw(at: NSPoint(x: 18, y: rect.height - 40))

            let pill = NSRect(x: rect.width - 18 - 104, y: rect.height - 46, width: 104, height: 34)
            NSColor(calibratedRed: 0.949, green: 0.627, blue: 0.678, alpha: 1).setFill()
            NSBezierPath(roundedRect: pill, xRadius: 17, yRadius: 17).fill()
            let register = NSAttributedString(string: "Register", attributes: [
                .font: NSFont.systemFont(ofSize: 16, weight: .semibold),
                .foregroundColor: NSColor.white,
            ])
            let registerSize = register.size()
            register.draw(at: NSPoint(x: pill.midX - registerSize.width / 2, y: pill.midY - registerSize.height / 2))

            let breadcrumb = NSAttributedString(string: "‹  Chapter 1", attributes: [
                .font: NSFont.systemFont(ofSize: 17, weight: .regular),
                .foregroundColor: NSColor(calibratedWhite: 0.56, alpha: 1),
            ])
            breadcrumb.draw(at: NSPoint(x: 18, y: rect.height - 92))

            let loading = NSAttributedString(string: "Loading", attributes: [
                .font: NSFont.systemFont(ofSize: 26, weight: .regular),
                .foregroundColor: NSColor(calibratedWhite: 0.85, alpha: 1),
            ])
            let loadingSize = loading.size()
            loading.draw(at: NSPoint(x: rect.midX - loadingSize.width / 2, y: rect.midY - loadingSize.height / 2))

            let chip = NSAttributedString(string: "DEMO FRAME", attributes: [
                .font: NSFont.systemFont(ofSize: 10, weight: .medium),
                .foregroundColor: NSColor(calibratedWhite: 0.72, alpha: 1),
            ])
            chip.draw(at: NSPoint(x: 18, y: 16))
            return true
        }
    }
}
