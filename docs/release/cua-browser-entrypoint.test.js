'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { rewriteDevToolsJSON } = require('./cua-browser-entrypoint.js');

test('rewrites DevTools version websocket URL to the request host port', () => {
  const rewritten = JSON.parse(rewriteDevToolsJSON(JSON.stringify({
    Browser: 'Chrome',
    webSocketDebuggerUrl: 'ws://127.0.0.1:9223/devtools/browser/session',
  }), '127.0.0.1:41238'));

  assert.equal(rewritten.webSocketDebuggerUrl, 'ws://127.0.0.1:41238/devtools/browser/session');
});

test('rewrites nested DevTools target websocket URLs to the request host port', () => {
  const rewritten = JSON.parse(rewriteDevToolsJSON(JSON.stringify([
    {
      id: 'page-1',
      webSocketDebuggerUrl: 'ws://localhost:9223/devtools/page/page-1',
    },
    {
      id: 'page-2',
      nested: {
        webSocketDebuggerUrl: 'wss://127.0.0.1:9223/devtools/page/page-2',
      },
    },
  ]), '[::1]:49222'));

  assert.equal(rewritten[0].webSocketDebuggerUrl, 'ws://[::1]:49222/devtools/page/page-1');
  assert.equal(rewritten[1].nested.webSocketDebuggerUrl, 'ws://[::1]:49222/devtools/page/page-2');
});

test('rejects Host headers without a published port', () => {
  assert.throws(
    () => rewriteDevToolsJSON('{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/browser/session"}', '127.0.0.1'),
    /host and port/,
  );
});
