'use strict';

// Xray VLESS + WS front door.
//
// Rebuilt from the two node templates formerly deployed by hand
// (InstaCloud-github: nginx -> xray:8081; pella-node: node -> xray:8081).
// Differences from the originals:
//   - Xray is baked into the image (pinned) instead of downloaded at runtime;
//   - the UUID lives on the data volume, so a container rebuild keeps links;
//   - the Xray child is supervised and restarted;
//   - /healthz reports both the front door and the Xray child;
//   - no shell string is built from environment values.

const http = require('http');
const net = require('net');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { spawn } = require('child_process');

const PORT = Number(process.env.PORT || 8080);
const XRAY_PORT = Number(process.env.XRAY_PORT || 8081);
const XRAY_BIN = process.env.XRAY_BIN || '/usr/local/bin/xray';
const WS_PATH = normalizePath(process.env.WS_PATH || '/proxy');
const STATE_DIR = process.env.STATE_DIR || '/data';
const PUBLIC_PORT = String(process.env.PUBLIC_PORT || 443);
const PUBLIC_TLS = String(process.env.PUBLIC_TLS || '1') !== '0';
const SOCKS_ENABLED = String(process.env.SOCKS_ENABLED || '1') !== '0';
const SOCKS_PORT = Number(process.env.SOCKS_PORT || 11080);
const SOCKS_LISTEN = process.env.SOCKS_LISTEN || '0.0.0.0';
const SOCKS_USER = process.env.SOCKS_USER || 'o2a';
const SOCKS_PASS = process.env.SOCKS_PASS || '';
const SUB_TOKEN = (process.env.SUB_TOKEN || '').trim();
const UUID_FILE = path.join(STATE_DIR, 'uuid.txt');
const CONFIG_FILE = path.join(STATE_DIR, 'xray-config.json');
const INDEX_FILE = path.join(__dirname, 'index.html');

function normalizePath(value) {
  let p = String(value || '').trim();
  if (!p.startsWith('/')) p = '/' + p;
  return p.replace(/\/+$/, '') || '/';
}

function loadUuid() {
  const fromEnv = (process.env.UUID || '').trim();
  if (fromEnv) return fromEnv;
  fs.mkdirSync(STATE_DIR, { recursive: true });
  try {
    const saved = fs.readFileSync(UUID_FILE, 'utf8').trim();
    if (saved) return saved;
  } catch (_) {}
  const fresh = crypto.randomUUID();
  fs.writeFileSync(UUID_FILE, fresh + '\n', { mode: 0o600 });
  return fresh;
}

const UUID = loadUuid();
if (!SUB_TOKEN) console.warn('SUB_TOKEN unset: /sub and /api/info are public');

function writeXrayConfig() {
  const inbounds = [{
    tag: 'vless-ws',
    listen: '127.0.0.1',
    port: XRAY_PORT,
    protocol: 'vless',
    settings: {
      clients: [{ id: UUID, flow: '' }],
      decryption: 'none'
    },
    streamSettings: {
      network: 'ws',
      wsSettings: { path: WS_PATH }
    },
    sniffing: { enabled: true, destOverride: ['http', 'tls'] }
  }];
  if (SOCKS_ENABLED) {
    const socks = {
      tag: 'socks',
      listen: SOCKS_LISTEN,
      port: SOCKS_PORT,
      protocol: 'socks',
      settings: { auth: SOCKS_PASS ? 'password' : 'noauth', udp: true },
      sniffing: { enabled: true, destOverride: ['http', 'tls'] }
    };
    if (SOCKS_PASS) socks.settings.accounts = [{ user: SOCKS_USER, pass: SOCKS_PASS }];
    inbounds.push(socks);
  }
  const config = {
    log: { loglevel: process.env.XRAY_LOG_LEVEL || 'warning' },
    inbounds,
    outbounds: [
      { tag: 'direct', protocol: 'freedom' },
      { tag: 'block', protocol: 'blackhole' }
    ]
  };
  fs.mkdirSync(STATE_DIR, { recursive: true });
  fs.writeFileSync(CONFIG_FILE, JSON.stringify(config, null, 2) + '\n');
}

