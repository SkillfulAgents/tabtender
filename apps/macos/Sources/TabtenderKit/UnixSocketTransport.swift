import Foundation

/// Exchanges one serialized HTTP request for the raw bytes of its response.
protocol ManagementTransport: Sendable {
    /// Blocks until the peer closes the connection or `timeout` elapses.
    func roundTrip(_ request: Data, timeout: TimeInterval) throws -> Data
}

/// Blocking HTTP transport over a Unix domain stream socket. Each round trip
/// uses a fresh connection.
struct UnixSocketTransport: ManagementTransport {
    let path: String

    func roundTrip(_ request: Data, timeout: TimeInterval) throws -> Data {
        var address = try makeAddress()
        let descriptor = socket(AF_UNIX, SOCK_STREAM, 0)
        guard descriptor >= 0 else {
            throw ManagementError.transport(Self.describe(errno))
        }
        defer { close(descriptor) }

        var enabled: Int32 = 1
        setsockopt(descriptor, SOL_SOCKET, SO_NOSIGPIPE, &enabled, socklen_t(MemoryLayout<Int32>.size))

        let connected = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connected == 0 else {
            switch errno {
            case ENOENT, ECONNREFUSED, ENOTSOCK:
                throw ManagementError.notRunning
            default:
                throw ManagementError.transport(Self.describe(errno))
            }
        }

        let deadline = Date().addingTimeInterval(timeout)
        try send(request, to: descriptor, deadline: deadline)
        return try receiveAll(from: descriptor, deadline: deadline)
    }

    private func makeAddress() throws -> sockaddr_un {
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        guard bytes.count < capacity else {
            throw ManagementError.transport("socket path is longer than \(capacity - 1) bytes: \(path)")
        }
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        return address
    }

    private func send(_ data: Data, to descriptor: Int32, deadline: Date) throws {
        var offset = 0
        while offset < data.count {
            try applyTimeout(SO_SNDTIMEO, on: descriptor, deadline: deadline)
            let written = data.withUnsafeBytes { buffer in
                Darwin.send(descriptor, buffer.baseAddress! + offset, data.count - offset, 0)
            }
            if written < 0 {
                try handleFailure(errno)
                continue
            }
            offset += written
        }
    }

    private func receiveAll(from descriptor: Int32, deadline: Date) throws -> Data {
        var response = Data()
        var buffer = [UInt8](repeating: 0, count: 16 * 1024)
        while true {
            try applyTimeout(SO_RCVTIMEO, on: descriptor, deadline: deadline)
            let count = recv(descriptor, &buffer, buffer.count, 0)
            if count < 0 {
                try handleFailure(errno)
                continue
            }
            if count == 0 {
                return response
            }
            response.append(contentsOf: buffer[..<count])
        }
    }

    /// Bounds the next blocking call by the time left until `deadline`.
    private func applyTimeout(_ option: Int32, on descriptor: Int32, deadline: Date) throws {
        let remaining = deadline.timeIntervalSinceNow
        guard remaining > 0 else {
            throw ManagementError.timedOut
        }
        let seconds = remaining.rounded(.down)
        var value = timeval(
            tv_sec: Int(seconds),
            tv_usec: max(Int32((remaining - seconds) * 1_000_000), 1)
        )
        setsockopt(descriptor, SOL_SOCKET, option, &value, socklen_t(MemoryLayout<timeval>.size))
    }

    /// Returns normally when the interrupted call should simply be retried.
    private func handleFailure(_ code: Int32) throws {
        switch code {
        case EINTR:
            return
        case EAGAIN:
            throw ManagementError.timedOut
        default:
            throw ManagementError.transport(Self.describe(code))
        }
    }

    private static func describe(_ code: Int32) -> String {
        String(cString: strerror(code))
    }
}
