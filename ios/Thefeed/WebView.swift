import SwiftUI
import WebKit

struct WebView: UIViewRepresentable {
    let url: URL

    func makeCoordinator() -> Bridge { Bridge() }

    func makeUIView(context: Context) -> WKWebView {
        let cfg = WKWebViewConfiguration()
        cfg.websiteDataStore = .default()
        cfg.allowsInlineMediaPlayback = true
        cfg.mediaTypesRequiringUserActionForPlayback = []

        let userContent = WKUserContentController()
        userContent.add(context.coordinator, name: "thefeed")
        cfg.userContentController = userContent

        let view = WKWebView(frame: .zero, configuration: cfg)
        view.allowsBackForwardNavigationGestures = true
        view.scrollView.bounces = true
        // Don't double-pad the bottom: the page already adds
        // env(safe-area-inset-bottom) via CSS; the WebView's automatic
        // content inset would stack on top and leave a huge gap.
        view.scrollView.contentInsetAdjustmentBehavior = .never
        view.scrollView.contentInset = .zero
        view.scrollView.scrollIndicatorInsets = .zero
        view.navigationDelegate = context.coordinator
        context.coordinator.webView = view

        userContent.addUserScript(WKUserScript(
            source: shimSource(),
            injectionTime: .atDocumentStart,
            forMainFrameOnly: true
        ))

        view.load(URLRequest(url: url))
        return view
    }

    private func shimSource() -> String {
        let lang = resolveLang()
        let langJS = lang.replacingOccurrences(of: "\"", with: "\\\"")
        return """
        window.IOS = {
          isIOS: true,
          _lang: "\(langJS)",
          getLang: function() { return this._lang; },
          setLang: function(l) {
            this._lang = l;
            window.webkit.messageHandlers.thefeed.postMessage(
              { action: 'setLang', lang: l });
          },
          saveMedia: function(b64, mime, name) {
            window.webkit.messageHandlers.thefeed.postMessage(
              { action: 'saveMedia', body: b64, mime: mime, name: name });
          },
          shareMedia: function(b64, mime, name) {
            window.webkit.messageHandlers.thefeed.postMessage(
              { action: 'shareMedia', body: b64, mime: mime, name: name });
          },
          openMedia: function(b64, mime, name) {
            window.webkit.messageHandlers.thefeed.postMessage(
              { action: 'openMedia', body: b64, mime: mime, name: name });
          }
        };
        """
    }

    /// Returns the saved language ("" on first launch) so the WebView's
    /// own language picker can detect first-launch and show its modal.
    private func resolveLang() -> String {
        return UserDefaults.standard.string(forKey: "tf.lang") ?? ""
    }

    func updateUIView(_ view: WKWebView, context: Context) {
        if view.url != url {
            view.load(URLRequest(url: url))
        }
    }
}
