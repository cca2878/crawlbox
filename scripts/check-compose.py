#!/usr/bin/env python3
"""Exercise first-start and restart using only the public setup/login pages."""
import base64
import json
import os
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

os.environ["CRAWLBOX_IMAGE"] = "manager:ci"
os.environ["CRAWLBOX_PORT"] = "18080"
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
    before = run("exec", "-T", "kopia", "sha256sum", "/data/secrets.json")
    run("restart")
    wait_for(lambda: request(authenticated=True)[0] == 200)
    assert request()[0] == 401
    assert before == run("exec", "-T", "kopia", "sha256sum", "/data/secrets.json")
    assert password not in run("logs", "--no-color")
    new_password = "integration-only-reset-admin"
    subprocess.run(compose + ["exec", "-T", "manager", "manager", "reset-password"], input=new_password, text=True, check=True, capture_output=True)
    run("restart", "manager")
    password = new_password
    wait_for(lambda: request(authenticated=True)[0] == 200)
    assert request()[0] == 401
    print("Compose first administrator, internal Kopia, API isolation and restart passed")
finally:
    subprocess.run(compose + ["down", "--volumes"], check=True)
