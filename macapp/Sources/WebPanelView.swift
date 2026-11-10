import SwiftUI
import WebKit

/// 内嵌管理面板窗口：用 WKWebView 加载本机网关面板，随 app 退出一起关闭。
struct WebPanelView: View {
    let url: URL

    var body: some View {
        WebView(url: url)
    }
}

private struct WebView: NSViewRepresentable {
    let url: URL

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        // 面板前端编译在 wb2api 二进制内、无源码可改，这里注入一小段样式修正其排版缺陷。
        config.userContentController.addUserScript(Self.layoutFixScript)
        let webView = WKWebView(frame: .zero, configuration: config)
        webView.uiDelegate = context.coordinator
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {
        if webView.url == nil {
            webView.load(URLRequest(url: url))
        }
    }

    /// 面板的「版本」单选行在窄弹窗内会把中文标签压成两行，
    /// 叠加 `.realm-opt{align-items:center}` 导致圆点落在两行中间（视觉错位）。
    /// 让整段标签不被压缩换行即可恢复圆点与文字对齐。
    private static let layoutFixScript: WKUserScript = {
        let css = ".realm-row{flex-wrap:wrap}.realm-opt{white-space:nowrap;flex-shrink:0}"
        let js = "var s=document.createElement('style');s.textContent='\(css)';document.head.appendChild(s);"
        return WKUserScript(source: js, injectionTime: .atDocumentEnd, forMainFrameOnly: false)
    }()

    /// 面板用 `window.open(url, '_blank')`（如「在浏览器打开」）打开登录页。
    /// WKWebView 默认丢弃这类新窗口请求，这里改为交给系统默认浏览器打开。
    final class Coordinator: NSObject, WKUIDelegate {
        func webView(_ webView: WKWebView,
                     createWebViewWith configuration: WKWebViewConfiguration,
                     for navigationAction: WKNavigationAction,
                     windowFeatures: WKWindowFeatures) -> WKWebView? {
            if let target = navigationAction.request.url {
                NSWorkspace.shared.open(target)
            }
            return nil
        }
    }
}
