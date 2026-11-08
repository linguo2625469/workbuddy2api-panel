import Foundation
import Combine
import CryptoKit

/// 上游仓库版本检查与一键更新。
///
/// 版本来源：直接读取本地 `wb2api` 二进制内嵌的 Go 构建信息
/// （`vcs.revision=<40hex>`）与版本串（`x.y.z-panel`），无需运行 Go 工具链。
/// 最新版本：GitHub Releases API 的 latest 发布。
///
/// 更新策略（稳妥）：下载对应 `darwin-<arch>.tar.gz` → 校验 sha256 →
/// 停止服务 → 备份原 `wb2api` → 原子替换 → 重启服务。
/// 只替换 `wb2api`，绝不触碰 `config.json` / `auths/` / `data/`。
final class UpdateChecker: ObservableObject {

    enum Status: Equatable {
        case idle
        case checking
        case upToDate(current: String)
        case available(current: String, latest: String)
        case updating(stage: String)
        case failed(String)
    }

    static let shared = UpdateChecker(manager: .shared)

    @Published private(set) var status: Status = .idle
    @Published private(set) var currentVersion: String = "—"

    private let manager: ProcessManager
    private let repo = "linguo2625469/workbuddy2api-panel"
    private var latestRelease: Release?
    private var timer: Timer?

    init(manager: ProcessManager) {
        self.manager = manager
        // 常驻菜单栏免打扰后台复查：每 6 小时静默刷新一次状态。
        timer = Timer.scheduledTimer(withTimeInterval: 6 * 3600, repeats: true) { [weak self] _ in
            self?.check()
        }
    }

    // MARK: - 对外状态

    var isBusy: Bool {
        switch status {
        case .checking, .updating: return true
        default: return false
        }
    }

    /// 有可用更新时返回最新版本号（含 `v` 前缀），否则 nil。
    var latestVersionText: String? {
        if case .available(_, let latest) = status { return latest }
        return nil
    }

    var statusText: String {
        switch status {
        case .idle: return "未检查更新"
        case .checking: return "正在检查更新…"
        case .upToDate(let current): return "已是最新版本（\(current)）"
        case .available(let current, let latest): return "发现新版本 \(latest)（当前 \(current)）"
        case .updating(let stage): return stage
        case .failed(let message): return message
        }
    }

    // MARK: - 检查

    /// 查询上游最新发布并与本地二进制比对。completion 在主线程调用。
    func check(completion: (() -> Void)? = nil) {
        guard !isBusy else { completion?(); return }
        status = .checking

        let local = readLocalBuild()
        currentVersion = local.displayVersion
        manager.logs.append("[更新] 当前 wb2api：\(local.displayVersion)")

        fetchLatestRelease { [weak self] result in
            guard let self else { completion?(); return }
            switch result {
            case .failure(let error):
                self.status = .failed("检查更新失败：\(error.localizedDescription)")
                self.manager.logs.append("[更新] 检查更新失败：\(error.localizedDescription)")
            case .success(let release):
                self.latestRelease = release
                if self.isUpToDate(local: local, release: release) {
                    self.status = .upToDate(current: local.displayVersion)
                    self.manager.logs.append("[更新] 已是最新版本（\(release.tagName)）")
                } else {
                    self.status = .available(current: local.displayVersion, latest: release.tagName)
                    self.manager.logs.append("[更新] 发现新版本 \(release.tagName)，当前 \(local.displayVersion)")
                }
            }
            completion?()
        }
    }

    // MARK: - 更新

    /// 执行一键更新：下载 → 校验 → 停服 → 备份 → 替换 → 重启。
    func update() {
        guard case .available(let current, let latestTag) = status else { return }
        guard let release = latestRelease else {
            status = .failed("缺少发布信息，请重新检查更新")
            return
        }

        #if arch(arm64)
        let arch = "arm64"
        #else
        let arch = "amd64"
        #endif

        guard let asset = release.assets.first(where: {
            $0.name.contains("darwin-\(arch)") && $0.name.hasSuffix(".tar.gz")
        }) else {
            fail("未找到适用于 macOS \(arch) 的发布包")
            return
        }
        guard let digest = asset.digest, digest.hasPrefix("sha256:") else {
            fail("该发布未提供 sha256 校验值，为安全起见已中止（可前往 Releases 页手动更新）")
            return
        }

        let wasRunning = manager.isRunning
        status = .updating(stage: "正在下载 \(asset.name)…")
        manager.logs.append("[更新] 开始下载 \(asset.name)")

        download(asset.browserDownloadURL) { [weak self] result in
            guard let self else { return }
            switch result {
            case .failure(let error):
                self.fail("下载失败：\(error.localizedDescription)")
            case .success(let data):
                let actual = "sha256:" + SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
                guard actual == digest else {
                    self.fail("校验失败：下载文件与发布校验值不一致，已中止")
                    return
                }
                self.manager.logs.append("[更新] 下载完成，sha256 校验通过")
                self.install(data: data, version: release.version, tag: latestTag,
                             current: current, wasRunning: wasRunning)
            }
        }
    }

