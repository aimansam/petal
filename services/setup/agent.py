#!/usr/bin/env python3
"""Petal setup helper.

Runs next to the stack with access to Docker and the host network, so the dashboard never needs it.
It only listens on a unix socket shared with the API container (no TCP port), and the API only
forwards requests from logged-in users. All real work is done by install.sh:

  GET  /discover   facts for the setup page (PETAL_MODE=discover)
  POST /apply      validate settings, then run PETAL_MODE=apply ./install.sh in the background
  GET  /job        state and log of the last apply
  POST /password   set a new team password (hash in .env, then recreate the API)
"""

import json
import os
import re
import socketserver
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler

from werkzeug.security import generate_password_hash

HOME = os.environ.get("PETAL_HOME") or "/petal"
SOCK = os.environ.get("PETAL_SETUP_SOCK", "/run/petal/setup.sock")
INSTALL = os.path.join(HOME, "install.sh")
ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")

IPV4 = r"(\d{1,3}\.){3}\d{1,3}"
SERVICE = r"[A-Za-z0-9_.-]{1,40}:(" + IPV4 + r":)?\d{1,5}"
IFACE = r"[A-Za-z0-9_.@-]{1,32}"
CIDR = r"[0-9A-Fa-f:.]{2,45}(/\d{1,3})?"
RULES = {
    "VM_IP": rf"^{IPV4}$",
    "UI_BIND": rf"^{IPV4}$",
    "UI_PORT": r"^\d{1,5}$",
    "SERVICES": rf"^$|^{SERVICE}(,{SERVICE})*$",
    "CAPTURE_IFACE": rf"^$|^none$|^{IFACE}(,{IFACE})*$",
    "CAPTURE_FILTER": r"^[A-Za-z0-9 ().:/_!&|<>=\[\]-]{0,4000}$",
    "CAPTURE_ROTATE": r"^\d{1,4}$",
    "VPN_RANGES": rf"^$|^none$|^{CIDR}(,{CIDR})*$",
    "TICK_START": r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?([+-]\d{2}:\d{2}|Z)?$",
    "TICK_LENGTH": r"^\d{1,9}$",
    "FLAG_REGEX": r"^[^'\r\n\t]{1,200}$",
}
# Empty means "let install.sh decide" (auto capture interfaces/filter): not passed on.
AUTO_WHEN_EMPTY = {"CAPTURE_IFACE", "CAPTURE_FILTER"}

job_lock = threading.Lock()
job = {"state": "idle", "started": None, "finished": None, "log": [], "error": None}


def validate(settings):
    if not isinstance(settings, dict):
        raise ValueError("settings must be an object")
    clean = {}
    for key, value in settings.items():
        if key not in RULES:
            raise ValueError(f"unknown setting {key}")
        if not isinstance(value, str):
            raise ValueError(f"{key} must be text")
        value = value.strip()
        if key in ("SERVICES", "CAPTURE_IFACE", "VPN_RANGES"):
            value = re.sub(r"\s+", "", value)
        if not re.match(RULES[key], value):
            raise ValueError(f"invalid {key}: {value!r}")
        if key == "UI_PORT" and not 1 <= int(value) <= 65535:
            raise ValueError("UI_PORT must be 1-65535")
        if key in ("VM_IP", "UI_BIND") and any(int(p) > 255 for p in value.split(".")):
            raise ValueError(f"invalid {key}: {value!r}")
        if key == "FLAG_REGEX":
            try:
                re.compile(value)
            except re.error as e:
                raise ValueError(f"FLAG_REGEX does not compile: {e}")
        if key in AUTO_WHEN_EMPTY and value == "":
            continue
        if key == "VPN_RANGES" and value == "":
            value = "none"
        clean[key] = value
    return clean


def run_install(mode, extra_env=None, timeout=None):
    env = dict(os.environ, PETAL_MODE=mode, PETAL_UPDATED="1", TERM="dumb")
    env.update(extra_env or {})
    return subprocess.run(["bash", INSTALL], cwd=HOME, env=env, stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=timeout)


