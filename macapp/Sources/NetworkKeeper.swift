import Foundation
import Combine
import Network

/// 「保持网络」守护。
///
/// 背景：网络切换 / 掉线 / 重连后，网关与上游账号之间已建立的 TCP 连接会变为陈旧
/// (stale)；而网关只有在 `header_timeout_seconds`(120s) / `idle_timeout_seconds`(300s)
/// 超时后才会判死，期间会话粘滞在旧连接上，AI IDE 表现为「卡住」，需手动停/启服务。
/// 网关本身没有重置连接池或强制重连的接口，因此本守护在检测到网络恢复 / 网络出口变化 /
/// 服务失联时自动重启服务，用最快的方式重建上游连接，让 AI IDE 无需手动操作即可继续。
final class NetworkKeeper: ObservableObject {
    static let shared = NetworkKeeper(manager: .shared)

    /// 用户开关：是否启用网络守护（持久化到 UserDefaults，启动后自动生效）。
    @Published private(set) var isEnabled: Bool
    /// 当前网络是否可用。
    @Published private(set) var networkAvailable = true

    private let manager: ProcessManager
    private let monitor = NWPathMonitor()
    private let monitorQueue = DispatchQueue(label: "workbuddy2api.network-keeper")
    private let defaultsKey = "workbuddy2api.keepNetwork"

    private var probeTimer: Timer?
    private var pendingRecovery: DispatchWorkItem?
    /// 上一次「网络出口类型」签名（wifi / wired / cellular / other 的组合），用于识别真实出口切换。
    private var lastPathSignature: String?
    private var probeFailures = 0
    private var isRecovering = false

    private init(manager: ProcessManager) {
        self.manager = manager
        self.isEnabled = UserDefaults.standard.bool(forKey: defaultsKey)
        monitor.pathUpdateHandler = { [weak self] path in
            DispatchQueue.main.async { self?.handlePath(path) }
        }
        monitor.start(queue: monitorQueue)
        if isEnabled { startProbing() }
    }

    func setEnabled(_ on: Bool) {
        guard on != isEnabled else { return }
        isEnabled = on
        UserDefaults.standard.set(on, forKey: defaultsKey)
        if on {
            manager.logs.append("[网络守护] 已开启：网络变化/恢复或服务失联时将自动重启以重建连接")
            startProbing()
        } else {
            manager.logs.append("[网络守护] 已关闭")
            stopProbing()
            pendingRecovery?.cancel()
            pendingRecovery = nil
            probeFailures = 0
        }
    }

    // MARK: - 网络路径

    private func handlePath(_ path: NWPath) {
        let satisfied = path.status == .satisfied
        let signature = Self.pathSignature(path)
        let previousSignature = lastPathSignature
        let wasSatisfied = networkAvailable

        networkAvailable = satisfied
        lastPathSignature = signature

        guard isEnabled else { return }

        if !satisfied {
            if wasSatisfied { manager.logs.append("[网络守护] 网络已断开，等待恢复…") }
            return
        }
        if !wasSatisfied {
            manager.logs.append("[网络守护] 网络已恢复")
            scheduleRecovery(reason: "网络恢复", delay: 3)
        } else if let previousSignature, signature != previousSignature {
            manager.logs.append("[网络守护] 网络出口类型由 \(previousSignature) 变为 \(signature)")
            scheduleRecovery(reason: "网络出口变化", delay: 3)
        }
    }

    /// 网络出口类型签名：只关心 wifi / wired / cellular / other 的组合，
    /// 从而忽略 macOS 上 awdl0 / utun 等接口的频繁增减，避免误触发重启。
    private static func pathSignature(_ path: NWPath) -> String {
        var types: [String] = []
        if path.usesInterfaceType(.wifi) { types.append("wifi") }
        if path.usesInterfaceType(.wiredEthernet) { types.append("wired") }
        if path.usesInterfaceType(.cellular) { types.append("cellular") }
        if path.usesInterfaceType(.other) { types.append("other") }
        return types.joined(separator: "+")
    }

    // MARK: - 健康探测

    private func startProbing() {
        probeTimer?.invalidate()
        probeTimer = Timer.scheduledTimer(withTimeInterval: 20, repeats: true) { [weak self] _ in
            self?.probe()
        }
        probe()
    }

    private func stopProbing() {
        probeTimer?.invalidate()
        probeTimer = nil
    }

    /// 周期性探测本地服务；连续 3 次无响应则判定服务失联并重启。
    /// 注意：网络离线时本地 healthz 仍应可响应，因此失败须排除「网络离线」这一情形。
    private func probe() {
        guard isEnabled, !isRecovering, manager.isRunning else { return }
        manager.checkHealth { [weak self] healthy in
            guard let self, self.isEnabled, !self.isRecovering else { return }
            if healthy {
                self.probeFailures = 0
                return
            }
            guard self.networkAvailable else { return }
            self.probeFailures += 1
            if self.probeFailures >= 3 {
                self.probeFailures = 0
                self.scheduleRecovery(reason: "服务无响应", delay: 0)
            }
        }
    }

    // MARK: - 恢复

    private func scheduleRecovery(reason: String, delay: TimeInterval) {
        guard isEnabled, !isRecovering, manager.isRunning else { return }
        pendingRecovery?.cancel()
        manager.logs.append("[网络守护] \(reason)，\(delay > 0 ? "\(Int(delay)) 秒后" : "即将")重启服务以重建连接…")
        let work = DispatchWorkItem { [weak self] in self?.recover(reason: reason) }
        pendingRecovery = work
        DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: work)
    }

    private func recover(reason: String) {
        guard isEnabled, !isRecovering, manager.isRunning else { return }
        isRecovering = true
        probeFailures = 0
        manager.logs.append("[网络守护] 正在重启服务（原因：\(reason)）…")
        manager.stop()
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self] in
            guard let self else { return }
            self.isRecovering = false
            guard self.isEnabled else { return }
            self.manager.start()
            self.manager.logs.append("[网络守护] 服务已重启，上游连接已重建")
        }
    }
}
