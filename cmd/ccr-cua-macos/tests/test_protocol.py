"""Portable protocol and source-policy tests for the macOS preview helper."""

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
FIXTURES = ROOT / "tests" / "fixtures"
TOKEN = "test-session-token"
ALLOWED_ACTIONS = {
    "screenshot",
    "click",
    "double_click",
    "drag",
    "move",
    "type",
    "keypress",
    "scroll",
    "wait",
}
REQUEST_FIELDS = {"version", "id", "type", "token", "action"}
ID_PATTERN = re.compile(r"^[A-Za-z0-9._-]{1,128}$")


class ProtocolValidationError(Exception):
    def __init__(self, code: str) -> None:
        self.code = code
        super().__init__(code)


def validate_request(request: Any, state: str) -> str:
    if not isinstance(request, dict):
        raise ProtocolValidationError("invalid_request")
    if set(request) - REQUEST_FIELDS:
        raise ProtocolValidationError("invalid_request")
    if request.get("version") != 1 or not isinstance(request.get("id"), str):
        raise ProtocolValidationError("invalid_request")
    if not ID_PATTERN.fullmatch(request["id"]):
        raise ProtocolValidationError("invalid_request")
    if request.get("token") != TOKEN:
        raise ProtocolValidationError("unauthenticated")

    message_type = request.get("type")
    if message_type in {"start", "close"}:
        if "action" in request:
            raise ProtocolValidationError("invalid_request")
        if message_type == "start":
            if state != "ready":
                raise ProtocolValidationError("invalid_state")
            return "active"
        return "closed"
    if message_type != "action":
        raise ProtocolValidationError("invalid_request")
    if state != "active":
        raise ProtocolValidationError("not_started")
    validate_action(request.get("action"))
    return state


def validate_action(action: Any) -> None:
    if not isinstance(action, dict) or not isinstance(action.get("kind"), str):
        raise ProtocolValidationError("invalid_request")

    kind = action["kind"]
    expected_fields = {
        "screenshot": {"kind"},
        "click": {"kind", "x", "y"},
        "double_click": {"kind", "x", "y"},
        "drag": {"kind", "from", "to"},
        "move": {"kind", "x", "y"},
        "type": {"kind", "text"},
        "keypress": {"kind", "keys"},
        "scroll": {"kind", "x", "y", "delta_x", "delta_y"},
        "wait": {"kind", "milliseconds"},
    }
    if kind not in expected_fields or set(action) != expected_fields[kind]:
        raise ProtocolValidationError("invalid_action")

    if kind in {"click", "double_click", "move"}:
        if not all(type(action[name]) in {int, float} for name in ("x", "y")):
            raise ProtocolValidationError("invalid_request")
    elif kind == "drag":
        for name in ("from", "to"):
            if not isinstance(action[name], dict):
                raise ProtocolValidationError("invalid_request")
            if set(action[name]) != {"x", "y"}:
                raise ProtocolValidationError("invalid_action")
            if not all(type(action[name][axis]) in {int, float} for axis in ("x", "y")):
                raise ProtocolValidationError("invalid_request")
    elif kind == "type" and not isinstance(action["text"], str):
        raise ProtocolValidationError("invalid_request")
    elif kind == "keypress" and not (
        isinstance(action["keys"], list) and all(isinstance(key, str) for key in action["keys"])
    ):
        raise ProtocolValidationError("invalid_request")
    elif kind == "scroll":
        if not all(type(action[name]) in {int, float} for name in ("x", "y")):
            raise ProtocolValidationError("invalid_request")
        if not all(type(action[name]) is int for name in ("delta_x", "delta_y")):
            raise ProtocolValidationError("invalid_request")
    elif kind == "wait" and type(action["milliseconds"]) is not int:
        raise ProtocolValidationError("invalid_request")


