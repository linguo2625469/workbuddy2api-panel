import Foundation
import Combine

/// 运行日志环形缓冲：按行保存，超出上限丢弃最旧行；并周期性自动清空，避免长期累积占用内存。
final class LogStore: ObservableObject {
    @Published private(set) var lines: [String] = []

    private let maxLines = 3000
    private var partial = ""

    /// 自动清空间隔：每 30 分钟清空一次历史日志。
    private let autoClearInterval: TimeInterval = 30 * 60
    private var autoClearTimer: Timer?

    init() {
        autoClearTimer = Timer.scheduledTimer(withTimeInterval: autoClearInterval, repeats: true) { [weak self] _ in
            self?.autoClear()
        }
    }

    deinit {
        autoClearTimer?.invalidate()
    }

    /// 主线程调用：追加一整行（自动补前缀无需，交由调用方决定）。
    func append(_ line: String) {
        lines.append(line)
        if lines.count > maxLines {
            lines.removeFirst(lines.count - maxLines)
        }
    }

    /// 处理子进程分片输出：按换行切分，未闭合的尾部暂存。
    func ingest(_ chunk: String) {
        partial += chunk
        while let idx = partial.firstIndex(of: "\n") {
            var line = String(partial[partial.startIndex..<idx])
            if line.hasSuffix("\r") { line.removeLast() }
            append(line)
            partial = String(partial[partial.index(after: idx)...])
        }
    }

    func clear() {
        lines.removeAll()
        partial = ""
    }

    /// 自动清空：仅在存在日志时执行，并留一行记录本次清理。
    private func autoClear() {
        guard !lines.isEmpty else { return }
        let removed = lines.count
        clear()
        append("[日志] 已自动清空 \(removed) 行历史日志（每 \(Int(autoClearInterval / 60)) 分钟自动清理一次）")
    }
}
