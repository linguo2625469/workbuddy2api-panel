import Foundation

/// 应用配置：定位 wb2api 二进制、工作目录、面板端口与 API Key。
///
/// 端口规则（与项目自身逻辑一致）：
/// - 工作目录下存在 `config.json` 时，读取其 `listen` 字段（用户在管理面板个性化并保存过的端口）；
/// - `config.json` 不存在时，回落到 wb2api 内建默认端口 7863。
struct AppConfig {
    let binaryPath: String
    let workingDirectory: String
    private(set) var port: Int
    /// 端口是否来自 config.json（false 表示用了默认值）。
    private(set) var portFromConfig: Bool
    /// config.json 中的 `api_key`；为空或缺失时为 nil（表示网关不校验鉴权，直接放行）。
    private(set) var apiKey: String?

    /// wb2api 的内建默认端口（与 config.example.json 一致）。
    static let defaultPort = 7863
    /// 用户手动选择的网关目录（UserDefaults 持久化）。
    static let homeKey = "workbuddy2api.home"

    /// 二进制是否存在且可执行。为 false 时界面提示用户选择目录 / 下载网关。
    var isInstalled: Bool {
        FileManager.default.isExecutableFile(atPath: binaryPath)
    }

    /// 记录用户选择的网关目录，后续启动优先使用。
    static func setHome(_ path: String) {
        UserDefaults.standard.set(path, forKey: homeKey)
    }

    static func load() -> AppConfig {
        let workDir = resolveWorkDir()
        let file = readConfigFile(workDir: workDir)
        return AppConfig(
            binaryPath: workDir + "/wb2api",
            workingDirectory: workDir,
            port: file.port ?? defaultPort,
            portFromConfig: file.port != nil,
            apiKey: file.apiKey
        )
    }

    /// 解析网关工作目录（不写死任何本机路径）：
    /// 1. 环境变量 `WB2API_HOME`
    /// 2. 用户在 app 内选择过的目录
    /// 3. `~/workbuddy2api`
    /// 4. app 包所在目录及其上两级（兼容 `macapp/WorkBuddy2API.app` 与 `.app` 与 wb2api 同级两种布局）
    /// 5. 当前进程工作目录
    ///
    /// 以上均不含 `wb2api` 时，返回首选路径，由界面提示「未安装」。
    static func resolveWorkDir() -> String {
        let fm = FileManager.default
        var preferred = NSHomeDirectory() + "/workbuddy2api"
        if let saved = UserDefaults.standard.string(forKey: homeKey), !saved.isEmpty {
            preferred = (saved as NSString).expandingTildeInPath
        }
        if let env = ProcessInfo.processInfo.environment["WB2API_HOME"], !env.isEmpty {
            preferred = (env as NSString).expandingTildeInPath
        }

        var candidates = [preferred, NSHomeDirectory() + "/workbuddy2api"]
        var dir = Bundle.main.bundleURL.deletingLastPathComponent().path
        for _ in 0..<3 {
            candidates.append(dir)
            dir = (dir as NSString).deletingLastPathComponent
        }
        candidates.append(fm.currentDirectoryPath)

        for path in candidates where fm.isExecutableFile(atPath: path + "/wb2api") {
            return path
        }
        return preferred
    }

    /// 重新读取 config.json。用户可能在服务运行期间改了配置，启动前调用以保持一致。
    mutating func reloadPort() {
        let file = AppConfig.readConfigFile(workDir: workingDirectory)
        self.port = file.port ?? AppConfig.defaultPort
        self.portFromConfig = file.port != nil
        self.apiKey = file.apiKey
    }

    /// OpenAI 兼容客户端使用的 Base URL。
    var baseURL: String { "http://127.0.0.1:\(port)/v1" }

    /// 模型列表端点。
    var modelsURL: String { baseURL + "/models" }

    var portSourceText: String {
        portFromConfig ? "config.json" : "默认值(config.json 不存在)"
    }

    /// 从 `data/state.json` 读取最近使用的模型（服务未运行、无法拉取模型列表时的兜底展示）。
    func lastUsedModel() -> String? {
        let url = URL(fileURLWithPath: workingDirectory)
            .appendingPathComponent("data/state.json")
        guard let data = try? Data(contentsOf: url),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let model = (obj["last_model"] as? String)?.trimmingCharacters(in: .whitespacesAndNewlines),
              !model.isEmpty else {
            return nil
        }
        return model
    }

    private struct ConfigFile {
        var port: Int?
        var apiKey: String?
    }

    private static func readConfigFile(workDir: String) -> ConfigFile {
        let cfgURL = URL(fileURLWithPath: workDir).appendingPathComponent("config.json")
        guard let data = try? Data(contentsOf: cfgURL),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return ConfigFile()
        }
        var result = ConfigFile()
        if let listen = obj["listen"] as? String { result.port = parsePort(listen) }
        if let key = obj["api_key"] as? String,
           !key.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            result.apiKey = key
        }
        return result
    }

    /// 从 `:7863` / `127.0.0.1:7863` 这类监听地址中取出端口号。
    private static func parsePort(_ listen: String) -> Int? {
        guard let last = listen.split(separator: ":").last else { return nil }
        return Int(last)
    }
}
