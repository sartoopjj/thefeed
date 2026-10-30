import Foundation
import Photos
import UIKit
import WebKit

/// Receives WKScriptMessage actions from `window.IOS.*` and routes
/// outbound navigations: loopback stays in the WebView, anything else
/// hands off to Safari.
final class Bridge: NSObject, WKScriptMessageHandler, WKNavigationDelegate {
    weak var webView: WKWebView?

    /// Per-launch HTTP Basic secret for the local server. Set by WebView
    /// before the first load; see ServerController.password.
    var localSecret: String = ""

    /// Answers the local server's Basic auth challenge.
    ///
    /// Loopback is not private on a phone: any other app with the INTERNET
    /// permission can reach this port, and the service starts before anything
    /// gates it. So the server requires a secret generated this launch, and
    /// this is the only place that knows it. Another app gets 401.
    ///
    /// `.forSession` persistence, not `.permanent`: the secret changes every
    /// launch, so storing it in the keychain would leave stale credentials
    /// behind and leak it past the process.
    func webView(
        _ webView: WKWebView,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        let space = challenge.protectionSpace
        guard space.authenticationMethod == NSURLAuthenticationMethodHTTPBasic,
              space.host == "127.0.0.1" || space.host == "localhost",
              !localSecret.isEmpty
        else {
            completionHandler(.performDefaultHandling, nil)
            return
        }
        // A second challenge for the same request means the secret was
        // rejected; retrying would loop.
        if challenge.previousFailureCount > 0 {
            completionHandler(.cancelAuthenticationChallenge, nil)
            return
        }
        // Any user name is accepted; only the password is checked.
        completionHandler(.useCredential, URLCredential(
            user: "thefeed", password: localSecret, persistence: .forSession))
    }

    func userContentController(
        _ userContentController: WKUserContentController,
        didReceive message: WKScriptMessage
    ) {
        guard
            let body = message.body as? [String: Any],
            let action = body["action"] as? String
        else { return }

        switch action {
        case "saveMedia": save(body)
        case "shareMedia": share(body)
        case "openMedia": share(body)  // iOS treats open and share via the same picker
        case "setLang": setLang(body)
        case "setSystemBars": setSystemBars(body)
        default: break
        }
    }

    // MARK: - System bars (status bar + safe-area appearance)

    // Force the window's appearance to the web theme so the status-bar icons and
    // the SwiftUI safe-area fill (driven by @Environment(\.colorScheme)) both
    // follow it — otherwise the notch / home-indicator bands stay dark in light
    // theme. The WebView content keeps its own CSS theme; this is only chrome.
    private func setSystemBars(_ body: [String: Any]) {
        let dark = (body["dark"] as? Bool) ?? true
        DispatchQueue.main.async { [weak self] in
            let style: UIUserInterfaceStyle = dark ? .dark : .light
            let window = self?.webView?.window
                ?? UIApplication.shared.windows.first(where: { $0.isKeyWindow })
                ?? UIApplication.shared.windows.first
            window?.overrideUserInterfaceStyle = style
        }
    }

