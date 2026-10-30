/** Linux --network none fixture; run the unmodified production main executable.
 * The synthetic CDN address is assigned ONLY to container loopback. A temporary
 * CA is trusted by this child process, never installed in the host trust store.
 * No Tools execution/result rows are seeded: submit -> poll -> archive -> reload.
 */
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHash, createHmac, randomBytes } from 'node:crypto';
import { createWriteStream, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import tls from 'node:tls';
import { networkInterfaces } from 'node:os';
import { resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { deflateSync } from 'node:zlib';

assert.equal(process.platform, 'linux');
const serve = process.env.FIXTURE_SERVE === 'true';
if (serve) {
  assert.ok(!readFileSync('/proc/net/route', 'utf8').trim().split('\n').slice(1).some(line => line.trim().split(/\s+/)[1] === '00000000'), 'fixture must have no default route; use a dedicated Docker internal network');
} else {
  assert.deepEqual(Object.keys(networkInterfaces()), ['lo'], 'requires network=none; never run on a networked host');
}
assert.ok(networkInterfaces().lo.some(a => a.address === '8.8.8.8'), 'fixture address must belong only to container loopback');
const [binary, output, certPath, keyPath] = process.argv.slice(2).map(p => resolve(p));
mkdirSync(output, { recursive: false, mode: 0o700 });
const dbPath = resolve(output, 'synthetic-only.db');
const signing = 'recovery-tools-fake-signing-key-000000000000';
const providerKey = 'recovery-fake-provider-key-not-a-secret';
const storageKey = 'synthetic-success-storage-not-a-secret';
const actor = { userId: 'fixture-user', agentId: 'fixture-agent', sessionId: 'fixture-session' };
const model = 'fixture/edit';
const digest = b => createHash('sha256').update(b).digest('hex');
const json = value => JSON.stringify(value);
const metrics = { submissions: 0, polls: 0, results: 0, cdnReads: 0, archives: 0, downloads: 0, connects: 0 };
const checks = [];
const check = (name, value) => { checks.push({ name, passed: !!value }); assert.ok(value, name); };

// Deterministic 1600x900 PNG; no model or image API creates these fixture bytes.
function crc32(b) {
  let n = 0xffffffff;
  for (const byte of b) { n ^= byte; for (let i = 0; i < 8; i++) n = (n >>> 1) ^ ((n & 1) ? 0xedb88320 : 0); }
  return (n ^ 0xffffffff) >>> 0;
}
function chunk(name, body) {
  const tag = Buffer.from(name), size = Buffer.alloc(4), crc = Buffer.alloc(4);
  size.writeUInt32BE(body.length); crc.writeUInt32BE(crc32(Buffer.concat([tag, body])));
  return Buffer.concat([size, tag, body, crc]);
}
const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(1600, 0); ihdr.writeUInt32BE(900, 4); ihdr[8] = 8; ihdr[9] = 2;
const rows = Buffer.alloc((1600 * 3 + 1) * 900, 150);
for (let y = 0; y < 900; y++) rows[y * (1600 * 3 + 1)] = 0;
const png = Buffer.concat([Buffer.from('89504e470d0a1a0a', 'hex'), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(rows)), chunk('IEND', Buffer.alloc(0))]);
const reference = 'data:image/png;base64,' + png.toString('base64');
const blobs = new Map(), tasks = new Map();
const requestBody = async req => { const parts = []; for await (const p of req) parts.push(p); return Buffer.concat(parts); };
const answer = (res, value, status = 200) => { const body = Buffer.from(json(value)); res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': body.length }); res.end(body); };
const listen = async (server, port = 0, host = '127.0.0.1') => { await new Promise((yes, no) => { server.once('error', no); server.listen(port, host, yes); }); return server.address().port; };
const close = server => new Promise(resolve => { server.closeAllConnections?.(); server.close(resolve); });
const tlsOptions = { cert: readFileSync(certPath), key: readFileSync(keyPath) };
let fatal;
const guarded = handler => async (req, res) => { try { await handler(req, res); } catch (error) { fatal = error; answer(res, { error: 'fixture_assertion' }, 500); } };
const providerHTTP = http.createServer(guarded(async (req, res) => {
  assert.equal(req.headers.authorization, 'Key ' + providerKey);
  if (req.method === 'POST' && req.url === '/fal-ai/fixture-edit') {
    const body = JSON.parse((await requestBody(req)).toString());
    for (const ref of body.image_urls ?? []) {
      assert.match(ref, /^data:image\/png;base64,/);
      const bytes = Buffer.from(ref.split(',')[1], 'base64');
      assert.equal(bytes.readUInt32BE(16), 1600); assert.equal(bytes.readUInt32BE(20), 900);
    }
    assert.deepEqual(body.image_size, { width: 1600, height: 900 });
    assert.equal(typeof body.prompt, 'string'); assert.ok(body.prompt.length > 0);
    metrics.submissions++;
    const id = 'synthetic-' + metrics.submissions; tasks.set(id, body.prompt);
    return answer(res, { request_id: id });
  }
  const poll = req.url.match(/^\/fal-ai\/fixture-edit\/requests\/(synthetic-\d+)(\/status)?$/);
  if (req.method === 'GET' && poll?.[2] && tasks.has(poll[1])) {
    metrics.polls++; return answer(res, { status: tasks.get(poll[1]).startsWith('MOCK_POLL_FAIL') ? 'FAILED' : 'COMPLETED' });
  }
  if (req.method === 'GET' && poll && tasks.has(poll[1])) {
    metrics.results++; return answer(res, { images: [{ url: 'https://fixture-cdn.invalid/image.png', width: 1600, height: 900 }] });
  }
  answer(res, { error: 'unexpected_fixture_path' }, 404);
}));
const tunnelTLS = tls.createServer(tlsOptions, socket => providerHTTP.emit('connection', socket));
const proxy = http.createServer((_req, res) => answer(res, { error: 'CONNECT only' }, 400));
const tunnelSockets = new Set();
proxy.on('connect', (req, socket, head) => {
  if (req.url !== 'queue.fal.run:443' || head.length) { socket.destroy(); return; }
  metrics.connects++;
  tunnelSockets.add(socket); socket.on('close', () => tunnelSockets.delete(socket));
  socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
  tunnelTLS.emit('connection', socket);
});
const cdn = https.createServer(tlsOptions, guarded(async (req, res) => {
  assert.equal(req.url, '/image.png'); metrics.cdnReads++;
  res.writeHead(200, { 'Content-Type': 'image/png', 'Content-Length': png.length }); res.end(png);
}));
const storage = http.createServer(guarded(async (req, res) => {
  assert.equal(req.headers.authorization, 'Bearer ' + storageKey);
  const u = new URL(req.url, 'http://fixture.invalid');
  assert.equal(u.pathname, '/api/internal/media/files');
  if (req.method === 'POST') {
    const bytes = await requestBody(req); assert.deepEqual(bytes, png);
    const task = req.headers['x-native-task-id'], index = req.headers['x-native-artifact-index'];
    assert.match(task, /^tools-[a-f0-9]{64}$/); assert.equal(index, '0');
    const key = `native-results/${task}/${index}/${digest(bytes)}.bin`;
    blobs.set(key, bytes); metrics.archives++;
    return answer(res, { code: 0, data: { Key: key, SHA256: digest(bytes), Size: bytes.length, Type: 'image/png' } });
  }
  const bytes = blobs.get(u.searchParams.get('key'));
  if (!bytes) return answer(res, { error: 'absent' }, 404);
  metrics.downloads++;
  res.writeHead(200, { 'Content-Type': 'image/png', 'Content-Length': bytes.length }); res.end(bytes);
}));
const proxyPort = await listen(proxy), storagePort = await listen(storage);
await listen(cdn, 443, '8.8.8.8');
const port = 13601, base = `http://127.0.0.1:${port}`;
const mainEnv = { PATH: '/usr/bin:/bin', ADMIN_KEY: 'image-agent-local-fake-key',
  SQLITE_PATH: dbPath, CONFIG_FILE_PATH: resolve(output, 'unused.yaml'), DISABLE_WEB: 'true', GIN_MODE: 'release',
  TOOLS_INTERNAL_KEY: signing, DISABLE_UPSTREAM_BILLING_RECOVERY: 'true',
  EXTERNAL_BALANCE_URL: `http://127.0.0.1:${storagePort}`, EXTERNAL_BALANCE_KEY: storageKey,
  HTTPS_PROXY: `http://127.0.0.1:${proxyPort}`, HTTP_PROXY: `http://127.0.0.1:${proxyPort}`,
  NO_PROXY: 'localhost,127.0.0.1', SSL_CERT_FILE: certPath };
const log = createWriteStream(resolve(output, 'main.log'));
let proc, bridge, completed = false;
async function start() {
  proc = spawn(binary, ['-listen', `127.0.0.1:${port}`, '-pprof-port', '13602'], { cwd: output, env: mainEnv, stdio: ['ignore', 'pipe', 'pipe'] });
  proc.stdout.pipe(log, { end: false }); proc.stderr.pipe(log, { end: false });
  for (let i = 0; i < 150; i++) {
    if (proc.exitCode !== null) throw new Error('main exited; see fixture main.log');
    // Fixed container loopback health probe; no credentials, no default route, no external plaintext transport.
    // nosemgrep: typescript.react.security.react-insecure-request.react-insecure-request
    try { if ((await fetch(base + '/api/status')).status === 200) return; } catch {}
    await new Promise(r => setTimeout(r, 100));
  }
  throw new Error('main not ready');
}
async function stop() {
  if (!proc || proc.exitCode !== null) return;
  const ended = new Promise(r => proc.once('exit', r)); proc.kill('SIGTERM');
  await Promise.race([ended, new Promise((_, reject) => setTimeout(() => reject(new Error('main stop timeout')), 15_000).unref())]);
}
async function request(method, path, body = '', { id = 'fixture-' + randomBytes(8).toString('hex'), revision = '', owner = actor } = {}) {
  const claims = { v: 1, ...owner, requestId: id, method, path, bodySha256: digest(body), revision, iat: Math.floor(Date.now() / 1000), nonce: randomBytes(16).toString('hex') };
  const payload = Buffer.from(json(claims)).toString('base64url');
  const sig = createHmac('sha256', signing).update('musespan-tools-execution-v1.' + payload).digest('base64url');
  const response = await fetch(base + path, { method, headers: { 'Content-Type': 'application/json', 'X-Request-Id': id, 'X-Tools-Execution-Revision': revision, 'X-Tools-Authorization': payload + '.' + sig }, ...(method !== 'GET' ? { body } : {}) });
  const bytes = Buffer.from(await response.arrayBuffer());
  let value; try { value = JSON.parse(bytes.toString()); } catch {}
  if (fatal) throw fatal;
  return { status: response.status, bytes, value };
}
try {
  await start(); await stop();
  const db = new DatabaseSync(dbPath);
  db.exec(readFileSync('/source/core/migrations/tools-execution-v1/sqlite.sql', 'utf8'));
  db.prepare('INSERT INTO channels(id,name,type,status,key,base_url) VALUES(11,?,59,1,?,?)').run('synthetic-only', providerKey, 'https://queue.fal.run');
  const contract = { version: 1, model, input_schema: { type: 'object', additionalProperties: false, required: ['prompt', 'image_urls', 'image_size'], properties: {
    prompt: { type: 'string' }, image_urls: { type: 'array', minItems: 1, maxItems: 1, items: { type: 'string', pattern: '^data:image/png;base64,[A-Za-z0-9+/=]+$' } },
    image_size: { type: 'object', additionalProperties: false, required: ['width', 'height'], properties: { width: { type: 'integer', minimum: 64, maximum: 8192 }, height: { type: 'integer', minimum: 64, maximum: 8192 } } } } },
    output_schema: { type: 'object', required: ['images'], properties: { images: { type: 'array', minItems: 1, maxItems: 1, items: { type: 'object', required: ['url', 'width', 'height'], properties: { url: { type: 'string' }, width: { type: 'integer' }, height: { type: 'integer' } } } } } }, artifacts: [{ path: ['images', '*', 'url'] }] };
  const binding = { version: 1, channelId: 11, endpoint: 'fal-ai/fixture-edit', credentialScope: 'synthetic-only', keyFingerprint: digest(providerKey), deliveryBase: 'https://fixture-gateway.invalid', contract };
  db.prepare('INSERT INTO tools_model_bindings VALUES(?,?,?,?,?,?)').run(model, 1, 'image-to-image', json(binding), '{}', 'a'.repeat(64));
  const appContract = { ...contract, model: 'local-recovery/image', input_schema: { ...contract.input_schema, required: ['prompt'] } };
  const appBinding = { ...binding, contract: appContract };
  db.prepare('INSERT INTO tools_model_bindings VALUES(?,?,?,?,?,?)').run('local-recovery/image', 1, 'image-to-image', json(appBinding), json({ modelId: 'synthetic', capabilityId: 'synthetic', draftId: 'synthetic', draftVersion: 1, channelId: '11' }), 'b'.repeat(64));
  check('no_seeded_execution_or_output', db.prepare('SELECT count(*) AS n FROM tools_executions').get().n === 0);
  db.close();
  await start();
  const access = await request('GET', '/internal/tools/model-access?model=fixture%2Fedit');
  check('real_main_edit_contract', access.status === 200 && access.value.agent.referenceFormat === 'data-uri-array');
  const id = 'tools_success_fixture', revision = access.value.revision;
  const body = json({ model, input: { prompt: 'Synthetic fixture: pale purple flower; preserve bird', image_urls: [reference], image_size: { width: 1600, height: 900 } } });
  const submission = await request('POST', '/internal/tools/model-tasks', body, { id, revision });
  check('official_transport_submits_once', submission.status === 202 && submission.value.status === 'pending' && metrics.submissions === 1);
  const result = await request('GET', '/internal/tools/model-tasks/' + id);
  check('submit_poll_download_archive_complete', result.status === 200 && result.value.status === 'completed' && metrics.polls === 1 && metrics.results === 1 && metrics.cdnReads === 1 && metrics.archives === 1);
  check('dimensions_preserved_in_output', result.value.output.images[0].width === 1600 && result.value.output.images[0].height === 900);
  check('provider_url_replaced_by_owned_url', result.value.output.images[0].url === `https://fixture-gateway.invalid/internal/tools/model-tasks/${id}/artifacts/0`);
  check('unknown_cost_remains_unreported', result.value.upstreamCost.status === 'unreported');
  const artifact = await request('GET', `/internal/tools/model-tasks/${id}/artifacts/0`);
  check('archived_bytes_match_actual_download', artifact.status === 200 && artifact.bytes.equals(png) && metrics.downloads === 1);
  await request('POST', '/internal/tools/model-tasks', body, { id, revision });
  check('successful_replay_does_not_regenerate', metrics.submissions === 1 && metrics.archives === 1);
  const wrong = await request('GET', `/internal/tools/model-tasks/${id}/artifacts/0`, '', { owner: { ...actor, sessionId: 'other-session' } });
  check('cross_session_read_denied', wrong.status === 404 && metrics.downloads === 1);
  await stop(); await start();
  const reload = await request('GET', '/internal/tools/model-tasks/' + id);
  const reloadedImage = await request('GET', `/internal/tools/model-tasks/${id}/artifacts/0`);
  check('restart_refresh_preserves_result_and_bytes', reload.value.status === 'completed' && reloadedImage.bytes.equals(png) && metrics.submissions === 1 && metrics.archives === 1 && metrics.polls === 1);
  const verify = new DatabaseSync(dbPath, { readOnly: true });
  check('persisted_execution_not_seeded_result', verify.prepare('SELECT count(*) AS n FROM tools_executions WHERE status=\'completed\'').get().n === 1);
  check('no_api_token_created', verify.prepare('SELECT count(*) AS n FROM tokens').get().n === 0);
  check('database_integrity', verify.prepare('PRAGMA integrity_check').get().integrity_check === 'ok'); verify.close();
  completed = true;
  if (serve) {
    bridge = http.createServer((req, res) => {
      if (req.url === '/__test/state') return answer(res, { syntheticOnly: true, fakeImageSHA256: digest(png), taskCount: metrics.submissions, events: [...tasks].map(([id]) => ({ event: 'submit', id })), metrics });
      const forwarded = http.request(base + req.url, { method: req.method, headers: req.headers }, upstream => {
        res.writeHead(upstream.statusCode, upstream.headers); upstream.pipe(res);
      });
      forwarded.on('error', () => { if (!res.headersSent) res.writeHead(502); res.end(); });
      req.pipe(forwarded);
    });
    await listen(bridge, 13603, '0.0.0.0');
    writeFileSync(resolve(output, 'ready.json'), json({ ready: true, binarySHA256: digest(readFileSync(binary)), metrics }));
    while (!existsSync(resolve(output, 'STOP'))) await new Promise(r => setTimeout(r, 500));
  }
} finally {
  await stop();
  if (bridge) await close(bridge);
  for (const socket of tunnelSockets) socket.destroy();
  await close(proxy); await close(storage); await close(cdn); log.end();
  writeFileSync(resolve(output, 'acceptance.json'), json({ completed, checks, metrics, binarySHA256: digest(readFileSync(binary)), imageSHA256: digest(png), dimensions: [1600, 900],
    scope: 'unmodified Linux production main; real TLS validation/HTTP proxy/public-IP guard and archive; synthetic providers/storage in network-none container; no app/LLM quality acceptance' }));
}
console.log(json({ passed: checks.length, metrics, output }));
