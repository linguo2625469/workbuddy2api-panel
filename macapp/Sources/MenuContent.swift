import SwiftUI
import AppKit

/// 菜单栏下拉菜单内容。
struct MenuContent: View {
    @ObservedObject var manager: ProcessManager
    @ObservedObject var updater: UpdateChecker
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // 用（空动作的）按钮而非只读 Text：只读菜单项会被系统强制为「禁用」灰色，
        // 覆盖掉我们设置的白字，导致绿底上文字看不清；启用项才能保留自定义前景色。
        Button(action: {}) { Text(statusRow) }

        Divider()

        if manager.isRunning {
            Button("打开管理面板") { openPanel() }
        }

        if manager.canStop {
            Button("停止服务") { manager.stop() }
                .disabled(manager.isStopping)
        }

        if manager.state == .stopped {
            Button("启动服务") { manager.start() }
        }

        Divider()

        // 更新状态行：同样用空动作按钮，避免只读项被系统置灰而看不清配色。
        Button(action: {}) { Text(updateStatusRow) }

        if let latest = updater.latestVersionText {
            Button("更新到 \(latest)") { updater.update() }
                .disabled(updater.isBusy)
        } else {
            Button("检查更新") { checkUpdates() }
                .disabled(updater.isBusy)
        }

        Divider()

        Button("显示主窗口") { openMainWindow() }

        Divider()

        Button("退出") { NSApp.terminate(nil) }
    }

    /// 顶部状态行：运行态用绿色底，**账号池无可用账号时改用醒目红色底**；
    /// 启动/停止中橙色底，停止态为普通灰色文字。
    private var statusRow: AttributedString {
        var text = AttributedString("WorkBuddy2API · \(manager.statusText)")
        text.font = Font.system(size: 13, weight: .semibold)
        switch manager.state {
        case .running:
            text.foregroundColor = .white
            text.backgroundColor = manager.poolAlert ? .systemRed : .systemGreen
        case .starting, .stopping:
            text.foregroundColor = .white
            text.backgroundColor = .systemOrange
        case .stopped:
            text.foregroundColor = .secondary
        }
        return text
    }

    /// 更新状态行：有新版本橙色底、已最新绿色底、失败红色底，其余普通文字。
    private var updateStatusRow: AttributedString {
        var text = AttributedString(updater.statusText)
        text.font = Font.system(size: 13, weight: .semibold)
        switch updater.status {
        case .available:
            text.foregroundColor = .white
            text.backgroundColor = .systemOrange
        case .upToDate:
            text.foregroundColor = .white
            text.backgroundColor = .systemGreen
        case .failed:
            text.foregroundColor = .white
            text.backgroundColor = .systemRed
        case .idle, .checking, .updating:
            text.foregroundColor = .secondary
        }
        return text
    }

    private func openPanel() {
        openWindow(id: WindowID.panel)
        NSApp.activate(ignoringOtherApps: true)
    }

    /// 菜单栏「检查更新」：菜单本身没有状态展示，因此无论是否有更新都弹窗反馈结果。
    private func checkUpdates() {
        updater.check {
            let alert = NSAlert()
            alert.messageText = "上游版本检查"
            alert.informativeText = updater.statusText
            alert.alertStyle = updater.latestVersionText == nil ? .informational : .warning
            alert.addButton(withTitle: "好")
            NSApp.activate(ignoringOtherApps: true)
            alert.runModal()
        }
    }

    private func openMainWindow() {
        openWindow(id: WindowID.main)
        NSApp.activate(ignoringOtherApps: true)
    }
}
