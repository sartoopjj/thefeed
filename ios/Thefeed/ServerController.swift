import Foundation
import Mobile
import UIKit

/// Owns the embedded gomobile-backed HTTP server.
///
/// The server is kept ALIVE for the whole app session — it is deliberately
/// NOT stopped on background and recreated on foreground. iOS only *suspends*
/// the process on app-switch / screen-lock (memory stays intact), so the
/// running resolver scan and bank rescan simply freeze and thaw on return —
/// true continuation with no state loss. The previous stop-on-background /
/// start-on-foreground churn (a) cancelled in-progress work and (b) could
/// leave two servers briefly writing profiles.json at once. On a genuine
/// background *kill* (memory pressure / long background), iOS cold-launches
/// the app and `AppDelegate` calls `start()` again from scratch.
final class ServerController: ObservableObject {
    @Published private(set) var port: Int = 0
    // Per-launch HTTP Basic secret the local server requires. Handed to the
    // WebView, whose navigation delegate answers the auth challenge with it.
    @Published private(set) var password: String = ""
    @Published private(set) var lastError: String?
    /// Bumped on every successful (re)start. ContentView keys the WebView on
    /// this so a cold start / retry reloads the page against the fresh server.
    /// It is NOT bumped on a plain foreground (the server is unchanged then,
    /// so the WebView — and the resolver/scanner view the user was watching —
    /// is preserved while its JS timers and SSE resume).
    @Published private(set) var generation: Int = 0

    private var instance: MobileServer?
    private var observers: [NSObjectProtocol] = []
    private var bgTask: UIBackgroundTaskIdentifier = .invalid

    init() {
        let center = NotificationCenter.default
        observers.append(center.addObserver(
            forName: UIApplication.didEnterBackgroundNotification,
            object: nil, queue: .main
        ) { [weak self] _ in self?.beginBackgroundGrace() })
        observers.append(center.addObserver(
            forName: UIApplication.willEnterForegroundNotification,
            object: nil, queue: .main
        ) { [weak self] _ in self?.endBackgroundGrace() })
    }

    deinit {
        observers.forEach(NotificationCenter.default.removeObserver)
        endBackgroundGrace()
        instance?.stop()
    }

    // beginBackgroundGrace asks iOS for a short window (~30s) before the
    // process is suspended, so a nearly-finished scan can complete instead of
    // freezing mid-flight. iOS grants only finite time — there is no API to
    // keep an HTTP server running in the background indefinitely.
    private func beginBackgroundGrace() {
        endBackgroundGrace()
        bgTask = UIApplication.shared.beginBackgroundTask(withName: "thefeed.serverGrace") {
            [weak self] in self?.endBackgroundGrace()
        }
    }

    private func endBackgroundGrace() {
        if bgTask != .invalid {
            UIApplication.shared.endBackgroundTask(bgTask)
            bgTask = .invalid
        }
    }

    func start() {
        guard instance == nil else { return }
        do {
            let dir = try Self.dataDir()
            let saved = UserDefaults.standard.integer(forKey: "tf.lastPort")
            var err: NSError?
            guard let s = MobileNewServer(dir.path, saved, &err) else {
                lastError = err?.localizedDescription ?? "server start failed"
                return
            }
            instance = s
            // Loopback is not private on a phone: another app can reach this
            // port. This secret is what the WebView has and nothing else does.
            password = s.password()
            let actual = Int(s.port())
            port = actual
            generation += 1
            UserDefaults.standard.set(actual, forKey: "tf.lastPort")
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    func stop() {
        instance?.stop()
        instance = nil
        port = 0
        password = ""
    }

    private static func dataDir() throws -> URL {
        let docs = try FileManager.default.url(
            for: .documentDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        let dir = docs.appendingPathComponent("thefeeddata", isDirectory: true)
        try FileManager.default.createDirectory(
            at: dir, withIntermediateDirectories: true
        )
        return dir
    }
}
