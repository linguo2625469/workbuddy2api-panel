import Foundation
import Combine

/// wb2api 子进程生命周期管理：启动 / 就绪探测 / 停止 / 日志收集。
/// 所有 @Published 变更均在主线程完成。
final class ProcessManager: ObservableObject {

    enum State: Equatable {
        case stopped
        case starting
        case running(owned: Bool)   // owned=true 表示由本 app 拉起，false 表示接管的外部实例
        case stopping
    }

    static let shared = ProcessManager(config: .load())

    @Published private(set) var state: State = .stopped
    /// 是否在 app 启动时自动启动服务（持久化到 UserDefaults，默认开启）。
    @Published var autoStartEnabled: Bool {
        didSet { UserDefaults.standard.set(autoStartEnabled, forKey: Self.autoStartKey) }
    }
    let logs = LogStore()

    /// 账号池健康：最近一次 `/healthz` 探测的可用 / 总账号数。
    @Published private(set) var poolHealthy = 0
    @Published private(set) var poolTotal = 0
    /// 账号池是否可用：`nil` 表示未知（未运行或尚未探测）；`false` 表示无可用账号（冷却 / 禁用 / 占满）。
    @Published private(set) var poolAvailable: Bool?

    private static let autoStartKey = "workbuddy2api.autoStartService"

    private(set) var config: AppConfig
    private var process: Process?
    private var readinessTimer: Timer?
    private var poolTimer: Timer?
    private var cancellables = Set<AnyCancellable>()

    init(config: AppConfig) {
        self.config = config
        // 首次运行无该键时默认开启「自动启动服务」。
        self.autoStartEnabled = UserDefaults.standard.object(forKey: Self.autoStartKey) as? Bool ?? true
        // 转发日志变更：观察 ProcessManager 的视图需要随 logs 刷新（否则清空/追加日志不会重绘）。
        logs.objectWillChange
            .sink { [weak self] _ in self?.objectWillChange.send() }
            .store(in: &cancellables)
        // 运行期间周期探测账号池健康；停止/启动中则清空状态。
        $state
            .removeDuplicates()
            .sink { [weak self] state in
                guard let self else { return }
                if case .running = state { self.startPoolPolling() } else { self.stopPoolPolling() }
            }
            .store(in: &cancellables)
    }

    var panelURL: URL { URL(string: "http://127.0.0.1:\(config.port)/panel/")! }
    var healthURL: URL { URL(string: "http://127.0.0.1:\(config.port)/healthz")! }

    var isRunning: Bool {
        if case .running = state { return true }
        return false
    }
    var isStarting: Bool { state == .starting }
    var isExternal: Bool {
        if case .running(let owned) = state { return !owned }
        return false
    }
    var canStop: Bool {
        switch state {
        case .running, .starting: return true
        case .stopping, .stopped: return false
        }
    }
    var isStopping: Bool { state == .stopping }
    /// 运行中但账号池无可用账号（冷却 / 禁用 / 占满）——AI IDE 此时会卡住，需醒目告警。
    var poolAlert: Bool { isRunning && poolAvailable == false }
    var statusText: String {
        switch state {
        case .stopped: return "已停止"
        case .starting: return "启动中…"
        case .stopping: return "停止中…"
        case .running(let owned):
            if poolAvailable == false {
                let counts = poolTotal > 0 ? "（可用 \(poolHealthy)/\(poolTotal)）" : ""
                return "运行中 · 无可用账号\(counts)"
            }
            return owned ? "运行中" : "运行中（外部进程）"
        }
    }

    // MARK: - 启停

    /// app 启动时调用：若端口上已有本网关在运行则直接接管；否则在开启「自动启动服务」时自动拉起。
    func refreshOnLaunch(completion: (() -> Void)? = nil) {
        guard state == .stopped else { completion?(); return }
        config.reloadPort()
        probeService { [weak self] responded, isOurs in
            guard let self else { completion?(); return }
            if isOurs {
                self.logs.append("[启动器] 检测到已在运行的 wb2api 实例，接管为运行中（端口 \(self.config.port)，来源 \(self.config.portSourceText)）")
                self.state = .running(owned: false)
                completion?()
            } else if self.autoStartEnabled {
                self.logs.append("[启动器] 已开启「自动启动服务」，正在自动启动…")
                self.start()
                completion?()
            } else {
                completion?()
            }
        }
    }