    // MARK: - 安装（主线程，含同步停服/替换/重启）

    private func install(data: Data, version: String, tag: String, current: String, wasRunning: Bool) {
        let fm = FileManager.default
        let binaryURL = URL(fileURLWithPath: manager.config.binaryPath)
        let workDir = binaryURL.deletingLastPathComponent()
        let stagedURL = workDir.appendingPathComponent(".wb2api.update")
        let tmp = fm.temporaryDirectory.appendingPathComponent("wb2api-update-\(UUID().uuidString)")
        var backupURL: URL?

        do {
            try fm.createDirectory(at: tmp, withIntermediateDirectories: true)
            defer { try? fm.removeItem(at: tmp) }

            let archive = tmp.appendingPathComponent("update.tar.gz")
            try data.write(to: archive)

            let extractDir = tmp.appendingPathComponent("extract")
            try fm.createDirectory(at: extractDir, withIntermediateDirectories: true)
            try untar(archive, into: extractDir, entry: "wb2api")

            let extracted = extractDir.appendingPathComponent("wb2api")
            guard fm.fileExists(atPath: extracted.path) else {
                throw UpdateError.installFailed("发布包中未找到 wb2api 可执行文件")
            }

            // 停服务，释放二进制占用
            status = .updating(stage: "正在停止服务…")
            manager.logs.append("[更新] 停止服务以替换二进制")
            manager.stop()

            // 备份原二进制（仅备份 wb2api，不触碰用户数据）
            let backup = workDir.appendingPathComponent("wb2api.bak-\(sanitize(current))")
            try? fm.removeItem(at: backup)
            try fm.copyItem(at: binaryURL, to: backup)
            backupURL = backup
            manager.logs.append("[更新] 原二进制已备份为 \(backup.lastPathComponent)")

            // 暂存新二进制到同一卷，再原子替换
            try? fm.removeItem(at: stagedURL)
            try fm.copyItem(at: extracted, to: stagedURL)
            try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: stagedURL.path)

            status = .updating(stage: "正在安装 \(tag)…")
            _ = try fm.replaceItemAt(binaryURL, withItemAt: stagedURL)

            manager.logs.append("[更新] 已替换 wb2api，准备重启服务")
        } catch {
            // 回滚：仅在二进制已缺失时用备份还原
            if let backup = backupURL, !fm.fileExists(atPath: binaryURL.path) {
                try? fm.copyItem(at: backup, to: binaryURL)
                manager.logs.append("[更新] 安装失败，已从备份回滚")
            }
            try? fm.removeItem(at: stagedURL)
            fail("更新失败：\(error.localizedDescription)")
            if wasRunning { manager.start() }
            return
        }