let xray = null;
let xrayReady = false;
let xrayRestarts = 0;
let restartTimer = null;

function scheduleRestart(delayMs) {
  if (restartTimer) return;
  restartTimer = setTimeout(() => {
    restartTimer = null;
    startXray();
  }, delayMs);
  restartTimer.unref();
}

function startXray() {
  if (xray) return;
  writeXrayConfig();
  xrayReady = false;
  const child = spawn(XRAY_BIN, ['-config', CONFIG_FILE], {
    stdio: ['ignore', 'inherit', 'inherit']
  });
  xray = child;
  const readyTimer = setTimeout(() => {
    if (xray === child && child.exitCode === null) {
      xrayReady = true;
      console.log('[xray] ready');
    }
  }, 1000);
  readyTimer.unref();

  child.on('exit', (code, signal) => {
    clearTimeout(readyTimer);
    xrayReady = false;
    if (xray === child) xray = null;
    xrayRestarts += 1;
    console.error(`[xray] exited code=${code} signal=${signal}, restarting in 2s`);
    scheduleRestart(2000);
  });
  child.on('error', (err) => {
    clearTimeout(readyTimer);
    xrayReady = false;
    if (xray === child) xray = null;
    xrayRestarts += 1;
    console.error(`[xray] spawn failed: ${err.message}, retrying in 5s`);
    scheduleRestart(5000);
  });
}

function xrayHealthy() {
  return xrayReady && Boolean(xray) && xray.exitCode === null;
}

function publicHost(req) {
  const raw = req.headers['x-forwarded-host'] || req.headers.host || 'localhost';
  return String(raw).split(',')[0].trim().split(':')[0];
}

function linkFor(host) {
  const params = new URLSearchParams({
    encryption: 'none',
    security: PUBLIC_TLS ? 'tls' : 'none',
    type: 'ws',
    host,
    path: WS_PATH
  });
  return `vless://${UUID}@${host}:${PUBLIC_PORT}?${params.toString()}#Node-${host.split('.')[0]}`;
}

function clashFor(host) {
  const name = `Node-${host.split('.')[0]}`;
  return [
    'proxies:',
    `  - name: "${name}"`,
    '    type: vless',
    `    server: ${host}`,
    `    port: ${PUBLIC_PORT}`,
    `    uuid: ${UUID}`,
    '    network: ws',
    `    tls: ${PUBLIC_TLS ? 'true' : 'false'}`,
    `    sni: ${host}`,
    '    skip-cert-verify: true',
    '    ws-opts:',
    `      path: "${WS_PATH}"`,
    '      headers:',
    `        Host: ${host}`,
    '',
    'proxy-groups:',
    '  - name: "PROXY"',
    '    type: select',
    '    proxies:',
    `      - "${name}"`,
    '',
    'rules:',
    '  - MATCH,PROXY',
    ''
  ].join('\n');
}

const SUB_UA = /(v2ray|clash|mihomo|sing-box|shadowrocket|stash|quantumult|sagernet|hiddify|nekobox|passwall|surfboard|loon|surge|verge)/i;

function tokenOK(url) {
  if (!SUB_TOKEN) return true;
  const got = Buffer.from(String(url.searchParams.get('token') || ''));
  const want = Buffer.from(SUB_TOKEN);
  if (got.length !== want.length) return false;
  try { return crypto.timingSafeEqual(got, want); } catch (_) { return false; }
}

function send(res, status, type, body) {
  res.writeHead(status, {
    'Content-Type': type,
    'Cache-Control': 'no-store',
    'Access-Control-Allow-Origin': '*'
  });
  res.end(body);
}

