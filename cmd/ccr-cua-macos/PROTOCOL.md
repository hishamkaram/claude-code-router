# CCR macOS Preview Protocol v1

The helper uses exactly one UTF-8 JSON object per line on standard input and
standard output. It does not write diagnostics to standard error. Protocol
output is private IPC between the helper and its future CCR wrapper; it must not
be copied to logs because a `result` may contain screenshot bytes.

## Lifecycle

1. On process launch, the helper writes one `ready` response with a fresh,
   in-memory session token.
2. The wrapper sends `start` with that token. The helper checks Accessibility
   event-posting and Screen Recording availability before replying `started`.
3. The wrapper sends zero or more token-authenticated `action` messages.
4. The wrapper sends `close` with the token. The helper replies `closed` and
   exits. `close` is also accepted after a failed `start` so the wrapper can
   clean up deterministically.

The protocol has no command, shell, environment, credential, provider, file,
network, clipboard, AppleScript, or arbitrary-process message. Unknown fields
and unknown action kinds are rejected.

Both permissions are rechecked before every action. A revoked permission returns
the same explicit preview-only rejection instead of allowing a partial session.

## Messages

Every request has this envelope:

```json
{"version":1,"id":"req-17","type":"action","token":"<ready token>","action":{}}
```

`version` is always `1`. `id` is an ASCII correlation id of 1 to 128 letters,
digits, dots, underscores, or hyphens. `token` is the exact token from `ready`.
The helper never emits the token again after `ready` and does not place it in an
error message.

### `ready` response

```json
{"version":1,"type":"ready","preview":true,"token":"<ephemeral token>","actions":["screenshot","click","double_click","drag","move","type","keypress","scroll","wait"]}
```

### `start` request and response

```json
{"version":1,"id":"start-1","type":"start","token":"<ready token>"}
{"version":1,"id":"start-1","type":"started","preview":true,"permissions":{"accessibility":true,"screen_recording":true}}
```

### `action` request and response

```json
{"version":1,"id":"shot-1","type":"action","token":"<ready token>","action":{"kind":"screenshot"}}
{"version":1,"id":"shot-1","type":"result","preview":true,"action":"screenshot","result":{"content_type":"image/png","data_base64":"<base64 PNG>","width":1440,"height":900}}
```

Non-screenshot action results are:

```json
{"version":1,"id":"click-1","type":"result","preview":true,"action":"click","result":{"status":"ok"}}
```

### `close` request and response

```json
{"version":1,"id":"close-1","type":"close","token":"<ready token>"}
{"version":1,"id":"close-1","type":"closed","preview":true}
```

## Action Shapes

Action objects allow only the listed fields. Coordinates are relative to the
active-display ScreenCaptureKit screenshot composite returned to the model. The
helper validates them against the active virtual desktop size, then translates
them to CoreGraphics desktop coordinates before posting events. This keeps
scaled and multi-display layouts aligned even when the virtual desktop origin is
not `(0,0)`.

| Action | Exact action object |
| --- | --- |
| Screenshot | `{"kind":"screenshot"}` |
| Click | `{"kind":"click","x":640,"y":480}` |
| Double click | `{"kind":"double_click","x":640,"y":480}` |
| Drag | `{"kind":"drag","from":{"x":100,"y":100},"to":{"x":500,"y":400}}` |
| Move | `{"kind":"move","x":640,"y":480}` |
| Type | `{"kind":"type","text":"hello"}` |
| Keypress | `{"kind":"keypress","keys":["command","a"]}` |
| Scroll | `{"kind":"scroll","x":640,"y":480,"delta_x":0,"delta_y":-400}` |
| Wait | `{"kind":"wait","milliseconds":250}` |

`type` accepts 1 to 4096 UTF-16 code units. `keypress` accepts one primary key
and up to four unique modifiers. Modifiers are `command`, `control`, `option`,
and `shift`; primary keys are letters, digits, punctuation names documented in
the helper source, navigation keys, and `f1` through `f12`. `scroll` moves to
the requested coordinate before posting its integer deltas, which are bounded
from -10000 to 10000. `wait` is bounded to 0 through 60000 milliseconds.

## Errors

Errors never echo request tokens, screenshots, typed text, or full action
payloads:

```json
{"version":1,"id":"start-1","type":"error","preview":true,"error":{"code":"permission_required","message":"CCR macOS preview needs Accessibility and Screen Recording permission. Enable the missing permission for the process that launches ccr-cua-macos in System Settings > Privacy & Security, then restart the helper. No action was performed.","permissions":["screen_recording"],"preview_only":true}}
```

Expected codes are `invalid_json`, `invalid_request`, `unauthenticated`,
`not_started`, `invalid_state`, `permission_required`, `unsupported_platform`,
`invalid_action`, and `action_failed`. A permission error is an explicit
preview-only rejection, never a fallback to another executor or provider.