    func start() {
        guard state == .stopped else { return }
        guard config.isInstalled else {
            logs.append("[启动器] 未找到网关二进制：\(config.binaryPath)")
            logs.append("[启动器] 请点击「选择网关目录…」指向已解压的 wb2api 所在目录，或先下载对应 Release")
            return
        }
        // 重新读取 config.json，保证与用户个性化保存的端口一致
        config.reloadPort()
        state = .starting
        logs.append("[启动器] 使用端口 \(config.port)（来源：\(config.portSourceText)），开始检测…")

        probeService { [weak self] responded, isOurs in
            guard let self else { return }
            if isOurs {
                self.logs.append("[启动器] 端口 \(self.config.port) 上已有 wb2api 实例，接管为运行中（外部进程）")
                self.state = .running(owned: false)
            } else if responded {
                self.logs.append("[启动器] 端口 \(self.config.port) 被其它程序占用，无法启动；请点「停止服务」释放端口或更换端口")
                self.state = .stopped
            } else {
                self.spawn()
            }
        }
    }

    func stop() {
        readinessTimer?.invalidate()
        readinessTimer = nil

        state = .stopping
        if let proc = process, isAlive(proc) {
            // 本 app 拉起的子进程：逐级信号终止。
            terminateChild(proc, reason: "正在停止服务")
        } else {
            // 无本 app 子进程（接管的外部实例）：直接结束占用本端口的 wb2api 进程以释放端口。
            process = nil
            releasePortListeners(reason: "正在停止服务")
        }
        process = nil
        state = .stopped
        reportPortState(action: "停止")
    }

    /// 退出 app 时调用（同步执行，确保子进程真正退出、端口被释放）。
    /// 无论进程由本 app 还是外部命令拉起，都负责释放本项目端口。
    func shutdown() {
        readinessTimer?.invalidate()
        readinessTimer = nil
        if let proc = process, isAlive(proc) {
            terminateChild(proc, reason: "退出前清理")
        } else {
            releasePortListeners(reason: "退出前清理")
        }
        process = nil
    }

    // MARK: - 私有

    private func spawn() {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: config.binaryPath)
        proc.currentDirectoryURL = URL(fileURLWithPath: config.workingDirectory)

        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe

        attach(outPipe)
        attach(errPipe)

        proc.terminationHandler = { [weak self] p in
            DispatchQueue.main.async {
                guard let self else { return }
                self.readinessTimer?.invalidate()
                self.readinessTimer = nil
                self.logs.append("[启动器] 进程已退出，code=\(p.terminationStatus)")
                if self.process === p { self.process = nil }
                if self.state == .starting || self.state == .stopping {
                    self.state = .stopped
                }
            }
        }

