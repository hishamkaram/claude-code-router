import Foundation

private enum SessionState: Equatable {
    case ready
    case active
}

private let sessionToken = makeSessionToken()
private let executor = MacOSActionExecutor()

writeReady(token: sessionToken)

private var state = SessionState.ready
private var shouldExit = false

while !shouldExit, let line = readLine(strippingNewline: true) {
    let data = Data(line.utf8)
    let fallbackID = safeRequestID(from: data)

    do {
        let message = try decodeMessage(data)
        guard constantTimeTokenMatches(message.token, expected: sessionToken) else {
            throw ProtocolFailure("unauthenticated", "Session token was not accepted.")
        }

        switch message.type {
        case "start":
            guard state == .ready else {
                throw ProtocolFailure("invalid_state", "Preview helper has already started.")
            }
            try executor.requireStartPermissions()
            state = .active
            writeStarted(id: message.id)
        case "action":
            guard state == .active else {
                throw ProtocolFailure("not_started", "Send a successful start message before actions.")
            }
            guard let action = message.action else {
                throw ProtocolFailure("invalid_request", "Action message is missing its action object.")
            }
            let result = try executor.execute(action)
            writeActionResult(id: message.id, action: action.kind, result: result)
        case "close":
            writeClosed(id: message.id)
            shouldExit = true
        default:
            throw ProtocolFailure("invalid_request", "Message type is not supported by this preview helper.")
        }
    } catch let failure as ProtocolFailure {
        writeFailure(id: fallbackID, failure)
    } catch {
        writeFailure(
            id: fallbackID,
            ProtocolFailure("action_failed", "The preview helper could not process the request.")
        )
    }
}

private func makeSessionToken() -> String {
    var generator = SystemRandomNumberGenerator()
    return (0..<4).map { _ in
        let random: UInt64 = generator.next()
        let part = String(random, radix: 16)
        return String(repeating: "0", count: 16 - part.count) + part
    }.joined()
}
