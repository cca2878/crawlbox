#!/usr/bin/env python3
"""Exercise first-start and restart using only the public setup/login pages."""
import base64
import json
import http.cookiejar
import re
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

os.environ["CRAWLBOX_IMAGE"] = "manager:ci"
os.environ["CRAWLBOX_KOPIA_IMAGE"] = "kopia:ci"
os.environ["CRAWLBOX_VERSION"] = "ci"
os.environ["CRAWLBOX_PORT"] = "18080"
os.environ["CRAWLBOX_KOPIA_UI_PORT"] = "18081"
compose = ["docker", "compose", "-p", "crawlbox-ci", "-f", "deploy/compose.yaml"]
password = "integration-only-first-admin"

def run(*args):
    return subprocess.check_output(compose + list(args), text=True)

def request(path="/ui/", data=None, authenticated=False):
    headers = {}
    if authenticated:
        headers["Authorization"] = "Basic " + base64.b64encode(("admin:" + password).encode()).decode()
    req = urllib.request.Request("http://127.0.0.1:18080" + path, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=2) as response:
            return response.status, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()

def wait_for(check):
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, TimeoutError):
            pass
        time.sleep(1)
    raise RuntimeError("Compose readiness timed out")

def proxy_request(secret=''):
    headers = {}
    if secret:
        headers['Authorization'] = 'Basic ' + base64.b64encode(f'admin:{secret}'.encode()).decode()
    endpoint = 'http://127.0.0.1:18081'
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
    run("up", "-d", "--pull", "never")
    wait_for(lambda: "创建管理员账号" in request()[1])
    body = urllib.parse.urlencode({"username": "admin", "password": password, "confirm": password}).encode()
    assert request("/ui/setup", data=body)[0] == 201
    wait_for(lambda: request()[0] == 401)
    assert request(authenticated=True)[0] == 200
    assert request("/api/v1/sources")[0] == 401
    assert request("/ui/setup", data=body)[0] in (401, 404, 405)
    container = run("ps", "-q", "kopia").strip()
    ports = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]["NetworkSettings"]["Ports"]
    assert not any(ports.values())
    inspect = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
    assert inspect["Config"]["Image"] == "kopia:ci"
    assert sorted(run("config", "--services").split()) == ["kopia", "manager"]
    for service, expected in (("manager", "10001"), ("kopia", "0")):
        for identity in ("Uid", "Gid"):
            assert run("exec", "-T", service, "sh", "-c", f"sed -n '/^{identity}:/p' /proc/1/status").split()[1:] == [expected] * 4
    assert proxy_request() == 503
    assert request("/ui/kopia-ui-proxy", data=b"enabled=true", authenticated=True)[0] == 200
    assert proxy_request() == 401
    kopia_password = run("exec", "-T", "kopia", "jq", "-r", ".server", "/data/secrets.json").strip()
    assert proxy_request(kopia_password) == 200
    assert proxy_request(password) == 401
    before = run("exec", "-T", "kopia", "sha256sum", "/data/secrets.json")
    run("restart", "kopia", "manager")
    wait_for(lambda: request(authenticated=True)[0] == 200)
    assert request()[0] == 401
    assert before == run("exec", "-T", "kopia", "sha256sum", "/data/secrets.json")
    assert proxy_request(kopia_password) == 200
    assert request("/ui/kopia-ui-proxy", data=b"enabled=false", authenticated=True)[0] == 200
    assert proxy_request(kopia_password) == 503
    logs = run("logs", "--no-color")
    assert kopia_password not in logs
    assert password not in logs
    for event in ["manager listening", "history recovery completed", "no sources configured"]:
        assert event in logs, event
    new_password = "integration-only-reset-admin"
    subprocess.run(compose + ["exec", "-T", "manager", "manager-entrypoint", "reset-password"], input=new_password, text=True, check=True, capture_output=True)
    run("restart", "manager")
    password = new_password
    wait_for(lambda: request(authenticated=True)[0] == 200)
    assert request()[0] == 401
    # Recreate the whole project without deleting volumes: initialization must be repeatable.
    run("down")
    run("up", "-d", "--pull", "never")
    wait_for(lambda: request(authenticated=True)[0] == 200)
    assert before == run("exec", "-T", "kopia", "sha256sum", "/data/secrets.json")
    print("Compose paired entrypoints, non-root manager, logs, first setup, reset and recreate passed")
finally:
    subprocess.run(compose + ["down", "--volumes"], check=True)
