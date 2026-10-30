#!/usr/bin/env python3
"""macOS production-main HTTP acceptance. Synthetic DB, no upstream network.

No monkeypatch/test executable: launch the supplied normal main binary, seed only
an isolated fresh database, and exercise auth, frozen config, a rejected CONNECT,
idempotency, ownership, cancellation, timeout, and seeded artifact readback.
This does NOT claim successful provider generation or public-CDN archiving.
"""
import argparse
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request, build_opener, ProxyHandler

SIGNING = 'synthetic-main-tools-signing-key-not-a-secret'
ADMIN = 'synthetic-main-admin-not-a-secret'
PROVIDER = 'synthetic-main-provider-not-a-secret'
STORAGE = 'synthetic-main-storage-not-a-secret'
MODEL = 'fixture/main-image'
ACTOR = dict(userId='fixture-user', agentId='fixture-agent', sessionId='fixture-session')
PNG = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jP1sAAAAASUVORK5CYII=')
connects, storage_reads, checks = [], [], []


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_CONNECT(self):
        # Never resolve, dial or tunnel the target. The only network attempt
        # made by the real Fal transport receives this controlled failure.
        connects.append(self.path)
        self.send_response(502)
        self.send_header('Content-Length', '0')
        self.end_headers()

    def do_GET(self):
        if (self.path.startswith('/api/internal/media/files?key=') and
                self.headers.get('Authorization') == 'Bearer ' + STORAGE):
            storage_reads.append(self.path)
            self.send_response(200)
            self.send_header('Content-Type', 'image/png')
            self.send_header('Content-Length', str(len(PNG)))
            self.end_headers()
            self.wfile.write(PNG)
        else:
            self.send_response(404)
            self.send_header('Content-Length', '0')
            self.end_headers()


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def check(name, condition):
    checks.append(dict(name=name, passed=bool(condition)))
    assert condition, name


def encoded(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b'=').decode()


