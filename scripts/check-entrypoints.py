#!/usr/bin/env python3
"""Run embedded entrypoints against real Kopia, without requiring Docker."""
import argparse
import base64
import json
import http.cookiejar
import re
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
KOPIA = os.environ['KOPIA_BIN']
MANAGER = os.environ['MANAGER_BIN']
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--distinct-users', action='store_true', help='requires root; exercise different numeric users with a shared group')
parser.add_argument('--root-server', action='store_true', help='with --distinct-users, exercise a root server and a non-root manager')
args = parser.parse_args()
if args.root_server and not args.distinct_users:
    parser.error('--root-server requires --distinct-users')
if args.distinct_users and os.geteuid() != 0:
    parser.error('--distinct-users requires root')
manager_identity = dict(user=21001, group=21000, extra_groups=[]) if args.distinct_users else {}
server_identity = dict(user=21002, group=21000, extra_groups=[]) if args.distinct_users else {}
if args.root_server:
    server_identity = dict(user=0, group=0, extra_groups=[])


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def wait(check, processes=()):
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        for process in processes:
            assert process.poll() is None, 'entrypoint exited prematurely'
        try:
            if check():
                return
        except (OSError, ValueError):
            pass
        time.sleep(0.25)
    raise AssertionError('entrypoint readiness timed out')


