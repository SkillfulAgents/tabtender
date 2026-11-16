import XCTest
@testable import TabtenderKit

final class HTTPMessageTests: XCTestCase {
    private func response(_ text: String) throws -> HTTPResponse {
        try HTTPResponse(parsing: Data(text.utf8))
    }

    func testParsesStatusHeadersAndBody() throws {
        let parsed = try response(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json; charset=utf-8\r\nContent-Length: 11\r\n\r\n{\"ok\":true}"
        )
        XCTAssertEqual(parsed.status, 200)
        XCTAssertEqual(parsed.headers["content-type"], "application/json; charset=utf-8")
        XCTAssertEqual(String(decoding: parsed.body, as: UTF8.self), "{\"ok\":true}")
    }

    func testHeaderNamesAreCaseInsensitive() throws {
        let parsed = try response("HTTP/1.1 200 OK\r\ncontent-LENGTH:2\r\n\r\nhi")
        XCTAssertEqual(parsed.body, Data("hi".utf8))
    }

    func testBodyIsLimitedToContentLength() throws {
        let parsed = try response("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi-and-trailing-bytes")
        XCTAssertEqual(parsed.body, Data("hi".utf8))
    }

    func testBodyWithoutContentLengthRunsToEndOfStream() throws {
        let parsed = try response("HTTP/1.0 404 Not Found\r\n\r\nmissing")
        XCTAssertEqual(parsed.status, 404)
        XCTAssertEqual(parsed.body, Data("missing".utf8))
    }

    func testEmptyBody() throws {
        let parsed = try response("HTTP/1.1 204 No Content\r\n\r\n")
        XCTAssertEqual(parsed.status, 204)
        XCTAssertTrue(parsed.body.isEmpty)
    }

    func testBodyMayContainHeaderTerminator() throws {
        let parsed = try response("HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nab\r\n\r\ncd")
        XCTAssertEqual(parsed.body, Data("ab\r\n\r\ncd".utf8))
    }

    func testRejectsMalformedResponses() {
        let malformed = [
            "",
            "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n",
            "SPDY/3 200 OK\r\n\r\n",
            "HTTP/1.1 abc OK\r\n\r\n",
            "HTTP/1.1 200 OK\r\nno-colon-here\r\n\r\n",
            "HTTP/1.1 200 OK\r\nContent-Length: nope\r\n\r\n",
            "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nshort",
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nhi\r\n0\r\n\r\n",
        ]
        for text in malformed {
            XCTAssertThrowsError(try response(text), "accepted: \(text.debugDescription)") { error in
                guard case ManagementError.invalidResponse = error else {
                    return XCTFail("unexpected error \(error) for \(text.debugDescription)")
                }
            }
        }
    }

    func testSerializesRequestWithoutBody() {
        let request = HTTPRequest(method: "GET", path: "/mgmt/v1/status")
        XCTAssertEqual(
            String(decoding: request.serialized(), as: UTF8.self),
            "GET /mgmt/v1/status HTTP/1.1\r\nHost: tabtender\r\nAccept: application/json\r\nConnection: close\r\n\r\n"
        )
    }

    func testSerializesRequestWithBody() {
        let request = HTTPRequest(method: "PUT", path: "/mgmt/v1/config", body: Data("{}".utf8))
        let text = String(decoding: request.serialized(), as: UTF8.self)
        XCTAssertTrue(text.hasPrefix("PUT /mgmt/v1/config HTTP/1.1\r\n"))
        XCTAssertTrue(text.contains("\r\nContent-Type: application/json\r\n"))
        XCTAssertTrue(text.contains("\r\nContent-Length: 2\r\n"))
        XCTAssertTrue(text.hasSuffix("\r\n\r\n{}"))
    }
}