        do {
            try proc.run()
            process = proc
            logs.append("[启动器] 已启动 wb2api (pid \(proc.processIdentifier))，等待服务就绪…")
            startReadiness()
        } catch {
            logs.append("[启动器] 启动失败：\(error.localizedDescription)")
            state = .stopped
        }
    }

    private func attach(_ pipe: Pipe) {
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty, let text = String(data: data, encoding: .utf8) else { return }
            DispatchQueue.main.async { self?.logs.ingest(text) }
        }
    }

    /// 轮询 healthz，直到服务就绪或超时。
    /// 注意：本方法必须在主线程调用（Timer 依赖主线程 RunLoop）。
    private func startReadiness() {
        readinessTimer?.invalidate()
        let deadline = Date().addingTimeInterval(20)
        readinessTimer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] timer in
            guard let self else { return }
            self.probeService { responded, isOurs in
                if isOurs {
                    timer.invalidate()
                    self.readinessTimer = nil
                    self.logs.append("[启动器] 服务已就绪：\(self.panelURL.absoluteString)")
                    self.state = .running(owned: true)
                } else if Date() > deadline {
                    timer.invalidate()
                    self.readinessTimer = nil
                    self.logs.append("[启动器] 等待服务就绪超时，请查看上方日志")
                }
            }
        }
    }

    /// 探测端口：responded=有 HTTP 响应；isOurs=响应来自本网关（healthz 含 workbuddy2api 标识）。
    /// completion 保证在主线程回调。
    private func probeService(completion: @escaping (_ responded: Bool, _ isOurs: Bool) -> Void) {
        var request = URLRequest(url: healthURL)
        request.timeoutInterval = 1
        request.cachePolicy = .reloadIgnoringLocalCacheData
        // healthz 在账号不可用时返回 503，但仍有响应体，故以「收到 HTTP 响应」作为就绪判据。
        URLSession.shared.dataTask(with: request) { data, response, _ in
            let http = response as? HTTPURLResponse
            let body = data.flatMap { String(data: $0, encoding: .utf8) }
            let header = http?.value(forHTTPHeaderField: "X-Service") ?? ""
            let responded = http != nil
            let isOurs = (body?.contains("workbuddy2api") ?? false) || header.contains("workbuddy2api")
            DispatchQueue.main.async { completion(responded, isOurs) }
        }.resume()
    }

    // MARK: - 对外探测 / 配置刷新

    /// 重新读取 config.json（端口 / api_key），并通知视图刷新。
    /// 用于「API 配置」弹窗等需要拿到最新配置的场景。
    func refreshConfig() {
        config.reloadPort()
        objectWillChange.send()
    }

    /// 切换网关目录：记录选择、停止当前服务并重新解析配置。
    func setHome(_ path: String) {
        AppConfig.setHome(path)
        if state != .stopped { stop() }
        config = AppConfig.load()
        logs.append("[配置] 网关目录已设为 \(path)（\(config.isInstalled ? "已找到 wb2api" : "未找到 wb2api")）")
        objectWillChange.send()
    }

    /// 健康探测：返回 healthz 是否由本网关正常响应。completion 在主线程回调。
    func checkHealth(completion: @escaping (_ healthy: Bool) -> Void) {
        probeService { _, isOurs in completion(isOurs) }
    }

    // MARK: - 账号池健康

    /// 运行期间每 10s 探测 `/healthz`：200 表示有可用账号，503 表示无可用账号（冷却/禁用/占满）。
    private func startPoolPolling() {
        poolTimer?.invalidate()
        poolTimer = Timer.scheduledTimer(withTimeInterval: 10, repeats: true) { [weak self] _ in
            self?.probePool()
        }
        probePool()
    }

    private func stopPoolPolling() {
        poolTimer?.invalidate()
        poolTimer = nil
        poolAvailable = nil
    }

    private func probePool() {
        guard isRunning else { return }
        var request = URLRequest(url: healthURL)
        request.timeoutInterval = 3
        request.cachePolicy = .reloadIgnoringLocalCacheData
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            struct Health: Decodable {
                let healthy: Int?
                let total: Int?
                let service: String?
            }
            let http = response as? HTTPURLResponse
            let decoded = data.flatMap { try? JSONDecoder().decode(Health.self, from: $0) }
            let isOurs = decoded?.service == "workbuddy2api"
            DispatchQueue.main.async {
                guard let self, self.isRunning else { return }
                // 无响应 / 非本网关时保持原状态（进程失联交由「保持网络」守护处理）。
                guard isOurs, let decoded else { return }
                self.poolHealthy = decoded.healthy ?? 0
                self.poolTotal = decoded.total ?? 0
                self.poolAvailable = (http?.statusCode == 200)
            }
        }.resume()
    }

    // MARK: - 子进程终止

    /// 逐级发送 SIGTERM → SIGINT → SIGKILL，直到进程确认退出（wb2api 不响应 SIGTERM，需 SIGINT 兜底）。
    private func terminateChild(_ proc: Process, reason: String) {
        let pid = proc.processIdentifier
        logs.append("[启动器] \(reason)：向 wb2api (pid \(pid)) 发送 SIGTERM …")
        proc.terminate()
        if waitForExit(proc, timeout: 2.0) { return }

        logs.append("[启动器] 未响应 SIGTERM，发送 SIGINT …")
        proc.interrupt()
        if waitForExit(proc, timeout: 2.0) { return }

        logs.append("[启动器] 仍未退出，强制发送 SIGKILL …")
        kill(pid, SIGKILL)
        _ = waitForExit(proc, timeout: 2.0)
    }

    /// 在超时时间内等待进程退出；返回是否已退出。
    @discardableResult
    private func waitForExit(_ proc: Process, timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if !isAlive(proc) { return true }
            usleep(50_000)
        }
        return !isAlive(proc)
    }

    /// 进程是否仍存活（Process.isRunning 与 kill(pid,0) 双重校验）。
    private func isAlive(_ proc: Process) -> Bool {
        if !proc.isRunning { return false }
        if kill(proc.processIdentifier, 0) != 0 && errno == ESRCH { return false }
        return true
    }

    /// 报告端口是否已释放。
    private func reportPortState(action: String) {
        if isPortBusy() {
            logs.append("[启动器] \(action)后端口 \(config.port) 仍被占用，请确认没有残留的 wb2api 进程")
        } else {
            logs.append("[启动器] \(action)后端口 \(config.port) 已释放，下次可正常启动")
        }
    }

    /// 本机 127.0.0.1:port 是否仍可建立连接（可连接即视为占用）。
    private func isPortBusy() -> Bool {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = UInt16(config.port).bigEndian
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let result = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        return result == 0
    }

    // MARK: - 外部实例的端口释放

    /// 结束监听本项目端口的 wb2api 进程（用于停止/退出时释放端口）。
    /// 仅处理命令行包含 wb2api 的进程，避免误杀其它程序。
    private func releasePortListeners(reason: String) {
        let pids = Array(Set(
            listenersOnPort().filter { $0.command.contains("wb2api") }.map(\.pid)
        ))
        guard !pids.isEmpty else {
            logs.append("[启动器] \(reason)：端口 \(config.port) 上没有本项目的 wb2api 进程")
            return
        }
        logs.append("[启动器] \(reason)：释放端口 \(config.port)，结束进程 \(pids)")

        for pid in pids { kill(pid, SIGTERM) }
        if waitForPidsExit(pids, timeout: 2.0) { return }

        logs.append("[启动器] 进程未响应 SIGTERM，发送 SIGINT …")
        for pid in pids { kill(pid, SIGINT) }
        if waitForPidsExit(pids, timeout: 2.0) { return }

        logs.append("[启动器] 仍未退出，强制发送 SIGKILL …")
        for pid in pids { kill(pid, SIGKILL) }
        _ = waitForPidsExit(pids, timeout: 2.0)
    }

    /// 等待给定 pid 全部退出；返回是否全部已退出。
    private func waitForPidsExit(_ pids: [pid_t], timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if pids.allSatisfy({ kill($0, 0) != 0 && errno == ESRCH }) { return true }
            usleep(50_000)
        }
        return pids.allSatisfy({ kill($0, 0) != 0 && errno == ESRCH })
    }

    /// 通过 lsof 列出监听本项目端口的进程及其命令名。
    private func listenersOnPort() -> [(pid: pid_t, command: String)] {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/sbin/lsof")
        proc.arguments = ["-nP", "-iTCP:\(config.port)", "-sTCP:LISTEN", "-Fpc"]
        let out = Pipe()
        proc.standardOutput = out
        proc.standardError = Pipe()
        do { try proc.run() } catch { return [] }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        proc.waitUntilExit()
        guard let text = String(data: data, encoding: .utf8) else { return [] }

        var result: [(pid_t, String)] = []
        var curPid: pid_t?
        var curCmd = ""
        for raw in text.split(separator: "\n") {
            guard let tag = raw.first else { continue }
            let value = String(raw.dropFirst())
            switch tag {
            case "p":
                if let p = curPid, !curCmd.isEmpty { result.append((p, curCmd)) }
                curPid = pid_t(value)
                curCmd = ""
            case "c":
                curCmd = value
            default:
                break
            }
        }
        if let p = curPid, !curCmd.isEmpty { result.append((p, curCmd)) }
        return result
    }
}
