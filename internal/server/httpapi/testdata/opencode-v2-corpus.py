#!/usr/bin/env python3
"""Build disposable selected-runtime corpus; accepts only explicitly supplied synthetic snapshots."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import sqlite3
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

VERSION = '2.0.30'
DIGEST = 'ddb556472e044be6d757fbc5fefc25b2edd2896ddbc0d388d6178b7b0a639b96'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--legacy-synthetic-db', type=Path, required=True)
    parser.add_argument('--native-synthetic-db', type=Path, required=True)
    parser.add_argument('--mixed-synthetic-db', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    if hashlib.sha256(binary.read_bytes()).hexdigest() != DIGEST:
        raise ValueError('selected binary digest differs')
    # Refuse ambient system policy without opening it.
    import pwd
    policies = [Path('/etc/opencode'), Path('/Library/Application Support/opencode'),
                Path('/Library/Managed Preferences/ai.opencode.managed.plist'),
                Path('/Library/Managed Preferences') / pwd.getpwuid(os.getuid()).pw_name / 'ai.opencode.managed.plist']
    if any(path.exists() or path.is_symlink() for path in policies):
        raise ValueError('system policy present; use an isolated host')
    root = args.output.absolute()
    root.mkdir(mode=0o700)
    root = root.resolve()
    for name in ['home', 'config/opencode', 'data', 'cache', 'state', 'tmp', 'project']:
        (root / name).mkdir(mode=0o700, parents=True, exist_ok=True)
    env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': str(root / 'home'),
           'TMPDIR': str(root / 'tmp'), 'OPENCODE_DB': str(root / 'rich.sqlite'),
           'OPENCODE_CONFIG_DIR': str(root / 'config/opencode'),
           'OPENCODE_DISABLE_PROJECT_CONFIG': '1', 'OPENCODE_CONFIG_PROJECT_DISABLE': '1',
           'OPENCODE_DISABLE_AUTOUPDATE': '1', 'OPENCODE_DISABLE_MODELS_FETCH': '1',
           'OPENCODE_DISABLE_FILEWATCHER': '1', 'OPENCODE_DISABLE_FFF': '1',
           'OPENCODE_PASSWORD': secrets.token_urlsafe(32)}
    env.update({'XDG_' + name.upper() + '_HOME': str(root / name) for name in ['config', 'data', 'cache', 'state']})
    (root / 'config/opencode/opencode.json').write_text(json.dumps({'update': 'disable', 'snapshots': False, 'plugins': [], 'mcp': {'servers': {}}}))
    version = subprocess.run([str(binary), '--version'], cwd=root / 'project', env=env, capture_output=True, text=True, check=True, timeout=20)
    if version.stdout.strip() not in [VERSION, 'opencode v' + VERSION]:
        raise ValueError('selected version differs')
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    endpoint = f'http://127.0.0.1:{port}'
    token = base64.b64encode(('opencode:' + env['OPENCODE_PASSWORD']).encode()).decode()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(path, payload=None):
        req = urllib.request.Request(endpoint + path, data=json.dumps(payload).encode() if payload is not None else None,
                                     headers={'Authorization': 'Basic ' + token, 'Content-Type': 'application/json'})
        with opener.open(req, timeout=5) as response:
            return json.load(response)
    with (root / 'server.log').open('w') as log:
        process = subprocess.Popen([str(binary), 'serve', '--hostname', '127.0.0.1', '--port', str(port)],
                                   cwd=root / 'project', env=env, stdout=log, stderr=log, start_new_session=True)
        try:
            deadline = time.monotonic() + 40
            while True:
                if process.poll() is not None:
                    raise RuntimeError('server exited')
                try:
                    info = request('/api/info')
                    if info['pid'] != process.pid or info['version'] != VERSION:
                        raise ValueError('server identity differs')
                    break
                except (urllib.error.URLError, TimeoutError):
                    if time.monotonic() > deadline:
                        raise
                    time.sleep(0.1)
            transfers = json.loads(Path(__file__).with_name('opencode-v2-transfer.json').read_text())
            for transfer in transfers:
                response = request('/api/experimental/session/import', dict(transfer, location={'directory': str(root / 'project')}))
                if response['data']['id'] != transfer['info']['id']:
                    raise ValueError('import changed identity')
            for transfer in transfers:
                response = request('/api/experimental/session/' + transfer['info']['id'] + '/export')
                (root / (transfer['info']['id'] + '-export.json')).write_text(json.dumps(response, indent=2) + '\n')
        finally:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=10)
            finally:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)
    for name, source in [('legacy.sqlite', args.legacy_synthetic_db), ('native.sqlite', args.native_synthetic_db), ('mixed.sqlite', args.mixed_synthetic_db)]:
        uri = 'file:' + urllib.parse.quote(str(source.resolve(strict=True))) + '?mode=ro'
        with sqlite3.connect(uri, uri=True) as src, sqlite3.connect(root / name) as dst:
            src.backup(dst)
    corpora = []
    for name in ['legacy.sqlite', 'native.sqlite', 'mixed.sqlite', 'rich.sqlite']:
        path = root / name
        # Normalize only disposable stopped-writer copies so readers need no WAL sidecars.
        with sqlite3.connect(path) as db:
            db.execute('PRAGMA journal_mode=DELETE')
            table = 'session' if name == 'legacy.sqlite' else 'session_v2'
            ids = [row[0] for row in db.execute('SELECT id FROM ' + table + ' ORDER BY id')]
        corpora.append({'path': name, 'sha256': hashlib.sha256(path.read_bytes()).hexdigest(), 'ids': ids})
    receipt = {'version': VERSION, 'binary_sha256': DIGEST, 'source_commit': 'f16ef9ad8878279ea2be3cfab7baeed97aef61aa',
               'corpora': corpora, 'owned_server_stopped': process.poll() is not None,
               'limit': 'Synthetic import validates selected-runtime schema/storage; no model/provider request was made.'}
    (root / 'corpus-evidence.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps(receipt, indent=2))


if __name__ == '__main__':
    main()