    // MARK: - Navigation

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping (WKNavigationActionPolicy) -> Void
    ) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.allow)
            return
        }
        // Keep loopback inside the WebView; everything else goes to Safari.
        if let host = url.host, host == "127.0.0.1" || host == "localhost" {
            decisionHandler(.allow)
            return
        }
        if navigationAction.navigationType == .linkActivated || url.scheme == "https" || url.scheme == "http" {
            UIApplication.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    private func save(_ body: [String: Any]) {
        guard let url = decode(body) else { return }
        let mime = (body["mime"] as? String) ?? ""
        if mime.hasPrefix("image/") {
            saveImage(at: url)
            return
        }
        if mime.hasPrefix("video/") {
            saveVideo(at: url)
            return
        }
        // Non-media (backups, PDFs, archives): a document picker, so the user
        // gets a real "Save to Files" on both iOS and macOS.
        present(url: url, save: true)
    }

    private func share(_ body: [String: Any]) {
        guard let url = decode(body) else { return }
        present(url: url, save: false)
    }

    // MARK: - Save to Photos

    private func saveImage(at url: URL) {
        guard let data = try? Data(contentsOf: url),
              let image = UIImage(data: data) else {
            toast("Save failed: cannot decode image")
            return
        }
        requestPhotoAdd { [weak self] granted in
            guard granted else { self?.toast("Photo library access denied"); return }
            UIImageWriteToSavedPhotosAlbum(image, self, #selector(Bridge.didFinishSavingImage(_:didFinishSavingWithError:contextInfo:)), nil)
        }
    }

    private func saveVideo(at url: URL) {
        requestPhotoAdd { [weak self] granted in
            guard granted else { self?.toast("Photo library access denied"); return }
            PHPhotoLibrary.shared().performChanges({
                PHAssetChangeRequest.creationRequestForAssetFromVideo(atFileURL: url)
            }, completionHandler: { ok, err in
                DispatchQueue.main.async {
                    self?.toast(ok ? "Saved to Photos" : "Save failed: \(err?.localizedDescription ?? "unknown")")
                }
            })
        }
    }

    @objc private func didFinishSavingImage(
        _ image: UIImage,
        didFinishSavingWithError error: NSError?,
        contextInfo: UnsafeRawPointer
    ) {
        DispatchQueue.main.async { [weak self] in
            self?.toast(error == nil ? "Saved to Photos" : "Save failed: \(error!.localizedDescription)")
        }
    }

    private func requestPhotoAdd(_ handler: @escaping (Bool) -> Void) {
        // The add-only access level (.addOnly) and .limited status are iOS 14+;
        // iOS 13 only has the all-or-nothing authorization.
        if #available(iOS 14.0, *) {
            let status = PHPhotoLibrary.authorizationStatus(for: .addOnly)
            if status == .authorized || status == .limited {
                handler(true); return
            }
            if status == .denied || status == .restricted {
                handler(false); return
            }
            PHPhotoLibrary.requestAuthorization(for: .addOnly) { st in
                DispatchQueue.main.async { handler(st == .authorized || st == .limited) }
            }
        } else {
            let status = PHPhotoLibrary.authorizationStatus()
            if status == .authorized {
                handler(true); return
            }
            if status == .denied || status == .restricted {
                handler(false); return
            }
            PHPhotoLibrary.requestAuthorization { st in
                DispatchQueue.main.async { handler(st == .authorized) }
            }
        }
    }

    private func toast(_ msg: String) {
        webView?.evaluateJavaScript(
            "window.showToast && window.showToast(\(jsString(msg)))",
            completionHandler: nil
        )
    }

    private func jsString(_ s: String) -> String {
        let escaped = s
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
        return "\"\(escaped)\""
    }

    // MARK: - Language

    private func setLang(_ body: [String: Any]) {
        guard let lang = body["lang"] as? String else { return }
        UserDefaults.standard.set(lang, forKey: "tf.lang")
    }

    private func decode(_ body: [String: Any]) -> URL? {
        guard
            let b64 = body["body"] as? String,
            let data = Data(base64Encoded: b64),
            let name = (body["name"] as? String).flatMap(safeName)
        else { return nil }
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("share", isDirectory: true)
        try? FileManager.default.createDirectory(
            at: dir, withIntermediateDirectories: true
        )
        let url = dir.appendingPathComponent(name)
        do {
            try data.write(to: url, options: .atomic)
            return url
        } catch {
            return nil
        }
    }

    private func present(url: URL, save: Bool) {
        DispatchQueue.main.async { [weak self] in
            // We run a non-scene UIKit lifecycle (see AppDelegate), so
            // UIApplication.connectedScenes is empty — find the key window
            // directly and walk to the top-most presented controller.
            guard let top = Self.topViewController() else { return }
            let vc: UIViewController
            if save {
                // A save must offer "Save to Files". The share sheet does not
                // when this app runs on macOS (it lists only Shortcuts), which
                // left backups impossible to save there, so exports use the
                // document picker instead.
                if #available(iOS 14.0, *) {
                    vc = UIDocumentPickerViewController(forExporting: [url], asCopy: true)
                } else {
                    vc = UIDocumentPickerViewController(url: url, in: .exportToService)
                }
            } else {
                vc = UIActivityViewController(
                    activityItems: [url],
                    applicationActivities: nil
                )
            }
            // iPad popover anchor.
            vc.popoverPresentationController?.sourceView = self?.webView
            top.present(vc, animated: true)
        }
    }

    private static func topViewController() -> UIViewController? {
        let windows = UIApplication.shared.windows
        let root = windows.first(where: { $0.isKeyWindow })?.rootViewController
            ?? windows.first?.rootViewController
        var top = root
        while let presented = top?.presentedViewController {
            top = presented
        }
        return top
    }

    private func safeName(_ s: String) -> String? {
        let bad = CharacterSet(charactersIn: "/\\:*?\"<>|\0")
        let cleaned = s.components(separatedBy: bad).joined(separator: "_")
        return cleaned.isEmpty ? nil : cleaned
    }
}
