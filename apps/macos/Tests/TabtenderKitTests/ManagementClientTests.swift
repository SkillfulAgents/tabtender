import XCTest
@testable import TabtenderKit

/// A transport that replays a canned response and records the request.
private final class StubTransport: ManagementTransport, @unchecked Sendable {
    private let lock = NSLock()
    private let result: Result<Data, Error>
    private var recorded: [(request: String, timeout: TimeInterval)] = []

    init(_ result: Result<Data, Error>) {
        self.result = result
    }

    convenience init(status: String, json: String) {
        self.init(.success(Data("HTTP/1.1 \(status)\r\nContent-Length: \(json.utf8.count)\r\n\r\n\(json)".utf8)))
    }

    var requests: [(request: String, timeout: TimeInterval)] {
        lock.lock()
        defer { lock.unlock() }
        return recorded
    }

    func roundTrip(_ request: Data, timeout: TimeInterval) throws -> Data {
        lock.lock()
        recorded.append((String(decoding: request, as: UTF8.self), timeout))
        lock.unlock()
        return try result.get()
    }
}

final class ManagementClientTests: XCTestCase {
    private func assertThrows<T>(
        _ expected: ManagementError,
        _ operation: () async throws -> T,
        file: StaticString = #filePath, line: UInt = #line
    ) async {
        do {
            _ = try await operation()
            XCTFail("expected \(expected)", file: file, line: line)
        } catch {
            XCTAssertEqual(error as? ManagementError, expected, file: file, line: line)
        }
    }

    func testDecodesErrorBodyIntoTypedError() async {
        let transport = StubTransport(
            status: "400 Bad Request",
            json: #"{"error":{"code":"validation_error","message":"server.port must be between 1 and 65535, got 0"}}"#
        )
        let client = ManagementClient(transport: transport)
        let expected = ManagementError.api(
            status: 400, code: "validation_error", message: "server.port must be between 1 and 65535, got 0"
        )
        await assertThrows(expected) { try await client.config() }
        XCTAssertEqual(expected.code, "validation_error")
        XCTAssertEqual(expected.localizedDescription, "server.port must be between 1 and 65535, got 0")
    }

    func testErrorStatusWithoutErrorBodyIsInvalidResponse() async {
        let client = ManagementClient(transport: StubTransport(status: "502 Bad Gateway", json: "oops"))
        await assertThrows(.invalidResponse("HTTP 502 without an error body")) { try await client.status() }
    }

    func testUndecodableSuccessBodyIsInvalidResponse() async {
        let client = ManagementClient(transport: StubTransport(status: "200 OK", json: #"{"unexpected":true}"#))
        do {
            _ = try await client.apiKey()
            XCTFail("expected a decoding failure")
        } catch {
            guard case ManagementError.invalidResponse = error else {
                return XCTFail("unexpected error \(error)")
            }
        }
    }

    func testTransportErrorsPropagate() async {
        let client = ManagementClient(transport: StubTransport(.failure(ManagementError.notRunning)))
        await assertThrows(.notRunning) { try await client.status() }
    }

    func testReadsAndMutationsUseTheirOwnTimeouts() async throws {
        let transport = StubTransport(
            status: "200 OK", json: #"{"apiKey":"tt_abc","source":"file","rotatable":true}"#
        )
        let client = ManagementClient(transport: transport, timeout: 3, mutationTimeout: 42)

        let key = try await client.apiKey()
        XCTAssertEqual(key, APIKeyInfo(apiKey: "tt_abc", source: "file", rotatable: true))
        _ = try await client.rotateAPIKey()

        let requests = transport.requests
        XCTAssertEqual(requests.count, 2)
        XCTAssertTrue(requests[0].request.hasPrefix("GET /mgmt/v1/apikey HTTP/1.1\r\n"))
        XCTAssertEqual(requests[0].timeout, 3)
        XCTAssertTrue(requests[1].request.hasPrefix("POST /mgmt/v1/apikey/rotate HTTP/1.1\r\n"))
        XCTAssertEqual(requests[1].timeout, 42)
    }

    func testSessionIdentifiersAreEscapedInPaths() async {
        let transport = StubTransport(status: "404 Not Found", json: #"{"error":{"code":"not_found","message":"gone"}}"#)
        let client = ManagementClient(transport: transport)
        await assertThrows(.api(status: 404, code: "not_found", message: "gone")) {
            try await client.terminateSession(id: "a/b c")
        }
        XCTAssertTrue(transport.requests[0].request.hasPrefix("DELETE /v1/sessions/a%2Fb%20c HTTP/1.1\r\n"))
    }

    func testLogsQueryCarriesLimitAndLevel() async throws {
        let transport = StubTransport(
            status: "200 OK",
            json: #"{"entries":[{"time":"2026-11-16T10:00:00.123456789Z","level":"WARN","message":"hello","attrs":{"k":"v"}}]}"#
        )
        let entries = try await ManagementClient(transport: transport).logs(limit: 5, minimumLevel: .warn)
        XCTAssertTrue(transport.requests[0].request.hasPrefix("GET /mgmt/v1/logs?limit=5&level=warn HTTP/1.1\r\n"))
        XCTAssertEqual(entries.count, 1)
        XCTAssertEqual(entries[0].level, "WARN")
        XCTAssertEqual(entries[0].attrs, ["k": "v"])
    }
}