const server = http.createServer((req, res) => {
  const host = publicHost(req);
  let url;
  try {
    url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  } catch (_) {
    return send(res, 400, 'text/plain; charset=utf-8', 'bad request');
  }
  const pathname = url.pathname.replace(/\/+$/, '') || '/';

  if ((pathname === '/sub' || pathname === '/api/info') && !tokenOK(url)) {
    return send(res, 403, 'application/json; charset=utf-8', JSON.stringify({ error: 'forbidden' }));
  }

  if (pathname === '/healthz' || pathname === '/ping') {
    const body = JSON.stringify({
      ok: xrayHealthy(),
      xray: xrayHealthy(),
      restarts: xrayRestarts,
      ws_path: WS_PATH,
      socks: SOCKS_ENABLED,
      port: PORT,
      uptime_s: Math.floor(process.uptime())
    });
    return send(res, xrayHealthy() ? 200 : 503, 'application/json; charset=utf-8', body);
  }

  if (pathname === '/' || pathname === '/index.html') {
    res.writeHead(302, { Location: '/sub' + url.search, 'Cache-Control': 'no-store' });
    return res.end();
  }

  if (pathname === '/sub') {
    const format = (url.searchParams.get('format') || '').toLowerCase();
    if (format === 'clash') {
      return send(res, 200, 'text/yaml; charset=utf-8', clashFor(host));
    }
    if (url.searchParams.get('raw') === '1' || SUB_UA.test(req.headers['user-agent'] || '')) {
      const link = linkFor(host);
      return send(res, 200, 'text/plain; charset=utf-8', Buffer.from(link).toString('base64'));
    }
    let html = '<!doctype html><meta charset="utf-8"><title>node</title><pre id="l"></pre>';
    try { html = fs.readFileSync(INDEX_FILE, 'utf8'); } catch (_) {}
    html = html
      .replace(/__UUID__/g, UUID)
      .replace(/__HOST__/g, host)
      .replace(/__WS_PATH__/g, WS_PATH)
      .replace(/__PUBLIC_PORT__/g, PUBLIC_PORT)
      .replace(/__PUBLIC_TLS__/g, PUBLIC_TLS ? '1' : '0')
      .replace(/__SUB_TOKEN__/g, SUB_TOKEN);
    return send(res, 200, 'text/html; charset=utf-8', html);
  }

  if (pathname === '/api/info') {
    return send(res, 200, 'application/json; charset=utf-8', JSON.stringify({
      host,
      uuid: UUID,
      ws_path: WS_PATH,
      public_port: Number(PUBLIC_PORT),
      tls: PUBLIC_TLS,
      link: linkFor(host),
      sub_url: SUB_TOKEN ? '/sub?token=' + encodeURIComponent(SUB_TOKEN) : '/sub',
      token_required: Boolean(SUB_TOKEN)
    }));
  }

  return send(res, 404, 'text/plain; charset=utf-8', 'Not Found');
});

server.on('upgrade', (req, socket, head) => {
  let pathname = '/';
  try {
    pathname = new URL(req.url, 'http://localhost').pathname;
  } catch (_) {}
  if (pathname !== WS_PATH && pathname !== '/proxy') {
    socket.destroy();
    return;
  }
  const upstream = net.connect(XRAY_PORT, '127.0.0.1', () => {
    let raw = req.method + ' ' + req.url + ' HTTP/1.1\r\n';
    for (let i = 0; i < req.rawHeaders.length; i += 2) {
      raw += req.rawHeaders[i] + ': ' + req.rawHeaders[i + 1] + '\r\n';
    }
    raw += '\r\n';
    upstream.write(raw);
    if (head && head.length) upstream.write(head);
    socket.pipe(upstream);
    upstream.pipe(socket);
  });
  upstream.setNoDelay(true);
  upstream.on('error', () => socket.destroy());
  socket.on('error', () => upstream.destroy());
});

server.listen(PORT, '0.0.0.0', () => {
  console.log(`node front door on :${PORT}, xray on 127.0.0.1:${XRAY_PORT}, ws=${WS_PATH}`);
  console.log(`uuid=${UUID} (state: ${UUID_FILE})`);
  startXray();
});

function shutdown(signal) {
  console.log(`received ${signal}, shutting down`);
  server.close(() => process.exit(0));
  if (xray) {
    xray.removeAllListeners('exit');
    xray.kill('SIGTERM');
  }
  setTimeout(() => process.exit(0), 5000).unref();
}
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