class PreviewProtocolTest(unittest.TestCase):
    def test_valid_session_fixture_follows_lifecycle(self) -> None:
        state = "ready"
        for line in (FIXTURES / "valid-session.jsonl").read_text(encoding="utf-8").splitlines():
            state = validate_request(json.loads(line), state)
        self.assertEqual(state, "closed")

    def test_invalid_fixture_has_stable_rejections(self) -> None:
        for line in (FIXTURES / "invalid-requests.jsonl").read_text(encoding="utf-8").splitlines():
            fixture = json.loads(line)
            with self.subTest(fixture=fixture["name"]):
                with self.assertRaises(ProtocolValidationError) as raised:
                    validate_request(fixture["request"], fixture.get("state", "ready"))
                self.assertEqual(raised.exception.code, fixture["expected_error"])

    def test_source_keeps_protocol_action_allowlist(self) -> None:
        source = (ROOT / "Sources" / "Protocol.swift").read_text(encoding="utf-8")
        declared_actions = set(re.findall(r'^    "([a-z_]+)",$', source, flags=re.MULTILINE))
        self.assertEqual(declared_actions, ALLOWED_ACTIONS)

    def test_source_has_no_logging_environment_or_process_api(self) -> None:
        source = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "Sources").glob("*.swift"))
        )
        forbidden = (
            "ProcessInfo.processInfo.environment",
            "getenv(",
            "setenv(",
            "Process(",
            "NSTask",
            "popen(",
            "system(",
            "NSWorkspace.shared.open",
            "print(",
            "NSLog",
            "os_log",
            "Logger(",
            "FileHandle.standardError",
        )
        for api in forbidden:
            with self.subTest(api=api):
                self.assertNotIn(api, source)

    def test_source_uses_only_allowed_framework_imports_and_permission_gates(self) -> None:
        imports = set()
        source = ""
        for path in sorted((ROOT / "Sources").glob("*.swift")):
            content = path.read_text(encoding="utf-8")
            source += content
            imports.update(re.findall(r"^import ([A-Za-z0-9_]+)$", content, flags=re.MULTILINE))
        self.assertEqual(imports, {"Foundation", "AppKit", "CoreGraphics", "ScreenCaptureKit"})
        self.assertIn("CGPreflightPostEventAccess()", source)
        self.assertIn("CGPreflightScreenCaptureAccess()", source)

    def test_source_keeps_top_level_state_private_for_swift_access_control(self) -> None:
        source = (ROOT / "Sources" / "main.swift").read_text(encoding="utf-8")
        self.assertIn("private var state = SessionState.ready", source)
        self.assertIn("private var shouldExit = false", source)

    def test_source_translates_screenshot_points_to_virtual_desktop_origin(self) -> None:
        source = (ROOT / "Sources" / "MacOSDriver.swift").read_text(encoding="utf-8")
        self.assertIn("CGRect(origin: .zero, size: bounds.size)", source)
        self.assertIn("bounds.origin.x + screenshotPoint.x", source)
        self.assertIn("bounds.origin.y + screenshotPoint.y", source)

    def test_source_uses_display_bounds_screenshots_for_coordinate_stability(self) -> None:
        source = (ROOT / "Sources" / "MacOSDriver.swift").read_text(encoding="utf-8")
        makefile = (ROOT.parents[1] / "Makefile").read_text(encoding="utf-8")
        self.assertIn("-framework ScreenCaptureKit", makefile)
        self.assertIn("SCScreenshotManager.captureImage", source)
        self.assertIn("SCContentFilter(", source)
        self.assertIn("virtualDesktopBounds(for: displays)", source)
        self.assertIn("bounds.maxY - displayBounds.maxY", source)
        self.assertIn("could not capture every active display", source)
        self.assertIn("timed out while capturing", source)
        self.assertNotIn("CGWindowListCreateImage", source)
        self.assertNotIn("CGDisplayCreateImage", source)


if __name__ == "__main__":
    unittest.main()
