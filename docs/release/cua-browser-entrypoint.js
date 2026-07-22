#!/usr/bin/env node
'use strict';

const http = require('node:http');
const net = require('node:net');
const { spawn } = require('node:child_process');

const chromiumPath = '/usr/local/bin/ccr-chromium';
const maxDevToolsJSONBytes = 1 << 20;

function main() {
  const args = process.argv.slice(2);
  const externalDebugPort = readDebugPort(args);
  const internalDebugPort = externalDebugPort + 1;

  if (internalDebugPort > 65535) {
    fail(`remote debugging port ${externalDebugPort} cannot be proxied`);
  }

  replaceDebugPort(args, internalDebugPort);
  const chromium = spawn(chromiumPath, args, { stdio: 'inherit' });
  const proxy = http.createServer((req, res) => proxyDevToolsHTTP(req, res, internalDebugPort));
  proxy.on('upgrade', (req, socket, head) => proxyDevToolsUpgrade(req, socket, head, internalDebugPort));

  proxy.on('error', (error) => {
    fail(`starting DevTools proxy: ${error.message}`);
  });
  proxy.listen({ host: '0.0.0.0', port: externalDebugPort });

  chromium.on('error', (error) => {
    fail(`starting Chromium: ${error.message}`);
  });
  chromium.on('exit', (code, signal) => {
    proxy.close(() => process.exit(code === 0 ? 0 : 1));
    setTimeout(() => process.exit(code === 0 ? 0 : 1), 250).unref();
    if (signal) {
      process.exitCode = 1;
    }
  });

  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      if (!chromium.killed) {
        chromium.kill(signal);
      }
    });
  }
}

function proxyDevToolsHTTP(req, res, internalDebugPort) {
  const upstream = http.request({
    host: '127.0.0.1',
    port: internalDebugPort,
    method: req.method,
    path: req.url,
    headers: req.headers,
  }, (upstreamRes) => {
    if (!shouldRewriteDevToolsJSON(req.url)) {
      res.writeHead(upstreamRes.statusCode || 502, upstreamRes.statusMessage, upstreamRes.headers);
      upstreamRes.pipe(res);
      return;
    }
    const chunks = [];
    let size = 0;
    upstreamRes.on('data', (chunk) => {
      size += chunk.length;
      if (size > maxDevToolsJSONBytes) {
        upstream.destroy();
        writeProxyError(res, 'DevTools JSON response too large');
        return;
      }
      chunks.push(chunk);
    });
    upstreamRes.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8');
      let rewritten;
      try {
        rewritten = rewriteDevToolsJSON(body, req.headers.host);
      } catch (error) {
        writeProxyError(res, `DevTools JSON rewrite failed: ${error.message}`);
        return;
      }
      const raw = Buffer.from(rewritten, 'utf8');
      const headers = { ...upstreamRes.headers };
      delete headers['transfer-encoding'];
      headers['content-length'] = String(raw.length);
      res.writeHead(upstreamRes.statusCode || 200, upstreamRes.statusMessage, headers);
      res.end(raw);
    });
  });
  upstream.on('error', (error) => {
    writeProxyError(res, `DevTools upstream error: ${error.message}`);
  });
  req.pipe(upstream);
}

function proxyDevToolsUpgrade(req, socket, head, internalDebugPort) {
  const upstream = net.connect({ host: '127.0.0.1', port: internalDebugPort }, () => {
    upstream.write(`${req.method} ${req.url} HTTP/${req.httpVersion}\r\n`);
    for (const [name, value] of Object.entries(req.headers)) {
      writeHeader(upstream, name, value);
    }
    upstream.write('\r\n');
    if (head.length > 0) {
      upstream.write(head);
    }
    socket.pipe(upstream);
    upstream.pipe(socket);
  });
  upstream.on('error', () => socket.destroy());
  socket.on('error', () => upstream.destroy());
}

function shouldRewriteDevToolsJSON(path) {
  return typeof path === 'string' && path.startsWith('/json');
}

function rewriteDevToolsJSON(body, hostHeader) {
  const host = devToolsRequestHost(hostHeader);
  const payload = JSON.parse(body);
  rewriteWebSocketDebuggerURLs(payload, host);
  return JSON.stringify(payload);
}

function rewriteWebSocketDebuggerURLs(value, host) {
  if (Array.isArray(value)) {
    for (const item of value) {
      rewriteWebSocketDebuggerURLs(item, host);
    }
    return;
  }
  if (value === null || typeof value !== 'object') {
    return;
  }
  for (const [key, item] of Object.entries(value)) {
    if (key === 'webSocketDebuggerUrl' && typeof item === 'string') {
      value[key] = rewriteWebSocketDebuggerURL(item, host);
      continue;
    }
    rewriteWebSocketDebuggerURLs(item, host);
  }
}

function rewriteWebSocketDebuggerURL(raw, host) {
  const parsed = new URL(raw);
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error('DevTools websocket URL must use ws or wss');
  }
  parsed.protocol = 'ws:';
  parsed.host = host;
  parsed.username = '';
  parsed.password = '';
  return parsed.toString();
}

function devToolsRequestHost(hostHeader) {
  if (Array.isArray(hostHeader)) {
    throw new Error('request Host header must be single-valued');
  }
  const host = String(hostHeader || '').trim();
  if (host === '' || host.includes('/') || host.includes('@') || /[\u0000-\u0020\u007f]/.test(host)) {
    throw new Error('request Host header is invalid');
  }
  const parsed = new URL(`http://${host}`);
  if (parsed.username !== '' || parsed.password !== '' || parsed.hostname === '' || parsed.port === '') {
    throw new Error('request Host header must include host and port without credentials');
  }
  return parsed.host;
}

function writeHeader(stream, name, value) {
  if (Array.isArray(value)) {
    for (const item of value) {
      stream.write(`${name}: ${item}\r\n`);
    }
    return;
  }
  if (value !== undefined) {
    stream.write(`${name}: ${value}\r\n`);
  }
}

function writeProxyError(res, message) {
  if (res.headersSent) {
    res.destroy();
    return;
  }
  const body = Buffer.from(message, 'utf8');
  res.writeHead(502, {
    'content-type': 'text/plain; charset=utf-8',
    'content-length': String(body.length),
  });
  res.end(body);
}

function readDebugPort(values) {
  for (let index = 0; index < values.length; index++) {
    const value = values[index];
    if (value.startsWith('--remote-debugging-port=')) {
      return validatePort(value.slice('--remote-debugging-port='.length));
    }
    if (value === '--remote-debugging-port' && index+1 < values.length) {
      return validatePort(values[index+1]);
    }
  }
  fail('missing --remote-debugging-port');
}

function replaceDebugPort(values, port) {
  for (let index = 0; index < values.length; index++) {
    if (values[index].startsWith('--remote-debugging-port=')) {
      values[index] = `--remote-debugging-port=${port}`;
      return;
    }
    if (values[index] === '--remote-debugging-port') {
      values[index + 1] = String(port);
      return;
    }
  }
  fail('missing --remote-debugging-port');
}

function validatePort(raw) {
  if (!/^[0-9]+$/.test(raw)) {
    fail(`invalid remote debugging port ${JSON.stringify(raw)}`);
  }
  const port = Number(raw);
  if (port < 1 || port > 65534) {
    fail(`invalid remote debugging port ${JSON.stringify(raw)}`);
  }
  return port;
}

function fail(message) {
  process.stderr.write(`ccr browser entrypoint: ${message}\n`);
  process.exit(64);
}

if (require.main === module) {
  main();
}

module.exports = {
  rewriteDevToolsJSON,
};
