import Foundation
import AppKit
import CoreGraphics
import ScreenCaptureKit

enum ActionExecutionResult {
    case completed
    case screenshot(ScreenshotResult)

    var protocolPayload: [String: Any] {
        switch self {
        case .completed:
            return ["status": "ok"]
        case .screenshot(let screenshot):
            return [
                "content_type": "image/png",
                "data_base64": screenshot.png.base64EncodedString(),
                "height": screenshot.height,
                "width": screenshot.width,
            ]
        }
    }
}

struct ScreenshotResult {
    let png: Data
    let width: Int
    let height: Int
}

struct MacOSActionExecutor {
    func requireStartPermissions() throws {
        let status = try permissionStatus()
        let missing = status.missingPermissions
        guard missing.isEmpty else {
            throw permissionFailure(missing)
        }
    }

    func execute(_ action: ActionRequest) throws -> ActionExecutionResult {
        try requireStartPermissions()

        switch action.kind {
        case "screenshot":
            return .screenshot(try captureScreenshot())
        case "click":
            try click(try requiredPoint(x: action.x, y: action.y))
            return .completed
        case "double_click":
            try doubleClick(try requiredPoint(x: action.x, y: action.y))
            return .completed
        case "drag":
            guard let from = action.from, let to = action.to else {
                throw ProtocolFailure("invalid_action", "Drag requires source and destination coordinates.")
            }
            try drag(from: try validatedPoint(from), to: try validatedPoint(to))
            return .completed
        case "move":
            try move(try requiredPoint(x: action.x, y: action.y))
            return .completed
        case "type":
            guard let text = action.text else {
                throw ProtocolFailure("invalid_action", "Type requires text.")
            }
            try typeText(text)
            return .completed
        case "keypress":
            guard let keys = action.keys else {
                throw ProtocolFailure("invalid_action", "Keypress requires keys.")
            }
            try keypress(keys)
            return .completed
        case "scroll":
            guard let deltaX = action.deltaX, let deltaY = action.deltaY else {
                throw ProtocolFailure("invalid_action", "Scroll requires integer deltas.")
            }
            try scroll(at: try requiredPoint(x: action.x, y: action.y), deltaX: deltaX, deltaY: deltaY)
            return .completed
        case "wait":
            guard let milliseconds = action.milliseconds else {
                throw ProtocolFailure("invalid_action", "Wait requires milliseconds.")
            }
            try wait(milliseconds: milliseconds)
            return .completed
        default:
            throw ProtocolFailure("invalid_action", "Action is not allowed by this preview helper.")
        }
    }

