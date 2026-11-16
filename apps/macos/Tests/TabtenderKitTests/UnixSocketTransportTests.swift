import XCTest
@testable import TabtenderKit

/// A Unix socket listener that can answer one connection with canned bytes.
private final class TestListener {
    let path: String
    private let descriptor: Int32

    init() throws {
        path = "/tmp/tt-test-\(UUID().uuidString.prefix(8)).sock"
        descriptor = socket(AF_UNIX, SOCK_STREAM, 0)
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(path.utf8)) }
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard descriptor >= 0, bound == 0, listen(descriptor, 4) == 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
    }

    /// Accepts one connection on a background thread, reads the request
    /// head, replies with `response`, and closes. Fulfills `received` with
    /// the request text.
    func answerOnce(with response: String, received: @escaping @Sendable (String) -> Void) {
        let descriptor = self.descriptor
        Thread.detachNewThread {
            let connection = accept(descriptor, nil, nil)
            guard connection >= 0 else { return }
            defer { Darwin.close(connection) }
            var request = Data()
            var buffer = [UInt8](repeating: 0, count: 4096)
            while request.range(of: Data("\r\n\r\n".utf8)) == nil {
                let count = recv(connection, &buffer, buffer.count, 0)
                guard count > 0 else { break }
                request.append(contentsOf: buffer[..<count])
            }
            received(String(decoding: request, as: UTF8.self))
            let bytes = Array(response.utf8)
            _ = Darwin.send(connection, bytes, bytes.count, 0)
        }
    }

    /// Stops listening. The socket file stays behind unless `unlinking`.
    func close(unlinking: Bool = true) {
        Darwin.close(descriptor)
        if unlinking {
            unlink(path)
        }
    }
}

final class UnixSocketTransportTests: XCTestCase {
    func testRoundTripAgainstRealSocket() throws {
        let listener = try TestListener()
        defer { listener.close() }
        let received = expectation(description: "request received")
        listener.answerOnce(with: "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok") { request in
            XCTAssertTrue(request.hasPrefix("GET /mgmt/v1/status HTTP/1.1\r\n"))
            XCTAssertTrue(request.contains("\r\nConnection: close\r\n"))
            received.fulfill()
        }

        let request = HTTPRequest(method: "GET", path: "/mgmt/v1/status").serialized()
        let raw = try UnixSocketTransport(path: listener.path).roundTrip(request, timeout: 5)
        wait(for: [received], timeout: 5)

        let response = try HTTPResponse(parsing: raw)
        XCTAssertEqual(response.status, 200)
        XCTAssertEqual(response.body, Data("ok".utf8))
    }

    func testMissingSocketMeansNotRunning() {
        let transport = UnixSocketTransport(path: "/tmp/tt-test-missing-\(UUID().uuidString.prefix(8)).sock")
        XCTAssertThrowsError(try transport.roundTrip(Data(), timeout: 5)) { error in
            XCTAssertEqual(error as? ManagementError, .notRunning)
        }
    }

    func testStaleSocketFileMeansNotRunning() throws {
        let listener = try TestListener()
        listener.close(unlinking: false)
        defer { unlink(listener.path) }
        XCTAssertTrue(FileManager.default.fileExists(atPath: listener.path))

        XCTAssertThrowsError(try UnixSocketTransport(path: listener.path).roundTrip(Data(), timeout: 5)) { error in
            XCTAssertEqual(error as? ManagementError, .notRunning)
        }
    }

    func testUnresponsivePeerTimesOut() throws {
        // The listener never accepts: the connection sits in its backlog.
        let listener = try TestListener()
        defer { listener.close() }

        let started = Date()
        let request = HTTPRequest(method: "GET", path: "/mgmt/v1/status").serialized()
        XCTAssertThrowsError(try UnixSocketTransport(path: listener.path).roundTrip(request, timeout: 0.5)) { error in
            XCTAssertEqual(error as? ManagementError, .timedOut)
        }
        let elapsed = Date().timeIntervalSince(started)
        XCTAssertGreaterThanOrEqual(elapsed, 0.4)
        XCTAssertLessThan(elapsed, 3)
    }

    func testOverlongSocketPathIsRejected() {
        let transport = UnixSocketTransport(path: "/tmp/" + String(repeating: "x", count: 200))
        XCTAssertThrowsError(try transport.roundTrip(Data(), timeout: 1)) { error in
            guard case ManagementError.transport = error else {
                return XCTFail("unexpected error \(error)")
            }
        }
    }

    func testClientReportsNotRunningWithoutHanging() async {
        let client = ManagementClient(socketPath: "/tmp/tt-test-missing-\(UUID().uuidString.prefix(8)).sock")
        let started = Date()
        do {
            _ = try await client.status()
            XCTFail("expected notRunning")
        } catch {
            XCTAssertEqual(error as? ManagementError, .notRunning)
            XCTAssertEqual(error.localizedDescription, "The Tabtender service is not running.")
        }
        XCTAssertLessThan(Date().timeIntervalSince(started), 1)
    }
}
