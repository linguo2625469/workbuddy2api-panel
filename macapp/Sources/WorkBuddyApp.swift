import SwiftUI
import AppKit

@main
struct WorkBuddyApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var manager = ProcessManager.shared
    @StateObject private var pulse = IconPulse.shared
    @StateObject private var updater = UpdateChecker.shared
    @StateObject private var keeper = NetworkKeeper.shared
    @StateObject private var catalog = ModelCatalog.shared

    var body: some Scene {
        MenuBarExtra {
            MenuContent(manager: manager, updater: updater)
        } label: {
            MenuBarIcon(active: manager.isRunning, bright: pulse.bright, alert: manager.poolAlert)
        }
        .menuBarExtraStyle(.menu)

        Window("WorkBuddy2API", id: WindowID.main) {
            ContentView(manager: manager, updater: updater, keeper: keeper, catalog: catalog)
        }
        .defaultLaunchBehavior(.suppressed)
        .defaultSize(width: 720, height: 480)

        Window("管理面板", id: WindowID.panel) {
            WebPanelView(url: manager.panelURL)
        }
        .defaultLaunchBehavior(.suppressed)
        .defaultSize(width: 1180, height: 820)
    }
}

enum WindowID {
    static let main = "main"
    static let panel = "panel"
}

/// 菜单栏图标：停止态用系统模板图标（随深浅色自适应）；
/// 运行态换成实心图标，正常时由 `bright` 在蓝色 / 黄色之间交替闪烁；
/// 账号池无可用账号（`alert`）时改为整只图标红色闪烁，提示 AI IDE 此刻会卡住。
struct MenuBarIcon: View {
    let active: Bool
    let bright: Bool
    let alert: Bool

    var body: some View {
        Image(nsImage: Self.image(active: active, bright: bright, alert: alert))
    }

    static func image(active: Bool, bright: Bool, alert: Bool) -> NSImage {
        let name = active ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle"
        let config = NSImage.SymbolConfiguration(pointSize: 15, weight: .semibold)
        guard let base = NSImage(systemSymbolName: name, accessibilityDescription: "WorkBuddy2API")?
            .withSymbolConfiguration(config) else { return NSImage() }
        guard active else {
            base.isTemplate = true   // 非运行态：交给系统着色
            return base
        }
        if alert {
            // 全红闪烁：亮/暗两档红色交替。
            return tinted(base, color: bright ? .systemRed : NSColor.systemRed.withAlphaComponent(0.3))
        }
        return tinted(base, color: bright ? .systemBlue : .systemYellow)
    }

    /// 把模板图按指定颜色染色，并关闭模板模式（否则系统会把蓝色覆盖掉）。
    private static func tinted(_ image: NSImage, color: NSColor) -> NSImage {
        let out = NSImage(size: image.size)
        out.lockFocus()
        let rect = NSRect(origin: .zero, size: image.size)
        image.draw(in: rect)
        color.set()
        rect.fill(using: .sourceAtop)
        out.unlockFocus()
        out.isTemplate = false
        return out
    }
}

/// 驱动菜单栏图标的闪烁节奏（运行时的蓝/黄交替）。
final class IconPulse: ObservableObject {
    static let shared = IconPulse()

    @Published private(set) var bright = true
    private var timer: Timer?

    private init() {
        timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            self?.bright.toggle()
        }
    }
}
