import Foundation

let protocolVersion = 1
let maximumRequestBytes = 1_048_576
let allowedActionKinds = [
    "screenshot",
    "click",
    "double_click",
    "drag",
    "move",
    "type",
    "keypress",
    "scroll",
    "wait",
]

struct ProtocolFailure: Error {
    let code: String
    let message: String
    let permissions: [String]?

    init(_ code: String, _ message: String, permissions: [String]? = nil) {
        self.code = code
        self.message = message
        self.permissions = permissions
    }
}

struct IncomingMessage: Decodable {
    let version: Int
    let id: String
    let type: String
    let token: String
    let action: ActionRequest?
}

struct ActionRequest: Decodable {
    let kind: String
    let x: Double?
    let y: Double?
    let from: ActionPoint?
    let to: ActionPoint?
    let text: String?
    let keys: [String]?
    let deltaX: Int?
    let deltaY: Int?
    let milliseconds: Int?

    enum CodingKeys: String, CodingKey {
        case kind
        case x
        case y
        case from
        case to
        case text
        case keys
        case deltaX = "delta_x"
        case deltaY = "delta_y"
        case milliseconds
    }
}

struct ActionPoint: Decodable {
    let x: Double
    let y: Double
}

func decodeMessage(_ data: Data) throws -> IncomingMessage {
    guard data.count <= maximumRequestBytes else {
        throw ProtocolFailure("invalid_request", "Request exceeds the preview protocol size limit.")
    }

    let object: Any
    do {
        object = try JSONSerialization.jsonObject(with: data)
    } catch {
        throw ProtocolFailure("invalid_json", "Request must be a single JSON object.")
    }

    guard let dictionary = object as? [String: Any] else {
        throw ProtocolFailure("invalid_request", "Request must be a JSON object.")
    }

    let allowedMessageFields: Set<String> = ["version", "id", "type", "token", "action"]
    guard Set(dictionary.keys).isSubset(of: allowedMessageFields) else {
        throw ProtocolFailure("invalid_request", "Request contains unsupported fields.")
    }

    let message: IncomingMessage
    do {
        message = try JSONDecoder().decode(IncomingMessage.self, from: data)
    } catch {
        throw ProtocolFailure("invalid_request", "Request is missing or has invalid required fields.")
    }

    guard message.version == protocolVersion else {
        throw ProtocolFailure("invalid_request", "Unsupported preview protocol version.")
    }
    guard isValidRequestID(message.id) else {
        throw ProtocolFailure("invalid_request", "Request id must use 1 to 128 ASCII letters, digits, dots, underscores, or hyphens.")
    }
    guard message.token.utf8.count <= 128 else {
        throw ProtocolFailure("unauthenticated", "Session token was not accepted.")
    }

    switch message.type {
    case "start", "close":
        guard message.action == nil, dictionary["action"] == nil else {
            throw ProtocolFailure("invalid_request", "Control messages cannot include an action.")
        }
    case "action":
        guard let action = message.action else {
            throw ProtocolFailure("invalid_request", "Action message is missing its action object.")
        }
        guard let rawAction = dictionary["action"] as? [String: Any] else {
            throw ProtocolFailure("invalid_request", "Action must be a JSON object.")
        }
        try validateActionShape(action, rawAction: rawAction)
    default:
        throw ProtocolFailure("invalid_request", "Message type is not supported by this preview helper.")
    }

    return message
}

func safeRequestID(from data: Data) -> String? {
    guard let object = try? JSONSerialization.jsonObject(with: data),
          let dictionary = object as? [String: Any],
          let id = dictionary["id"] as? String,
          isValidRequestID(id)
    else {
        return nil
    }
    return id
}

func isValidRequestID(_ id: String) -> Bool {
    let allowed = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
    return !id.isEmpty && id.utf8.count <= 128 && id.unicodeScalars.allSatisfy(allowed.contains)
}

func constantTimeTokenMatches(_ candidate: String, expected: String) -> Bool {
    let candidateBytes = Array(candidate.utf8)
    let expectedBytes = Array(expected.utf8)
    guard candidateBytes.count <= 128 else {
        return false
    }

    var difference = candidateBytes.count ^ expectedBytes.count
    for index in 0..<128 {
        let candidateByte = index < candidateBytes.count ? candidateBytes[index] : 0
        let expectedByte = index < expectedBytes.count ? expectedBytes[index] : 0
        difference |= Int(candidateByte ^ expectedByte)
    }
    return difference == 0
}

