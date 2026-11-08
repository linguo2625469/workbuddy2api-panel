import AppKit
import UserNotifications

/// 应用委托：驻留菜单栏（无 Dock 图标），退出时清理子进程并给出提示。
final class AppDelegate: NSObject, NSApplicationDelegate {

    func applicationDidFinishLaunching(_ notification: Notification) {
        // 隐藏 Dock 图标、隐藏主窗口，仅保留菜单栏图标
        NSApp.setActivationPolicy(.accessory)
        requestNotificationPermission()
        // 启动即探测：若已有外部实例在运行，直接接管为可用状态
        ProcessManager.shared.refreshOnLaunch { [weak self] in
            let manager = ProcessManager.shared
            let body: String
            if manager.isRunning {
                body = "检测到服务已在运行，点击菜单栏图标可打开管理面板。"
            } else if manager.isStarting {
                body = "正在自动启动服务，稍候即可使用；点击菜单栏图标可查看状态。"
            } else {
                body = "点击顶部菜单栏图标即可启动服务并打开管理面板。"
            }
            self?.notify(title: "WorkBuddy2API 已驻留菜单栏", body: body)
            self?.checkForUpdates()
        }
    }

    /// 启动后静默检查上游是否有新版本，有则发通知提醒。
    private func checkForUpdates() {
        UpdateChecker.shared.check { [weak self] in
            guard let latest = UpdateChecker.shared.latestVersionText else { return }
            self?.notify(title: "发现新版本 \(latest)",
                         body: "点击菜单栏图标 → 「更新到 \(latest)」即可自动升级。")
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        ProcessManager.shared.shutdown()
    }

    // MARK: - 通知提示

    private func requestNotificationPermission() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }

    private func notify(title: String, body: String) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        let request = UNNotificationRequest(identifier: UUID().uuidString,
                                            content: content,
                                            trigger: nil)
        UNUserNotificationCenter.current().add(request)
    }
}
