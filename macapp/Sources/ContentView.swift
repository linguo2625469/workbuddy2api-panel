import SwiftUI
import AppKit

/// 主窗口：运行状态 + 操作按钮 + 实时日志。
struct ContentView: View {
    @ObservedObject var manager: ProcessManager
    @ObservedObject var updater: UpdateChecker
    @ObservedObject var keeper: NetworkKeeper
    @ObservedObject var catalog: ModelCatalog
    @Environment(\.openWindow) private var openWindow
    @State private var showAPIConfig = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            header
            if !manager.config.isInstalled {
                installBanner
            }
            controls
            updateRow
            logPanel
        }
        .padding(16)
        .frame(minWidth: 760, minHeight: 420)
        .sheet(isPresented: $showAPIConfig) {
            APIConfigView(config: manager.config, serviceRunning: manager.isRunning, catalog: catalog)
        }
    }

    private var header: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(statusColor)
                .frame(width: 11, height: 11)
            VStack(alignment: .leading, spacing: 2) {
                Text("WorkBuddy2API 网关")
                    .font(.headline)
                Text(headerDetail)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            if manager.isRunning, manager.isExternal {
                Text("外部进程")
                    .font(.caption)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(.quaternary, in: Capsule())
            }
        }
    }

    /// 未找到 wb2api 时的引导条：选择已有目录，或前往 Releases 下载。
    private var installBanner: some View {
        HStack(spacing: 10) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
            VStack(alignment: .leading, spacing: 2) {
                Text("未找到网关二进制（wb2api）")
                    .font(.subheadline.weight(.semibold))
                Text(manager.config.workingDirectory)
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Spacer()
            Button("选择网关目录…") { chooseWorkDir() }
            Button("下载网关") { openDownloadPage() }
        }
        .padding(10)
        .background(.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
    }

    private func chooseWorkDir() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "选择"
        panel.message = "选择包含 wb2api 与 config.json 的目录"
        if panel.runModal() == .OK, let url = panel.url {
            manager.setHome(url.path)
        }
    }

    private func openDownloadPage() {
        guard let url = URL(string: "https://github.com/linguo2625469/workbuddy2api-panel/releases/latest") else { return }
        NSWorkspace.shared.open(url)
    }

    private var controls: some View {
        HStack(spacing: 10) {
            if manager.isRunning {
                Button {
                    openWindow(id: WindowID.panel)
                    NSApp.activate(ignoringOtherApps: true)
                } label: {
                    Label("管理面板", systemImage: "rectangle.on.rectangle")
                }
                .keyboardShortcut("o")
            }

            // 只要可以停止（运行中/启动中）就显示「停止服务」按钮，
            // 避免启动卡住时主窗口没有任何可操作入口。
            if manager.canStop {
                Button(role: .destructive) {
                    manager.stop()
                } label: {
                    Label("停止服务", systemImage: "stop.fill")
                }
                .disabled(manager.isStopping)
            }

            if manager.state == .stopped {
                Button {
                    manager.start()
                } label: {
                    Label("启动服务", systemImage: "play.fill")
                }
                .keyboardShortcut("r")
            }

            if manager.isStopping {
                ProgressView().controlSize(.small)
            }

            Toggle("自启动", isOn: $manager.autoStartEnabled)
                .toggleStyle(.checkbox)
                .help("开启后：app 启动时自动启动网关服务（默认开启）。")

            Button {
                manager.refreshConfig()
                showAPIConfig = true
            } label: {
                Label("API 配置", systemImage: "key")
            }
            .help("查看 / 复制 Base URL、Model 与 API Key，用于在 AI IDE 中配置大模型")

            Toggle(isOn: keepNetworkBinding) {
                Label("断流保持", systemImage: "antenna.radiowaves.left.and.right")
            }
            .toggleStyle(.switch)
            .controlSize(.small)
            .help("开启后：弱网、掉线、抖动或网络出口切换时自动重启服务以重建上游连接并续接会话，服务失联时自动恢复。")

            Button {
                chooseWorkDir()
            } label: {
                Label("网关目录", systemImage: "folder")
            }
            .labelStyle(.iconOnly)
            .help("更改 wb2api 所在目录（当前：\(manager.config.workingDirectory)）")

            Spacer()

            Button {
                manager.logs.clear()
            } label: {
                Label("清空日志", systemImage: "trash")
            }
        }
        .controlSize(.large)
    }

    private var keepNetworkBinding: Binding<Bool> {
        Binding(get: { keeper.isEnabled }, set: { keeper.setEnabled($0) })
    }

    /// 头部状态明细：运行状态 · 端口 · （开启时）网络守护状态。
    private var headerDetail: String {
        var text = "\(manager.statusText) · 端口 \(manager.config.port)"
        if keeper.isEnabled {
            text += keeper.networkAvailable ? " · 网络守护在线" : " · 网络守护：网络离线"
        }
        return text
    }

    private var updateRow: some View {
        HStack(spacing: 10) {
            Image(systemName: updateIcon)
                .foregroundStyle(updateColor)
                .font(.system(size: 15))
            VStack(alignment: .leading, spacing: 1) {
                Text("上游版本更新")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(updater.statusText)
                    .font(.subheadline)
            }
            Spacer()
            if let latest = updater.latestVersionText {
                Button {
                    updater.update()
                } label: {
                    Label("更新到 \(latest)", systemImage: "arrow.down.circle.fill")
                }
                .buttonStyle(.borderedProminent)
                .disabled(updater.isBusy)
            }
            Button {
                updater.check()
            } label: {
                Label("检查更新", systemImage: "arrow.triangle.2.circlepath")
            }
            .disabled(updater.isBusy)
        }
        .padding(10)
        .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 10))
    }

    private var updateIcon: String {
        switch updater.status {
        case .idle: return "questionmark.circle"
        case .checking, .updating: return "arrow.triangle.2.circlepath"
        case .upToDate: return "checkmark.circle.fill"
        case .available: return "arrow.down.circle.fill"
        case .failed: return "exclamationmark.triangle.fill"
        }
    }

    private var updateColor: Color {
        switch updater.status {
        case .idle: return .secondary
        case .checking, .updating: return .orange
        case .upToDate: return .green
        case .available: return .accentColor
        case .failed: return .red
        }
    }

    private var logPanel: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 2) {
                    ForEach(Array(manager.logs.lines.enumerated()), id: \.offset) { index, line in
                        Text(line)
                            .font(.system(size: 11, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .id(index)
                    }
                }
                .padding(10)
            }
            .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 10))
            .onChange(of: manager.logs.lines.count) { _, newCount in
                guard newCount > 0 else { return }
                withAnimation(.linear(duration: 0.1)) {
                    proxy.scrollTo(newCount - 1, anchor: .bottom)
                }
            }
        }
    }

    private var statusColor: Color {
        switch manager.state {
        case .stopped: return .secondary
        case .starting, .stopping: return .orange
        case .running: return manager.poolAlert ? .red : .green
        }
    }
}
