import SwiftUI
import AppKit

/// 「API 配置」弹窗：展示可复制 / 可查看的 Base URL、Model、API Key，
/// 便于直接填入 AI IDE（Trae / Cursor / Continue 等）的 OpenAI 兼容配置。
///
/// Model 行优先从国际域免费模型（速率 0）列表中下拉选择，可直接复制模型 ID；
/// 无免费模型数据时退回「最近使用的模型」。
struct APIConfigView: View {
    let config: AppConfig
    /// 网关服务是否正在运行（决定能否实时拉取模型列表）。
    let serviceRunning: Bool
    @ObservedObject var catalog: ModelCatalog

    @Environment(\.dismiss) private var dismiss

    @State private var revealKey = false
    @State private var copiedField: String?
    @State private var selectedModelID: String?

    /// 无法获取任何模型信息时的兜底模型（README 示例 / 内置静态模型表）。
    private static let fallbackModel = "deepseek-v4-flash"

    private static let timeFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm"
        return formatter
    }()

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Label("API 配置", systemImage: "key")
                    .font(.headline)
                Spacer()
                Button("完成") { dismiss() }
                    .keyboardShortcut(.defaultAction)
            }
            .padding(16)

            Divider()

            VStack(alignment: .leading, spacing: 16) {
                field(label: "Base URL", value: config.baseURL)
                modelRow
                apiKeyRow
            }
            .padding(16)

            Divider()

            Text("在 AI IDE 中新增一个 OpenAI 兼容模型，填入以上 Base URL、Model 与 API Key 即可调用。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(16)
        }
        .frame(width: 620)
        .onAppear { syncSelection() }
        .onChange(of: catalog.freeModels) { _, _ in syncSelection() }
        .task {
            if serviceRunning { catalog.refresh() } else { catalog.loadFromCache() }
        }
    }

    // MARK: - 行

    private func field(label: String, value: String) -> some View {
        HStack(spacing: 12) {
            rowLabel(label)
            box {
                Text(value)
                    .font(.system(size: 13, design: .monospaced))
                    .textSelection(.enabled)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            copyButton(value: value, field: label)
        }
    }

    private var modelRow: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 12) {
                rowLabel("Model")
                box {
                    if catalog.freeModels.isEmpty {
                        Text(currentModelID)
                            .font(.system(size: 13, design: .monospaced))
                            .textSelection(.enabled)
                            .lineLimit(1)
                            .truncationMode(.middle)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    } else {
                        Picker("", selection: modelSelection) {
                            ForEach(catalog.freeModels) { model in
                                Text("\(model.name) · \(model.id)").tag(model.id)
                            }
                        }
                        .labelsHidden()
                        .pickerStyle(.menu)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if catalog.isRefreshing { ProgressView().controlSize(.small) }
                }
                copyButton(value: currentModelID, field: "Model")
            }
            HStack(spacing: 6) {
                Text(modelHint)
                    .font(.caption)
                    .foregroundStyle(catalog.freeModels.isEmpty ? Color.secondary : Color.green)
                if let updated = catalog.updatedAt {
                    Text("· 更新于 \(Self.timeFormatter.string(from: updated))\(catalog.isStale ? "（已过期，建议刷新）" : "")")
                        .font(.caption)
                        .foregroundStyle(catalog.isStale ? Color.orange : Color.secondary)
                }
                Spacer()
                Button {
                    catalog.refresh()
                } label: {
                    Label("刷新模型", systemImage: "arrow.clockwise")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
                .disabled(catalog.isRefreshing || !serviceRunning)
                .help(serviceRunning ? "重新拉取国际域模型列表" : "服务未运行，无法刷新（显示上次缓存）")
            }
        }
    }

    private var apiKeyRow: some View {
        HStack(spacing: 12) {
            rowLabel("API Key")
            box {
                Text(keyText)
                    .font(.system(size: 13, design: .monospaced))
                    .textSelection(.enabled)
                    .lineLimit(1)
                    .frame(maxWidth: .infinity, alignment: .leading)
                if config.apiKey != nil {
                    Button { revealKey.toggle() } label: {
                        Image(systemName: revealKey ? "eye.slash" : "eye")
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(.borderless)
                    .help(revealKey ? "隐藏 API Key" : "显示 API Key")
                }
            }
            copyButton(value: config.apiKey ?? "", field: "API Key")
        }
    }

    private func rowLabel(_ text: String) -> some View {
        Text(text)
            .font(.system(size: 13, weight: .medium))
            .foregroundStyle(.secondary)
            .frame(width: 78, alignment: .leading)
    }

    /// 统一的行内输入框外观。
    private func box<Content: View>(@ViewBuilder _ content: () -> Content) -> some View {
        HStack(spacing: 8) { content() }
            .padding(.horizontal, 10)
            .padding(.vertical, 8)
            .background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .textBackgroundColor)))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color(nsColor: .separatorColor)))
    }

    private func copyButton(value: String, field: String) -> some View {
        Button {
            copy(value, field: field)
        } label: {
            Image(systemName: copiedField == field ? "checkmark" : "doc.on.doc")
                .font(.system(size: 13))
                .foregroundStyle(copiedField == field ? Color.green : Color.accentColor)
        }
        .buttonStyle(.borderless)
        .disabled(value.isEmpty)
        .help("复制")
    }

    // MARK: - 取值

    private var modelSelection: Binding<String> {
        Binding(
            get: { selectedModelID ?? catalog.freeModels.first?.id ?? "" },
            set: { selectedModelID = $0 }
        )
    }

    /// 当前应复制的模型 ID。
    private var currentModelID: String {
        if let selected = selectedModelID, catalog.freeModels.contains(where: { $0.id == selected }) {
            return selected
        }
        if let first = catalog.freeModels.first { return first.id }
        return config.lastUsedModel() ?? Self.fallbackModel
    }

    private var modelHint: String {
        if catalog.isRefreshing { return "正在拉取国际域模型列表…" }
        if !catalog.freeModels.isEmpty {
            return "国际域免费模型（速率 0）共 \(catalog.freeModels.count) 个，可直接选择后复制"
        }
        if let error = catalog.lastError {
            return "模型列表不可用（\(error)），显示最近使用的模型"
        }
        return "暂无免费模型数据，显示最近使用的模型（可点「刷新模型」获取）"
    }

    private var keyText: String {
        guard let key = config.apiKey else { return "（未设置，网关不校验鉴权）" }
        if revealKey { return key }
        return String(repeating: "•", count: min(key.count, 44))
    }

    /// 默认选中：优先「最近使用的模型」（若在免费列表中），否则第一个免费模型。
    private func syncSelection() {
        guard !catalog.freeModels.isEmpty else { return }
        if let selected = selectedModelID, catalog.freeModels.contains(where: { $0.id == selected }) {
            return
        }
        if let last = config.lastUsedModel(), catalog.freeModels.contains(where: { $0.id == last }) {
            selectedModelID = last
        } else {
            selectedModelID = catalog.freeModels.first?.id
        }
    }

    private func copy(_ value: String, field: String) {
        guard !value.isEmpty else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(value, forType: .string)
        copiedField = field
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) {
            if copiedField == field { copiedField = nil }
        }
    }
}
