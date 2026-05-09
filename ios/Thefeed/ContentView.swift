import SwiftUI

struct ContentView: View {
    @EnvironmentObject var server: ServerController

    var body: some View {
        ZStack {
            // Background fills the notch + home-indicator area with the
            // same color as the page so there's no visible band; the
            // WebView itself stays *inside* the safe area so the page's
            // CSS env(safe-area-inset-*) returns 0 and we don't end up
            // double-padding (system inset + body padding).
            Color(red: 0.07, green: 0.09, blue: 0.13).ignoresSafeArea()
            if server.port > 0 {
                WebView(url: URL(string: "http://127.0.0.1:\(server.port)")!)
                    .ignoresSafeArea(.keyboard)
            } else if let err = server.lastError {
                VStack(spacing: 12) {
                    Text("startup failed").font(.headline).foregroundColor(.white)
                    Text(err).font(.caption).foregroundColor(.secondary)
                    Button("retry") { server.start() }
                }
                .padding()
            } else {
                ProgressView()
            }
        }
    }
}
