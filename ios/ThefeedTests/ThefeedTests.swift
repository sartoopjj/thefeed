import XCTest
import Mobile

final class ThefeedTests: XCTestCase {
    func testRejectsEmptyDir() {
        var err: NSError?
        let s = MobileNewServer("", &err)
        XCTAssertNil(s)
        XCTAssertNotNil(err)
    }

    func testServeAndStop() throws {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("thefeed-test-\(UUID().uuidString)")
        try FileManager.default.createDirectory(
            at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }

        var err: NSError?
        guard let server = MobileNewServer(dir.path, &err) else {
            XCTFail("start failed: \(err?.localizedDescription ?? "?")")
            return
        }
        XCTAssertGreaterThan(server.port(), 0)

        let url = URL(string: "http://127.0.0.1:\(server.port())/api/status")!
        let exp = expectation(description: "status")
        let task = URLSession.shared.dataTask(with: url) { _, resp, _ in
            if let http = resp as? HTTPURLResponse {
                XCTAssertEqual(http.statusCode, 200)
            }
            exp.fulfill()
        }
        task.resume()
        wait(for: [exp], timeout: 10)

        server.stop()
    }
}