        if wasRunning {
            status = .updating(stage: "正在重启服务…")
            manager.start()
        }
        currentVersion = tag
        status = .upToDate(current: tag)
        manager.logs.append("[更新] 已更新到 \(tag)（原版本 \(current)）")
    }

    private func fail(_ message: String) {
        status = .failed(message)
        manager.logs.append("[更新] \(message)")
    }

    // MARK: - 本地二进制版本读取

    struct LocalBuild {
        var version: String?
        var revision: String?

        var displayVersion: String {
            if let version { return version }
            if let revision { return String(revision.prefix(7)) }
            return "未知"
        }
    }

    private func readLocalBuild() -> LocalBuild {
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: manager.config.binaryPath)),
              let text = String(data: data, encoding: .isoLatin1) else {
            return LocalBuild(version: nil, revision: nil)
        }
        return LocalBuild(
            version: Self.firstGroup(#"\d+\.\d+\.\d+-panel"#, in: text),
            revision: Self.firstGroup(#"vcs\.revision=([0-9a-f]{40})"#, in: text)
        )
    }

    /// 返回正则匹配的第 1 个捕获组；无捕获组时返回整体匹配。
    private static func firstGroup(_ pattern: String, in text: String) -> String? {
        guard let regex = try? NSRegularExpression(pattern: pattern) else { return nil }
        let range = NSRange(text.startIndex..., in: text)
        guard let match = regex.firstMatch(in: text, range: range) else { return nil }
        let captured = match.numberOfRanges > 1 ? match.range(at: 1) : match.range(at: 0)
        guard captured.location != NSNotFound, let swiftRange = Range(captured, in: text) else { return nil }
        return String(text[swiftRange])
    }

    /// 本地是否已是最新：优先比对 commit 修订号，其次比对语义化版本号。
    private func isUpToDate(local: LocalBuild, release: Release) -> Bool {
        if let revision = local.revision, !release.targetCommitish.isEmpty, revision == release.targetCommitish {
            return true
        }
        if let localParts = versionParts(local.version), let remoteParts = versionParts(release.version) {
            return compare(localParts, remoteParts) >= 0
        }
        return false
    }

    private func versionParts(_ raw: String?) -> [Int]? {
        guard var core = raw else { return nil }
        if core.hasPrefix("v") { core.removeFirst() }
        if let dash = core.firstIndex(of: "-") { core = String(core[..<dash]) }
        let parts = core.split(separator: ".").map { Int($0) }
        guard !parts.isEmpty, parts.allSatisfy({ $0 != nil }) else { return nil }
        return parts.map { $0! }
    }

    private func compare(_ a: [Int], _ b: [Int]) -> Int {
        for index in 0..<max(a.count, b.count) {
            let left = index < a.count ? a[index] : 0
            let right = index < b.count ? b[index] : 0
            if left != right { return left < right ? -1 : 1 }
        }
        return 0
    }

    private func sanitize(_ value: String) -> String {
        let allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-"
        return String(value.map { allowed.contains($0) ? $0 : "_" })
    }

    // MARK: - 解压

    private func untar(_ archive: URL, into directory: URL, entry: String) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/tar")
        proc.arguments = ["-xzf", archive.path, "-C", directory.path, entry]
        let errPipe = Pipe()
        proc.standardOutput = Pipe()
        proc.standardError = errPipe
        try proc.run()
        proc.waitUntilExit()
        guard proc.terminationStatus == 0 else {
            let message = String(data: errPipe.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
            throw UpdateError.installFailed("解压失败：\(message.trimmingCharacters(in: .whitespacesAndNewlines))")
        }
    }

    // MARK: - 网络

    private func fetchLatestRelease(completion: @escaping (Result<Release, Error>) -> Void) {
        var request = URLRequest(url: URL(string: "https://api.github.com/repos/\(repo)/releases/latest")!)
        request.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        request.setValue("WorkBuddy2API-Launcher", forHTTPHeaderField: "User-Agent")
        request.timeoutInterval = 15
        URLSession.shared.dataTask(with: request) { data, _, error in
            DispatchQueue.main.async {
                if let error { completion(.failure(error)); return }
                guard let data, let release = try? JSONDecoder().decode(Release.self, from: data) else {
                    completion(.failure(UpdateError.network("无法解析发布信息")))
                    return
                }
                completion(.success(release))
            }
        }.resume()
    }

    private func download(_ url: URL, completion: @escaping (Result<Data, Error>) -> Void) {
        var request = URLRequest(url: url)
        request.setValue("WorkBuddy2API-Launcher", forHTTPHeaderField: "User-Agent")
        request.timeoutInterval = 120
        URLSession.shared.dataTask(with: request) { data, _, error in
            DispatchQueue.main.async {
                if let error { completion(.failure(error)); return }
                guard let data else {
                    completion(.failure(UpdateError.network("下载内容为空")))
                    return
                }
                completion(.success(data))
            }
        }.resume()
    }

    // MARK: - 数据模型

    struct Release: Decodable {
        let tagName: String
        let targetCommitish: String
        let assets: [Asset]

        enum CodingKeys: String, CodingKey {
            case tagName = "tag_name"
            case targetCommitish = "target_commitish"
            case assets
        }

        /// 去掉 `v` 前缀的版本号。
        var version: String { tagName.hasPrefix("v") ? String(tagName.dropFirst()) : tagName }

        struct Asset: Decodable {
            let name: String
            let browserDownloadURL: URL
            let digest: String?

            enum CodingKeys: String, CodingKey {
                case name
                case browserDownloadURL = "browser_download_url"
                case digest
            }
        }
    }

    enum UpdateError: LocalizedError {
        case network(String)
        case installFailed(String)

        var errorDescription: String? {
            switch self {
            case .network(let message): return message
            case .installFailed(let message): return message
            }
        }
    }
}
