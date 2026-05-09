import Foundation
import Photos
import UIKit
import WebKit

/// Receives WKScriptMessage actions from `window.IOS.*` and routes
/// outbound navigations: loopback stays in the WebView, anything else
/// hands off to Safari.
final class Bridge: NSObject, WKScriptMessageHandler, WKNavigationDelegate {
    weak var webView: WKWebView?

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
        default: break
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
        // Fallback for non-media (PDFs, archives, etc.) — share sheet so
        // the user picks Files / a third-party app.
        present(url: url, save: false)
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
            guard
                let scene = UIApplication.shared.connectedScenes
                    .compactMap({ $0 as? UIWindowScene })
                    .first,
                let root = scene.windows.first?.rootViewController
            else { return }
            let activities: [UIActivity]? = nil
            let vc = UIActivityViewController(
                activityItems: [url],
                applicationActivities: activities
            )
            // iPad popover anchor.
            vc.popoverPresentationController?.sourceView = self?.webView
            root.present(vc, animated: true)
        }
    }

    private func safeName(_ s: String) -> String? {
        let bad = CharacterSet(charactersIn: "/\\:*?\"<>|\0")
        let cleaned = s.components(separatedBy: bad).joined(separator: "_")
        return cleaned.isEmpty ? nil : cleaned
    }
}