with tempfile.TemporaryDirectory(prefix='crawlbox-entrypoints-') as directory:
    base = Path(directory)
    storage, manager, shared = [base / name for name in ('storage', 'manager', 'shared')]
    for path in (storage, manager, shared):
        path.mkdir()
    if args.distinct_users:
        os.chown(base, 0, 21000)
        base.chmod(0o750)
        for path, uid, mode in ((manager, 21001, 0o700), (storage, 21002, 0o700), (shared, 21002, 0o750)):
            os.chown(path, uid, 21000)
            path.chmod(mode)
    if args.root_server:
        os.chown(storage, 0, 0)
        os.chown(shared, 21001, 21000)
    mount_metadata = {path: (path.stat().st_uid, path.stat().st_gid, path.stat().st_mode) for path in (manager, storage, shared)}
    server_port, web_port, proxy_port = port(), port(), port()
    common = dict(os.environ, KOPIA_BINARY=KOPIA, MANAGER_BINARY=MANAGER,
                  CRAWLBOX_BOOTSTRAP=str(shared), KOPIA_CHECK_FOR_UPDATES='false')
    server_env = dict(common, CRAWLBOX_DATA=str(storage), KOPIA_LISTEN=f'https://127.0.0.1:{server_port}')
    if args.distinct_users:
        server_env.pop('USER', None)
        server_env.pop('LOGNAME', None)
        server_env['HOME'] = str(storage)
    manager_env = dict(common, CRAWLBOX_DATA=str(manager), KOPIA_URL=f'https://127.0.0.1:{server_port}',
                       CRAWLBOX_SETUP_LISTEN=f'127.0.0.1:{web_port}')
    # Custom configuration must survive entrypoint execution unchanged.
    config = dict(listen=f'127.0.0.1:{web_port}', data_dir=str(manager),
                  credentials=str(manager / 'admin.yaml'), kopia_binary=KOPIA,
                  kopia_config=str(manager / 'connection/repository.config'),
                  parallel=1, cache_bytes=21474836480, sources=[],
                  kopia_ui_proxy=dict(listen=f'127.0.0.1:{proxy_port}', target=f'https://127.0.0.1:{server_port}', fingerprint_file=str(shared / 'connection.json')))
    (manager / 'config.yaml').write_text(json.dumps(config))
    if args.distinct_users:
        os.chown(manager / 'config.yaml', 21001, 21000)
    processes = []
    logs = open(base / 'output.log', 'w+')

    def start(role):
        process = subprocess.Popen(['bash', str(ROOT / f'docker/{role}-entrypoint.sh')],
                                   env=server_env if role == 'kopia' else manager_env,
                                   stdout=logs, stderr=logs,
                                   **(server_identity if role == 'kopia' else manager_identity))
        processes.append(process)
        return process

    def stop():
        for process in reversed(processes):
            process.terminate()
        for process in processes:
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                raise
        processes.clear()

    password = 'entrypoint-test-password'

    def request(path='/ui/', data=None, authenticated=False):
        headers = {}
        if authenticated:
            headers['Authorization'] = 'Basic ' + base64.b64encode(f'admin:{password}'.encode()).decode()
        req = urllib.request.Request(f'http://127.0.0.1:{web_port}' + path, data=data, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=2) as response:
                return response.status, response.read().decode()
        except urllib.error.HTTPError as error:
            return error.code, error.read().decode()

    def proxy_request(secret=''):
        headers = {}
        if secret:
            headers['Authorization'] = 'Basic ' + base64.b64encode(f'admin:{secret}'.encode()).decode()
        endpoint = f'http://127.0.0.1:{proxy_port}'
        browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        try:
            with browser.open(urllib.request.Request(endpoint + '/', headers=headers), timeout=2) as response:
                html = response.read().decode()
            token = re.search(r'name="kopia-csrf-token" content="([^"]+)"', html)
            assert token, 'Kopia UI did not provide its CSRF token'
            headers['X-Kopia-Csrf-Token'] = token.group(1)
            with browser.open(urllib.request.Request(endpoint + '/api/v1/repo/status', headers=headers), timeout=2) as response:
                return response.status
        except urllib.error.HTTPError as error:
            return error.code

    try:
        start('manager')  # Exercise waiting for missing credentials/server.
        start('kopia')
        wait(lambda: '创建管理员账号' in request()[1], processes)
        body = urllib.parse.urlencode(dict(username='admin', password=password, confirm=password)).encode()
        assert request('/ui/setup', body)[0] == 201
        wait(lambda: request(authenticated=True)[0] == 200, processes)
        assert request('/api/v1/sources')[0] == 401
        assert proxy_request() == 503
        assert request('/ui/kopia-ui-proxy', b'enabled=true', authenticated=True)[0] == 200
        assert proxy_request() == 401
        secrets = (storage / 'secrets.json').read_bytes()
        assert proxy_request(json.loads(secrets)['server']) == 200
        assert proxy_request(password) == 401
        cert = (storage / 'server.crt').read_bytes()
        credentials = (manager / 'admin.yaml').read_bytes()
        client_config = manager / 'connection/repository.config'
        client_before = json.loads(client_config.read_text())
        fixture = base / 'fixture'
        fixture.mkdir()
        (fixture / 'example.txt').write_text('persistent history')
        client_cli = [KOPIA, '--config-file', str(client_config), '--disable-file-logging', '--no-progress']
        subprocess.run(client_cli + ['snapshot', 'create', str(fixture)], check=True, stdout=logs, stderr=logs, **manager_identity)
        stop()
        start('kopia')
        start('manager')
        wait(lambda: request(authenticated=True)[0] == 200, processes)
        assert json.loads(client_config.read_text()) == client_before, 'existing client connection changed'
        assert (storage / 'secrets.json').read_bytes() == secrets
        assert (storage / 'server.crt').read_bytes() == cert
        assert (manager / 'admin.yaml').read_bytes() == credentials
        assert proxy_request(json.loads(secrets)['server']) == 200
        assert request('/ui/kopia-ui-proxy', b'enabled=false', authenticated=True)[0] == 200
        assert proxy_request(json.loads(secrets)['server']) == 503
        snapshots = json.loads(subprocess.check_output(client_cli + ['snapshot', 'list', '--json'], stderr=logs, **manager_identity))
        assert len(snapshots) == 1
        stop()
        # Lost derived connections and handoff are reconstructed from durable credentials.
        (storage / 'repository.config').unlink()
        client_config.unlink()
        (shared / 'connection.json').unlink()
        start('kopia')
        start('manager')
        wait(lambda: request(authenticated=True)[0] == 200, processes)
        assert (storage / 'secrets.json').read_bytes() == secrets
        assert (storage / 'server.crt').read_bytes() == cert
        assert json.loads((manager / 'config.yaml').read_text()) == config
        stop()
        # Never replace missing credentials or incomplete TLS material on existing storage.
        for missing in ('secrets.json', 'server.key'):
            path = storage / missing
            saved = path.read_bytes()
            owner = path.stat()
            path.unlink()
            result = subprocess.run(['bash', str(ROOT / 'docker/kopia-entrypoint.sh')], env=server_env,
                                    stdout=logs, stderr=logs, timeout=30, **server_identity)
            assert result.returncode != 0
            assert not path.exists()
            path.write_bytes(saved)
            if args.distinct_users:
                os.chown(path, owner.st_uid, owner.st_gid)
        assert {path: (path.stat().st_uid, path.stat().st_gid, path.stat().st_mode) for path in mount_metadata} == mount_metadata
        assert (shared / "connection.json").stat().st_mode & 0o777 == 0o640
        logs.flush()
        logs.seek(0)
        output = logs.read()
        assert password not in output
        assert all(secret not in output for secret in json.loads(secrets).values())
        assert 'manager listening' in output
        print('Real Kopia entrypoints: setup, snapshot, restart, connection recovery, mount permissions and failure safety passed')
    except BaseException:
        logs.flush()
        logs.seek(0)
        # Redact persisted credentials even on assertion failures.
        output = logs.read().replace(password, '[redacted]')
        if (storage / 'secrets.json').exists():
            for secret in json.loads((storage / 'secrets.json').read_text()).values():
                output = output.replace(secret, '[redacted]')
        print(output)
        raise
    finally:
        stop()
        logs.close()