    private func permissionStatus() throws -> PermissionStatus {
        guard #available(macOS 14.0, *) else {
            throw ProtocolFailure("unsupported_platform", "CCR macOS preview requires macOS 14 or later.")
        }
        return PermissionStatus(
            accessibility: CGPreflightPostEventAccess(),
            screenRecording: CGPreflightScreenCaptureAccess()
        )
    }

    private func permissionFailure(_ missing: [String]) -> ProtocolFailure {
        ProtocolFailure(
            "permission_required",
            "CCR macOS preview needs Accessibility and Screen Recording permission. Enable the missing permission for the process that launches ccr-cua-macos in System Settings > Privacy & Security, then restart the helper. No action was performed.",
            permissions: missing
        )
    }

    private func requiredPoint(x: Double?, y: Double?) throws -> CGPoint {
        guard let x = x, let y = y else {
            throw ProtocolFailure("invalid_action", "Pointer action requires numeric coordinates.")
        }
        return try validatedPoint(ActionPoint(x: x, y: y))
    }

    private func validatedPoint(_ point: ActionPoint) throws -> CGPoint {
        guard point.x.isFinite, point.y.isFinite else {
            throw ProtocolFailure("invalid_action", "Pointer coordinates must be finite numbers.")
        }

        let bounds = try virtualDesktopBounds()
        let screenshotPoint = CGPoint(x: point.x, y: point.y)
        let screenshotBounds = CGRect(origin: .zero, size: bounds.size)
        guard screenshotBounds.contains(screenshotPoint) else {
            throw ProtocolFailure("invalid_action", "Pointer coordinates are outside the active desktop.")
        }
        return CGPoint(
            x: bounds.origin.x + screenshotPoint.x,
            y: bounds.origin.y + screenshotPoint.y
        )
    }

    private func virtualDesktopBounds() throws -> CGRect {
        let displays = try activeDisplays()
        return try virtualDesktopBounds(for: displays)
    }

    private func activeDisplays() throws -> [CGDirectDisplayID] {
        var displayCount: UInt32 = 0
        guard CGGetActiveDisplayList(0, nil, &displayCount) == .success, displayCount > 0 else {
            throw ProtocolFailure("action_failed", "The active desktop could not be determined.")
        }

        var displays = [CGDirectDisplayID](repeating: 0, count: Int(displayCount))
        let listResult = displays.withUnsafeMutableBufferPointer { buffer in
            CGGetActiveDisplayList(displayCount, buffer.baseAddress, &displayCount)
        }
        guard listResult == .success else {
            throw ProtocolFailure("action_failed", "The active desktop could not be determined.")
        }
        return Array(displays.prefix(Int(displayCount)))
    }

    private func virtualDesktopBounds(for displays: [CGDirectDisplayID]) throws -> CGRect {
        var bounds = CGRect.null
        for display in displays {
            bounds = bounds.union(CGDisplayBounds(display))
        }
        guard !bounds.isNull else {
            throw ProtocolFailure("action_failed", "The active desktop could not be determined.")
        }
        return bounds
    }

    private func click(_ point: CGPoint) throws {
        try postMouseEvent(.leftMouseDown, at: point, clickCount: 1)
        try postMouseEvent(.leftMouseUp, at: point, clickCount: 1)
    }

    private func doubleClick(_ point: CGPoint) throws {
        try click(point)
        Thread.sleep(forTimeInterval: 0.05)
        try postMouseEvent(.leftMouseDown, at: point, clickCount: 2)
        try postMouseEvent(.leftMouseUp, at: point, clickCount: 2)
    }

    private func drag(from: CGPoint, to: CGPoint) throws {
        try postMouseEvent(.leftMouseDown, at: from, clickCount: 1)
        var needsRelease = true
        defer {
            if needsRelease {
                try? postMouseEvent(.leftMouseUp, at: to, clickCount: 1)
            }
        }

        for step in 1...8 {
            let progress = CGFloat(step) / 8
            let point = CGPoint(
                x: from.x + (to.x - from.x) * progress,
                y: from.y + (to.y - from.y) * progress
            )
            try postMouseEvent(.leftMouseDragged, at: point, clickCount: 1)
            Thread.sleep(forTimeInterval: 0.01)
        }
        try postMouseEvent(.leftMouseUp, at: to, clickCount: 1)
        needsRelease = false
    }

    private func move(_ point: CGPoint) throws {
        try postMouseEvent(.mouseMoved, at: point, clickCount: 0)
    }

    private func postMouseEvent(_ type: CGEventType, at point: CGPoint, clickCount: Int64) throws {
        guard let event = CGEvent(
            mouseEventSource: nil,
            mouseType: type,
            mouseCursorPosition: point,
            mouseButton: .left
        ) else {
            throw ProtocolFailure("action_failed", "The preview helper could not create a pointer event.")
        }
        event.setIntegerValueField(.mouseEventClickState, value: clickCount)
        event.post(tap: .cghidEventTap)
    }

    private func typeText(_ text: String) throws {
        let characters = Array(text.utf16)
        guard !characters.isEmpty, characters.count <= 4_096 else {
            throw ProtocolFailure("invalid_action", "Type text must contain 1 to 4096 UTF-16 code units.")
        }
        guard let keyDown = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: true),
              let keyUp = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: false)
        else {
            throw ProtocolFailure("action_failed", "The preview helper could not create a keyboard event.")
        }
        characters.withUnsafeBufferPointer { buffer in
            keyDown.keyboardSetUnicodeString(stringLength: characters.count, unicodeString: buffer.baseAddress)
            keyUp.keyboardSetUnicodeString(stringLength: characters.count, unicodeString: buffer.baseAddress)
        }
        keyDown.post(tap: .cghidEventTap)
        keyUp.post(tap: .cghidEventTap)
    }

    private func keypress(_ keys: [String]) throws {
        guard (1...5).contains(keys.count) else {
            throw ProtocolFailure("invalid_action", "Keypress requires one primary key and up to four modifiers.")
        }

        var flags: CGEventFlags = []
        var primaryKey: CGKeyCode?
        var usedModifiers = Set<String>()

        for key in keys {
            let name = key.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            if let modifier = Self.modifierFlags[name] {
                guard usedModifiers.insert(name).inserted else {
                    throw ProtocolFailure("invalid_action", "Keypress cannot repeat a modifier.")
                }
                flags.insert(modifier)
                continue
            }
            guard let keyCode = Self.keyCodes[name], primaryKey == nil else {
                throw ProtocolFailure("invalid_action", "Keypress uses an unsupported key combination.")
            }
            primaryKey = keyCode
        }

        guard let primaryKey = primaryKey else {
            throw ProtocolFailure("invalid_action", "Keypress requires one primary key.")
        }
        guard let keyDown = CGEvent(keyboardEventSource: nil, virtualKey: primaryKey, keyDown: true),
              let keyUp = CGEvent(keyboardEventSource: nil, virtualKey: primaryKey, keyDown: false)
        else {
            throw ProtocolFailure("action_failed", "The preview helper could not create a keyboard event.")
        }
        keyDown.flags = flags
        keyUp.flags = flags
        keyDown.post(tap: .cghidEventTap)
        keyUp.post(tap: .cghidEventTap)
    }

    private func scroll(at point: CGPoint, deltaX: Int, deltaY: Int) throws {
        guard (-10_000...10_000).contains(deltaX), (-10_000...10_000).contains(deltaY) else {
            throw ProtocolFailure("invalid_action", "Scroll deltas must be between -10000 and 10000.")
        }
        try move(point)
        guard let event = CGEvent(
            scrollWheelEvent2Source: nil,
            units: .pixel,
            wheelCount: 2,
            wheel1: Int32(deltaY),
            wheel2: Int32(deltaX),
            wheel3: 0
        ) else {
            throw ProtocolFailure("action_failed", "The preview helper could not create a scroll event.")
        }
        event.post(tap: .cghidEventTap)
    }

    private func wait(milliseconds: Int) throws {
        guard (0...60_000).contains(milliseconds) else {
            throw ProtocolFailure("invalid_action", "Wait must be between 0 and 60000 milliseconds.")
        }
        Thread.sleep(forTimeInterval: TimeInterval(milliseconds) / 1_000)
    }

    private func captureScreenshot() throws -> ScreenshotResult {
        guard #available(macOS 14.0, *) else {
            throw ProtocolFailure("unsupported_platform", "CCR macOS preview requires macOS 14 or later.")
        }
        let displays = try activeDisplays()
        let bounds = try virtualDesktopBounds(for: displays)
        let width = Int(bounds.width.rounded(.up))
        let height = Int(bounds.height.rounded(.up))
        guard width > 0, height > 0 else {
            throw ProtocolFailure("action_failed", "The preview helper could not capture the active desktop.")
        }
        let colorSpace = CGColorSpaceCreateDeviceRGB()
        guard let context = CGContext(
            data: nil,
            width: width,
            height: height,
            bitsPerComponent: 8,
            bytesPerRow: 0,
            space: colorSpace,
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue
        ) else {
            throw ProtocolFailure("action_failed", "The preview helper could not allocate screenshot storage.")
        }
        var capturedDisplay = false
        context.setFillColor(NSColor.black.cgColor)
        context.fill(CGRect(x: 0, y: 0, width: width, height: height))
        let content = try shareableScreenContent()
        for display in displays {
            let displayBounds = CGDisplayBounds(display)
            let image = try captureDisplayImage(display, displayBounds: displayBounds, content: content)
            let destination = CGRect(
                x: displayBounds.minX - bounds.minX,
                y: bounds.maxY - displayBounds.maxY,
                width: displayBounds.width,
                height: displayBounds.height
            )
            context.draw(image, in: destination)
            capturedDisplay = true
        }
        guard capturedDisplay, let image = context.makeImage() else {
            throw ProtocolFailure("action_failed", "The preview helper could not capture the active desktop.")
        }
        let bitmap = NSBitmapImageRep(cgImage: image)
        guard let png = bitmap.representation(using: .png, properties: [:]) else {
            throw ProtocolFailure("action_failed", "The preview helper could not encode a screenshot.")
        }
        return ScreenshotResult(png: png, width: width, height: height)
    }

    @available(macOS 14.0, *)
    private func shareableScreenContent() throws -> SCShareableContent {
        try screenCaptureResult { completion in
            SCShareableContent.getExcludingDesktopWindows(false, onScreenWindowsOnly: true) { content, error in
                if let error = error {
                    completion(.failure(error))
                    return
                }
                guard let content = content else {
                    completion(.failure(ProtocolFailure("action_failed", "The active desktop could not be determined.")))
                    return
                }
                completion(.success(content))
            }
        }
    }

    @available(macOS 14.0, *)
    private func captureDisplayImage(
        _ displayID: CGDirectDisplayID,
        displayBounds: CGRect,
        content: SCShareableContent
    ) throws -> CGImage {
        let width = Int(displayBounds.width.rounded(.up))
        let height = Int(displayBounds.height.rounded(.up))
        guard width > 0, height > 0 else {
            throw ProtocolFailure("action_failed", "The preview helper could not capture every active display.")
        }
        guard let screenCaptureDisplay = content.displays.first(where: { $0.displayID == displayID }) else {
            throw ProtocolFailure("action_failed", "The active desktop could not be determined.")
        }

        let filter = SCContentFilter(
            display: screenCaptureDisplay,
            excludingApplications: [],
            exceptingWindows: []
        )
        let configuration = SCStreamConfiguration()
        configuration.width = width
        configuration.height = height
        configuration.capturesAudio = false
        configuration.showsCursor = false
        return try screenCaptureResult { completion in
            SCScreenshotManager.captureImage(contentFilter: filter, configuration: configuration) { image, error in
                if let error = error {
                    completion(.failure(error))
                    return
                }
                guard let image = image else {
                    completion(.failure(ProtocolFailure("action_failed", "The preview helper could not capture every active display.")))
                    return
                }
                completion(.success(image))
            }
        }
    }

    @available(macOS 14.0, *)
    private func screenCaptureResult<T>(
        _ operation: (@escaping @Sendable (Result<T, Error>) -> Void) -> Void
    ) throws -> T {
        let box = LockedCaptureResult<T>()
        let semaphore = DispatchSemaphore(value: 0)
        operation { result in
            box.set(result)
            semaphore.signal()
        }
        guard semaphore.wait(timeout: .now() + 10) == .success else {
            throw ProtocolFailure("action_failed", "The preview helper timed out while capturing the active desktop.")
        }
        guard let result = box.get() else {
            throw ProtocolFailure("action_failed", "The preview helper could not capture the active desktop.")
        }
        return try result.get()
    }

    private static let modifierFlags: [String: CGEventFlags] = [
        "command": .maskCommand,
        "control": .maskControl,
        "option": .maskAlternate,
        "shift": .maskShift,
    ]

    private static let keyCodes: [String: CGKeyCode] = [
        "a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04, "g": 0x05,
        "z": 0x06, "x": 0x07, "c": 0x08, "v": 0x09, "b": 0x0B, "q": 0x0C,
        "w": 0x0D, "e": 0x0E, "r": 0x0F, "y": 0x10, "t": 0x11, "1": 0x12,
        "2": 0x13, "3": 0x14, "4": 0x15, "6": 0x16, "5": 0x17, "equals": 0x18,
        "9": 0x19, "7": 0x1A, "minus": 0x1B, "8": 0x1C, "0": 0x1D, "right_bracket": 0x1E,
        "o": 0x1F, "u": 0x20, "left_bracket": 0x21, "i": 0x22, "p": 0x23, "l": 0x25,
        "j": 0x26, "quote": 0x27, "k": 0x28, "semicolon": 0x29, "backslash": 0x2A,
        "comma": 0x2B, "slash": 0x2C, "n": 0x2D, "m": 0x2E, "period": 0x2F,
        "grave": 0x32, "return": 0x24, "tab": 0x30, "space": 0x31, "delete": 0x33,
        "escape": 0x35, "command": 0x37, "shift": 0x38, "option": 0x3A, "control": 0x3B,
        "f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76, "f5": 0x60, "f6": 0x61,
        "f7": 0x62, "f8": 0x64, "f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F,
        "home": 0x73, "page_up": 0x74, "forward_delete": 0x75, "end": 0x77,
        "page_down": 0x79, "left": 0x7B, "right": 0x7C, "down": 0x7D, "up": 0x7E,
    ]
}

private final class LockedCaptureResult<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var result: Result<T, Error>?

    func set(_ nextResult: Result<T, Error>) {
        lock.lock()
        result = nextResult
        lock.unlock()
    }

    func get() -> Result<T, Error>? {
        lock.lock()
        let current = result
        lock.unlock()
        return current
    }
}

private struct PermissionStatus {
    let accessibility: Bool
    let screenRecording: Bool

    var missingPermissions: [String] {
        var missing: [String] = []
        if !accessibility {
            missing.append("accessibility")
        }
        if !screenRecording {
            missing.append("screen_recording")
        }
        return missing
    }
}