func writeProtocolMessage(_ object: [String: Any]) {
    guard JSONSerialization.isValidJSONObject(object),
          let data = try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    else {
        return
    }

    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([0x0A]))
}

func writeReady(token: String) {
    writeProtocolMessage([
        "actions": allowedActionKinds,
        "preview": true,
        "token": token,
        "type": "ready",
        "version": protocolVersion,
    ])
}

func writeStarted(id: String) {
    writeProtocolMessage([
        "id": id,
        "permissions": [
            "accessibility": true,
            "screen_recording": true,
        ],
        "preview": true,
        "type": "started",
        "version": protocolVersion,
    ])
}

func writeActionResult(id: String, action: String, result: ActionExecutionResult) {
    var output: [String: Any] = [
        "action": action,
        "id": id,
        "preview": true,
        "type": "result",
        "version": protocolVersion,
    ]
    output["result"] = result.protocolPayload
    writeProtocolMessage(output)
}

func writeClosed(id: String) {
    writeProtocolMessage([
        "id": id,
        "preview": true,
        "type": "closed",
        "version": protocolVersion,
    ])
}

func writeFailure(id: String?, _ failure: ProtocolFailure) {
    var error: [String: Any] = [
        "code": failure.code,
        "message": failure.message,
        "preview_only": true,
    ]
    if let permissions = failure.permissions {
        error["permissions"] = permissions
    }

    var output: [String: Any] = [
        "error": error,
        "preview": true,
        "type": "error",
        "version": protocolVersion,
    ]
    if let id = id {
        output["id"] = id
    }
    writeProtocolMessage(output)
}

private func validateActionShape(_ action: ActionRequest, rawAction: [String: Any]) throws {
    let actionFields = Set(rawAction.keys)

    switch action.kind {
    case "screenshot":
        try requireExactActionFields(actionFields, ["kind"])
    case "click", "double_click", "move":
        try requireExactActionFields(actionFields, ["kind", "x", "y"])
        guard action.x != nil, action.y != nil else {
            throw ProtocolFailure("invalid_action", "Pointer action requires numeric coordinates.")
        }
	case "drag":
		try requireExactActionFields(actionFields, ["kind", "from", "to"])
		guard action.from != nil, action.to != nil else {
			throw ProtocolFailure("invalid_action", "Drag requires source and destination coordinates.")
		}
		try requireExactPointFields(rawAction["from"], label: "source")
		try requireExactPointFields(rawAction["to"], label: "destination")
    case "type":
        try requireExactActionFields(actionFields, ["kind", "text"])
        guard action.text != nil else {
            throw ProtocolFailure("invalid_action", "Type requires text.")
        }
    case "keypress":
        try requireExactActionFields(actionFields, ["kind", "keys"])
        guard action.keys != nil else {
            throw ProtocolFailure("invalid_action", "Keypress requires keys.")
        }
    case "scroll":
        try requireExactActionFields(actionFields, ["kind", "x", "y", "delta_x", "delta_y"])
        guard action.x != nil, action.y != nil, action.deltaX != nil, action.deltaY != nil else {
            throw ProtocolFailure("invalid_action", "Scroll requires numeric coordinates and integer deltas.")
        }
    case "wait":
        try requireExactActionFields(actionFields, ["kind", "milliseconds"])
        guard action.milliseconds != nil else {
            throw ProtocolFailure("invalid_action", "Wait requires milliseconds.")
        }
    default:
        throw ProtocolFailure("invalid_action", "Action is not allowed by this preview helper.")
    }
}

private func requireExactActionFields(_ actual: Set<String>, _ expected: Set<String>) throws {
	guard actual == expected else {
		throw ProtocolFailure("invalid_action", "Action contains unsupported or missing fields.")
	}
}

private func requireExactPointFields(_ rawPoint: Any?, label: String) throws {
	guard let point = rawPoint as? [String: Any] else {
		throw ProtocolFailure("invalid_request", "Drag \(label) point must be a JSON object.")
	}
	guard Set(point.keys) == ["x", "y"] else {
		throw ProtocolFailure("invalid_action", "Drag \(label) point contains unsupported or missing fields.")
	}
}
