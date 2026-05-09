import SwiftUI

@main
struct ThefeedApp: App {
    @StateObject private var server = ServerController()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(server)
                .onAppear { server.start() }
                .onDisappear { server.stop() }
        }
    }
}
