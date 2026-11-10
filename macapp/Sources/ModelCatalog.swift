import Foundation
import Combine

/// 国际域模型目录。
///
/// 数据来源：网关的 `GET /v1/models`（Bearer 鉴权），它返回的即是国际域（`global:` 前缀）
/// 模型清单，每个模型带 `credits` 积分倍率；若网关同时下发 `promo_credits`
/// （限时优惠价）则优先按优惠价判定免费。本类把该 JSON **落盘为文档**
/// （工作目录 `data/models.json`），再从文档中筛选出「国际域且生效倍率为 0」的模型，
/// 供主窗口「API 配置」弹窗中展示与复制。
///
/// 服务未运行或拉取失败时，自动退回读取已落盘的文档，保证离线也能看到上次结果。
final class ModelCatalog: ObservableObject {
    static let shared = ModelCatalog(manager: .shared)

    /// 免费（生效倍率 0）的国际域模型。
    struct FreeModel: Identifiable, Equatable {
        let id: String
        let name: String
        let credits: Double
        /// 非空表示靠限时优惠免费（牌价不为 0），用于在界面上标注。
        let promoLabel: String?
    }

    @Published private(set) var freeModels: [FreeModel] = []
    @Published private(set) var totalModels = 0
    @Published private(set) var updatedAt: Date?
    @Published private(set) var isRefreshing = false
    @Published private(set) var lastError: String?

    private let manager: ProcessManager
    private var cancellables = Set<AnyCancellable>()

    /// 网关对 `/v1/models` 的缓存时长（README：缓存 1h），超过即视为数据可能过期。
    private static let cacheTTL: TimeInterval = 3600

    private init(manager: ProcessManager) {
        self.manager = manager
        loadFromCache()
        // 服务就绪（含网络守护重启后恢复）时，若数据缺失或已过期则自动刷新。
        manager.$state
            .removeDuplicates()
            .sink { [weak self] state in
                guard case .running = state else { return }
                self?.refreshIfStale()
            }
            .store(in: &cancellables)
    }

    /// 数据是否可能已过期（无数据，或距上次拉取超过网关 1h 缓存时长）。
    var isStale: Bool {
        guard let updatedAt else { return true }
        return Date().timeIntervalSince(updatedAt) > Self.cacheTTL
    }

    /// 仅在数据缺失或已过期时刷新（服务刚就绪 / 网络恢复时调用）。
    func refreshIfStale() {
        if freeModels.isEmpty || isStale { refresh() }
    }

    /// 模型列表 JSON 文档的落盘路径（工作目录 `data/models.json`）。
    var documentURL: URL {
        URL(fileURLWithPath: manager.config.workingDirectory)
            .appendingPathComponent("data/models.json")
    }

    // MARK: - 拉取 / 缓存

    /// 拉取最新模型列表文档并刷新免费模型；失败时退回缓存文档。
    func refresh() {
        guard !isRefreshing else { return }
        guard let url = URL(string: manager.config.modelsURL) else { return }

        isRefreshing = true
        lastError = nil
        var request = URLRequest(url: url)
        request.timeoutInterval = 8
        request.cachePolicy = .reloadIgnoringLocalCacheData
        if let key = manager.config.apiKey {
            request.setValue("Bearer \(key)", forHTTPHeaderField: "Authorization")
        }

        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            DispatchQueue.main.async {
                guard let self else { return }
                self.isRefreshing = false
                let status = (response as? HTTPURLResponse)?.statusCode
                guard error == nil, let data, !data.isEmpty, status == 200 else {
                    self.lastError = error?.localizedDescription ?? "模型列表获取失败（HTTP \(status ?? 0)）"
                    self.loadFromCache()
                    return
                }
                self.saveDocument(data)
                self.parse(data, timestamp: Date())
            }
        }.resume()
    }

    /// 读取已落盘的模型列表文档并刷新免费模型（时间戳取文档修改时间，以便判断新鲜度）。
    func loadFromCache() {
        guard let data = try? Data(contentsOf: documentURL) else { return }
        let attrs = try? FileManager.default.attributesOfItem(atPath: documentURL.path)
        let modified = (attrs?[.modificationDate] as? Date) ?? Date()
        parse(data, timestamp: modified)
    }

    private func saveDocument(_ data: Data) {
        let fm = FileManager.default
        try? fm.createDirectory(at: documentURL.deletingLastPathComponent(),
                                withIntermediateDirectories: true)
        try? data.write(to: documentURL, options: .atomic)
    }

    // MARK: - 解析

    private func parse(_ data: Data, timestamp: Date) {
        struct Response: Decodable {
            let data: [Model]
            struct Model: Decodable {
                let id: String
                let name: String?
                let credits: String?
                /// 限时优惠价（网关新增字段，未下发时为 nil）。
                let promoCredits: FlexibleRate?
                let promoLabel: String?

                enum CodingKeys: String, CodingKey {
                    case id, name, credits
                    case promoCredits = "promo_credits"
                    case promoLabel = "promo_label"
                }
            }
        }
        guard let decoded = try? JSONDecoder().decode(Response.self, from: data) else {
            lastError = "无法解析模型列表文档"
            return
        }
        totalModels = decoded.data.count
        freeModels = decoded.data.compactMap { model -> FreeModel? in
            guard model.id.hasPrefix(Self.realmPrefix) else { return nil }   // 仅国际域
            let listed = model.credits.flatMap(Self.parseCredits)
            let promo = model.promoCredits.flatMap { Self.parseCredits($0.text) }
            // 生效倍率取牌价与优惠价中更低的那个（优惠价只可能更便宜）。
            guard let rate = [listed, promo].compactMap({ $0 }).min(), rate == 0 else { return nil }
            let name = (model.name?.isEmpty == false ? model.name! : String(model.id.dropFirst(Self.realmPrefix.count)))
            let isPromo = (promo == 0) && (listed ?? 1) > 0
            return FreeModel(id: model.id, name: name, credits: rate,
                             promoLabel: isPromo ? (model.promoLabel ?? "限时免费") : nil)
        }
        updatedAt = timestamp
    }

    /// 兼容字符串（`"x0.00"`）与数字（`0`）两种写法的倍率字段。
    struct FlexibleRate: Decodable {
        let text: String

        init(from decoder: Decoder) throws {
            let container = try decoder.singleValueContainer()
            if let string = try? container.decode(String.self) {
                text = string
            } else if let number = try? container.decode(Double.self) {
                text = String(number)
            } else {
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "倍率字段格式不支持")
            }
        }
    }

    private static let realmPrefix = "global:"

    /// 解析 `x0.00` / `x3.31` 形式的积分倍率为数值。
    private static func parseCredits(_ raw: String) -> Double? {
        var text = raw.trimmingCharacters(in: .whitespaces)
        if text.hasPrefix("x") || text.hasPrefix("X") { text.removeFirst() }
        return Double(text)
    }
}