def signed(method, path, body='', request_id=None, revision='', actor=None):
    request_id = request_id or 'fixture-' + uuid.uuid4().hex
    claims = dict(v=1, **(actor or ACTOR), requestId=request_id, method=method,
                  path=path, bodySha256=hashlib.sha256(body.encode()).hexdigest(),
                  revision=revision, iat=int(time.time()), nonce=uuid.uuid4().hex)
    payload = encoded(json.dumps(claims, separators=(',', ':')).encode())
    sig = encoded(hmac.new(SIGNING.encode(), ('musespan-tools-execution-v1.' + payload).encode(), hashlib.sha256).digest())
    return {'Content-Type': 'application/json', 'X-Request-Id': request_id,
            'X-Tools-Authorization': payload + '.' + sig, 'X-Tools-Execution-Revision': revision}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    binary = Path(args.binary).resolve()
    out = Path(args.output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    dbfile = out / 'synthetic-only.db'
    fixture = ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
    proxy_port = fixture.server_address[1]
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    listen, pprof = port(), port()
    base = 'http://127.0.0.1:' + str(listen)
    # New process only; no system proxy/DNS settings are changed.
    profile = out / 'network.sb'
    profile.write_text('(version 1)\n(allow default)\n(deny network*)\n'
                       '(allow network-bind (local ip "localhost:*"))\n'
                       '(allow network-inbound (local ip "localhost:*"))\n'
                       f'(allow network-outbound (remote ip "localhost:{proxy_port}"))\n')
    env = dict(PATH='/usr/bin:/bin:/usr/sbin:/sbin', ADMIN_KEY=ADMIN,
               CONFIG_FILE_PATH=str(out / 'unused-config.yaml'), SQLITE_PATH=str(dbfile),
               DISABLE_WEB='true', GIN_MODE='release', TOOLS_INTERNAL_KEY=SIGNING,
               DISABLE_UPSTREAM_BILLING_RECOVERY='true', EXTERNAL_BALANCE_KEY=STORAGE,
               EXTERNAL_BALANCE_URL=f'http://127.0.0.1:{proxy_port}',
               HTTPS_PROXY=f'http://127.0.0.1:{proxy_port}', HTTP_PROXY=f'http://127.0.0.1:{proxy_port}',
               NO_PROXY='localhost,127.0.0.1')
    command = ['/usr/bin/sandbox-exec', '-f', str(profile), str(binary), '-listen',
               f'127.0.0.1:{listen}', '-pprof-port', str(pprof)]
    opener = build_opener(ProxyHandler({}))
    proc = None
    completed = False
    log = (out / 'main.log').open('wb')

    def http(method, path, body='', headers=None):
        request = Request(base + path, method=method, headers=headers or {},
                          data=body.encode() if method != 'GET' else None)
        try:
            response = opener.open(request, timeout=15)
        except HTTPError as error:
            response = error
        with response:
            raw = response.read(4 << 20)
            try:
                value = json.loads(raw)
            except (ValueError, UnicodeError):
                value = None
            return response.status, value, raw

    def req(method, path, body='', **kwargs):
        return http(method, path, body, signed(method, path, body, **kwargs))

    def start():
        process = subprocess.Popen(command, cwd=out, env=env, stdout=log, stderr=subprocess.STDOUT)
        for _ in range(150):
            if process.poll() is not None:
                raise RuntimeError('production main exited; inspect isolated main.log')
            try:
                if http('GET', '/api/status')[0] == 200:
                    return process
            except OSError:
                pass
            time.sleep(.1)
        process.terminate()
        process.wait(timeout=10)
        raise RuntimeError('production main startup timed out')

    def stop(process):
        process.terminate()
        process.wait(timeout=15)

    try:
        proc = start()
        check('main_health_before_tools_schema', http('GET', '/api/status')[0] == 200)
        check('missing_tools_storage_fails_closed', req('GET', '/internal/tools/models')[0] == 503)
        stop(proc)
        proc = None
        contract = dict(version=1, model=MODEL,
                        input_schema=dict(type='object', required=['prompt'], additionalProperties=False,
                                          properties=dict(prompt=dict(type='string', minLength=1))),
                        output_schema=dict(type='object', required=['images'], properties=dict(images=dict(
                            type='array', minItems=1, items=dict(type='object', required=['url'], properties=dict(url=dict(type='string')))))),
                        artifacts=[dict(path=['images', '*', 'url'])])
        binding = dict(version=1, channelId=11, endpoint='fal-ai/fixture-main', credentialScope='synthetic-only',
                       keyFingerprint=hashlib.sha256(PROVIDER.encode()).hexdigest(),
                       deliveryBase='https://fixture-gateway.invalid', contract=contract)
        migration = Path(__file__).resolve().parents[1] / 'core/migrations/tools-execution-v1/sqlite.sql'
        with sqlite3.connect(dbfile) as db:
            before_tables = {r[0] for r in db.execute("select name from sqlite_master where type='table'")}
            db.executescript(migration.read_text())
            after_tables = {r[0] for r in db.execute("select name from sqlite_master where type='table'")}
            check('exactly_three_additive_tools_tables', after_tables - before_tables == {'tools_executions', 'tools_model_bindings', 'tools_request_nonces'})
            db.execute('INSERT INTO channels(id,name,type,status,key,base_url) VALUES(11,?,59,1,?,?)', ('synthetic-only', PROVIDER, ''))
            db.execute('INSERT INTO tools_model_bindings VALUES(?,?,?,?,?,?)',
                       (MODEL, 1, 'text-to-image', json.dumps(binding), '{}', 'a' * 64))
        proc = start()
        check('unsigned_tools_denied', http('GET', '/internal/tools/models')[0] == 401)
        h = signed('GET', '/internal/tools/models')
        check('signed_tools_models', http('GET', '/internal/tools/models', headers=h)[0] == 200)
        check('nonce_replay_denied', http('GET', '/internal/tools/models', headers=h)[0] == 401)
        check('admin_binding_denied', http('GET', '/api/tools-model-bindings')[0] == 401)
        admin = {'Authorization': 'Bearer ' + ADMIN}
        check('admin_binding_read', http('GET', '/api/tools-model-bindings', headers=admin)[0] == 200)
        missing = http('GET', '/api/model_config/fixture/absent', headers=admin)
        check('main_typed404_preserved', missing[0] == 404 and missing[1].get('code') == 'model_config_not_found')
        access = req('GET', '/internal/tools/model-access?model=fixture%2Fmain-image')
        check('frozen_access', access[0] == 200 and len(access[1].get('revision', '')) == 64)
        rev = access[1]['revision']
        path = '/internal/tools/model-tasks'
        task_id = 'tools_fixture_' + uuid.uuid4().hex
        body = json.dumps(dict(model=MODEL, input=dict(prompt='synthetic only')))
        check('stale_revision_no_dispatch', req('POST', path, body, request_id=task_id, revision='0' * 64)[0] == 409 and len(connects) == 0)
        bad = json.dumps(dict(model=MODEL, input=dict(unexpected=True)))
        check('invalid_contract_no_dispatch', req('POST', path, bad, request_id=task_id, revision=rev)[0] == 422 and len(connects) == 0)
        first = req('POST', path, body, request_id=task_id, revision=rev)
        check('controlled_transport_failure_is_unconfirmed', first[0] == 202 and first[1]['status'] == 'unconfirmed')
        check('exactly_one_transport_attempt', connects == ['queue.fal.run:443'])
        replay = req('POST', path, body, request_id=task_id, revision=rev)
        check('replay_does_not_retry', replay[1]['id'] == task_id and len(connects) == 1)
        read = req('GET', path + '/' + task_id)
        check('unknown_cost_not_zero', read[1]['upstreamCost'] == {'status': 'unreported'})
        check('cross_owner_denied', req('GET', path + '/' + task_id, actor=dict(ACTOR, userId='other'))[0] == 404)
        check('conflicting_replay_denied', req('POST', path, body + ' ', request_id=task_id, revision=rev)[0] == 409)
        req('POST', path + '/' + task_id + '/cancel')
        check('unconfirmed_cancel_does_not_resubmit', len(connects) == 1)

        def seed(task, **values):
            row = dict(id=task, user_id=ACTOR['userId'], agent_id=ACTOR['agentId'], session_id=ACTOR['sessionId'],
                       model=MODEL, request_hash='b' * 64, revision=rev, binding_json=json.dumps(binding), status='pending',
                       upstream_id='synthetic-upstream', created_at=int(time.time()), updated_at=int(time.time()),
                       lease_owner='', lease_until=0, cancel_attempted=0, error_code='', output='', manifest='')
            row.update(values)
            with sqlite3.connect(dbfile) as db:
                db.execute('INSERT INTO tools_executions (' + ','.join(row) + ') VALUES(' + ','.join('?' for _ in row) + ')', tuple(row.values()))

        seed('tools_fixture_cancel')
        cancel = req('POST', path + '/tools_fixture_cancel/cancel')
        check('cancel_failure_persisted', cancel[0] == 200 and cancel[1]['status'] == 'cancellation_unconfirmed' and len(connects) == 2)
        req('POST', path + '/tools_fixture_cancel/cancel')
        check('cancel_replay_no_second_attempt', len(connects) == 2)
        seed('tools_fixture_expired', created_at=int(time.time()) - 601)
        expired = req('GET', path + '/tools_fixture_expired')
        check('expired_task_stops_before_provider', expired[1]['status'] == 'timed_out' and len(connects) == 2)
        digest = hashlib.sha256(PNG).hexdigest()
        task = 'tools_fixture_artifact'
        key = 'native-results/tools-' + hashlib.sha256(task.encode()).hexdigest() + '/0/' + digest + '.bin'
        receipt = dict(Key=key, SHA256=digest, Size=len(PNG), Type='image/png', Chunks=digest)
        seed(task, status='completed', manifest=json.dumps({'0': receipt}), output=json.dumps({'images': [{'url': 'https://fixture-gateway.invalid' + path + '/' + task + '/artifacts/0'}]}))
        asset = req('GET', path + '/' + task + '/artifacts/0')
        check('seeded_owned_artifact_integrity', asset[0] == 200 and asset[2] == PNG and len(storage_reads) == 1)
        check('cross_session_artifact_denied', req('GET', path + '/' + task + '/artifacts/0', actor=dict(ACTOR, sessionId='other'))[0] == 404 and len(storage_reads) == 1)
        with sqlite3.connect(dbfile) as db:
            tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            counts = {name: db.execute('SELECT count(*) FROM ' + name).fetchone()[0] if name in tables else 0 for name in ['tokens', 'native_tasks']}
            check('no_api_token_or_wallet_task_created', all(value == 0 for value in counts.values()))
            check('sqlite_integrity', db.execute('PRAGMA integrity_check').fetchone()[0] == 'ok')
        blocked = subprocess.run(['/usr/bin/sandbox-exec', '-f', str(profile), '/usr/bin/curl', '--max-time', '2', '-sS', 'http://1.1.1.1'], env={'PATH': '/usr/bin:/bin'}, capture_output=True)
        check('OS_blocks_external_network', blocked.returncode != 0)
        completed = True
    finally:
        if proc is not None and proc.poll() is None:
            stop(proc)
        log.close()
        fixture.shutdown()
        fixture.server_close()
        summary = dict(completed=completed, binarySHA256=hashlib.sha256(binary.read_bytes()).hexdigest(), checks=checks,
                       passed=sum(c['passed'] for c in checks), connectTargets=connects, storageReads=len(storage_reads),
                       scope='normal production main; synthetic database; rejected CONNECT; seeded artifact; not successful provider generation/CDN archive',
                       childExited=proc is None or proc.poll() is not None)
        (out / 'acceptance.json').write_text(json.dumps(summary, indent=2))
    print(json.dumps(dict(passed=len(checks), output=str(out))))


if __name__ == '__main__':
    main()
