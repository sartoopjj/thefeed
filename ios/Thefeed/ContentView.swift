import SwiftUI
import UIKit

struct ContentView: View {
    @EnvironmentObject var server: ServerController
    // Follows the window's overrideUserInterfaceStyle, which the web theme drives
    // via Bridge.setSystemBars — so the band recolors with the app.
    @Environment(\.colorScheme) private var colorScheme

    var body: some View {
        ZStack {
            // Background fills the notch + home-indicator area with the page's
            // --bg2 so there's no visible band; the WebView itself stays *inside*
            // the safe area so the page's CSS env(safe-area-inset-*) returns 0 and
            // we don't double-pad (system inset + body padding). The color tracks
            // light/dark so the bands aren't a fixed dark in light theme.
            //
            // Color(.sRGB, red:…) and edgesIgnoringSafeArea(.all) are the
            // iOS-13-compatible spellings of Color(red:…) / ignoresSafeArea().
            (colorScheme == .dark
                ? Color(.sRGB, red: 14 / 255, green: 22 / 255, blue: 33 / 255, opacity: 1)    // --bg2 dark  #0e1621
                : Color(.sRGB, red: 240 / 255, green: 242 / 255, blue: 245 / 255, opacity: 1)) // --bg2 light #f0f2f5
                .fillSafeArea()
            if server.port > 0, !server.password.isEmpty {
                WebView(url: URL(string: "http://127.0.0.1:\(server.port)")!,
                        secret: server.password)
                    // Force a fresh WebView (and page reload) on every server
                    // restart, even when the port is unchanged. See
                    // ServerController.generation.
                    .id(server.generation)
                    .ignoreKeyboardSafeArea()
            } else if let err = server.lastError {
                VStack(spacing: 12) {
                    Text("startup failed").font(.headline).foregroundColor(.white)
                    Text(err).font(.caption).foregroundColor(.secondary)
                    Button("retry") { server.start() }
                }
                .padding()
            } else {
                LoadingSpinner()
            }
        }
    }
}

// ProgressView is iOS 14+; fall back to a UIKit spinner on iOS 13.
private struct LoadingSpinner: View {
    var body: some View {
        if #available(iOS 14.0, *) {
            ProgressView()
        } else {
            ActivityIndicator()
        }
    }
}

private struct ActivityIndicator: UIViewRepresentable {
    func makeUIView(context: Context) -> UIActivityIndicatorView {
        let v = UIActivityIndicatorView(style: .large)
        v.startAnimating()
        return v
    }
    func updateUIView(_ view: UIActivityIndicatorView, context: Context) {}
}

private extension View {
    // ignoresSafeArea() is iOS 14+; fall back to edgesIgnoringSafeArea on 13.
    @ViewBuilder func fillSafeArea() -> some View {
        if #available(iOS 14.0, *) {
            self.ignoresSafeArea()
        } else {
            self.edgesIgnoringSafeArea(.all)
        }
    }

    // ignoresSafeArea(.keyboard) is iOS 14+; iOS 13 has no automatic SwiftUI
    // keyboard avoidance to opt out of, so it's a no-op there.
    @ViewBuilder func ignoreKeyboardSafeArea() -> some View {
        if #available(iOS 14.0, *) {
            self.ignoresSafeArea(.keyboard)
        } else {
            self
        }
    }
}
