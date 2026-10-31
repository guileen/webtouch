// macOS 取证小工具（过程存证用）
//   macprobe front            → 打印当前前台应用名（无需自动化权限）
//   macprobe screens          → 打印每块屏幕的 frame / visibleFrame（判断窗口是否真的在屏幕外）
//   macprobe list             → 打印所有窗口：id<TAB>owner<TAB>WxH<TAB>X,Y<TAB>title
// 用途：对"被遮挡/后台"的窗口也能取证（screencapture -l <id>），并可证明前台应用未被抢。
import AppKit

let args = CommandLine.arguments
let mode = args.count > 1 ? args[1] : "list"

if mode == "front" {
    print(NSWorkspace.shared.frontmostApplication?.localizedName ?? "-")
    exit(0)
}

if mode == "screens" {
    for (i, s) in NSScreen.screens.enumerated() {
        let f = s.frame, v = s.visibleFrame
        let main = (s == NSScreen.main) ? "main" : "-"
        print("\(i)\t\(main)\tframe=\(Int(f.minX)),\(Int(f.minY)) \(Int(f.width))x\(Int(f.height))\tvisible=\(Int(v.minX)),\(Int(v.minY)) \(Int(v.width))x\(Int(v.height))")
    }
    exit(0)
}

let list = (CGWindowListCopyWindowInfo(CGWindowListOption(arrayLiteral: .optionAll), kCGNullWindowID) as? [[String: Any]]) ?? []
for w in list {
    let layer = w[kCGWindowLayer as String] as? Int ?? -1
    if layer != 0 { continue }                                  // 只看普通应用窗口
    let owner = w[kCGWindowOwnerName as String] as? String ?? "?"
    let title = w[kCGWindowName as String] as? String ?? ""
    let num = w[kCGWindowNumber as String] as? Int ?? 0
    let b = (w[kCGWindowBounds as String] as? [String: Any]) ?? [:]
    let width = Int((b["Width"] as? Double) ?? 0)
    let height = Int((b["Height"] as? Double) ?? 0)
    let x = Int((b["X"] as? Double) ?? 0)
    let y = Int((b["Y"] as? Double) ?? 0)
    print("\(num)\t\(owner)\t\(width)x\(height)\t\(x),\(y)\t\(title)")
}