def discover():
    res = run_install("discover", timeout=120)
    out = {"defaults": {}, "env": {}, "host_ips": [], "interfaces": [], "containers": []}
    for line in res.stdout.splitlines():
        f = line.split("\t")
        if f[0] == "DEFAULT" and len(f) >= 3:
            out["defaults"][f[1]] = f[2]
        elif f[0] == "ENV" and len(f) >= 3:
            out["env"][f[1]] = f[2]
        elif f[0] == "HOSTIP" and len(f) >= 3:
            out["host_ips"].append({"iface": f[1], "ip": f[2]})
        elif f[0] == "IFACE" and len(f) >= 2:
            out["interfaces"].append(f[1])
        elif f[0] == "CONTAINER" and len(f) >= 6:
            out["containers"].append({"name": f[1], "container": f[2], "host_port": int(f[3]),
                                      "container_port": int(f[4]), "project": f[5]})
    if res.returncode != 0 and not out["env"]:
        raise RuntimeError(ANSI.sub("", res.stderr.strip())[-500:] or "discover failed")
    return out


def apply_worker(settings):
    env = dict(os.environ, PETAL_MODE="apply", PETAL_UPDATED="1", TERM="dumb", **settings)
    try:
        proc = subprocess.Popen(["bash", INSTALL], cwd=HOME, env=env, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
        for line in proc.stdout:
            line = ANSI.sub("", line.rstrip("\r\n"))
            if line.strip():
                with job_lock:
                    job["log"].append(line)
                    del job["log"][:-400]
        rc = proc.wait()
        with job_lock:
            job["state"] = "done" if rc == 0 else "failed"
            if rc != 0:
                errors = [l for l in job["log"] if l.startswith("[x]")]
                job["error"] = errors[-1][4:] if errors else f"install.sh exited with {rc}"
    except Exception as e:  # noqa: BLE001  report anything to the page
        with job_lock:
            job["state"], job["error"] = "failed", str(e)
    finally:
        with job_lock:
            job["finished"] = time.time()


def start_apply(settings):
    with job_lock:
        if job["state"] == "running":
            raise RuntimeError("settings are already being applied")
        job.update(state="running", started=time.time(), finished=None, log=[], error=None)
    threading.Thread(target=apply_worker, args=(settings,), daemon=True).start()


def write_env(key, value):
    path = os.path.join(HOME, ".env")
    line = f"{key}='{value}'\n"
    lines = open(path).readlines() if os.path.exists(path) else []
    for i, l in enumerate(lines):
        if l.startswith(key + "="):
            lines[i] = line
            break
    else:
        lines.append(line)
    with open(path, "r+" if os.path.exists(path) else "w") as f:  # rewrite in place: keeps owner/mode
        f.seek(0)
        f.writelines(lines)
        f.truncate()


def set_password(password):
    if not isinstance(password, str) or not 8 <= len(password) <= 128:
        raise ValueError("the password must be 8-128 characters")
    with job_lock:
        if job["state"] == "running":
            raise RuntimeError("settings are being applied, try again in a moment")
    write_env("PETAL_PASSWORD_HASH", generate_password_hash(password))

    def recreate_api():
        time.sleep(1)  # let the API answer this request first
        subprocess.run(["docker", "compose", "up", "-d", "--no-deps", "--no-build", "api"], cwd=HOME,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    threading.Thread(target=recreate_api, daemon=True).start()


class Handler(BaseHTTPRequestHandler):
    server_version = "petal-setup"

    def address_string(self):
        return "api"

    def _send(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n > 65536:
            raise ValueError("request too large")
        return json.loads(self.rfile.read(n) or b"{}")

    def do_GET(self):
        try:
            if self.path == "/discover":
                return self._send(200, discover())
            if self.path == "/job":
                with job_lock:
                    return self._send(200, dict(job))
            return self._send(404, {"error": "not found"})
        except Exception as e:  # noqa: BLE001
            return self._send(500, {"error": str(e)})

    def do_POST(self):
        try:
            data = self._body()
            if self.path == "/apply":
                start_apply(validate(data.get("settings")))
                return self._send(202, {"state": "running"})
            if self.path == "/password":
                set_password(data.get("password"))
                return self._send(200, {"ok": True})
            return self._send(404, {"error": "not found"})
        except ValueError as e:
            return self._send(400, {"error": str(e)})
        except RuntimeError as e:
            return self._send(409, {"error": str(e)})
        except Exception as e:  # noqa: BLE001
            return self._send(500, {"error": str(e)})


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def main():
    os.makedirs(os.path.dirname(SOCK), exist_ok=True)
    if os.path.exists(SOCK):
        os.unlink(SOCK)
    srv = Server(SOCK, Handler)
    os.chmod(SOCK, 0o660)
    print(f"petal setup helper listening on {SOCK} (petal home {HOME})", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
