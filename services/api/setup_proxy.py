"""Forward the dashboard's setup page to the setup helper over its unix socket.

The routes live behind the login check in auth.py; the helper itself has no network port.
"""

import http.client
import json
import os
import socket

from flask import Flask, Response, request

SOCK = os.environ.get("PETAL_SETUP_SOCK", "/run/petal/setup.sock")


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: float):
        super().__init__("petal-setup", timeout=timeout)
        self._path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self._path)


def _forward(method: str, path: str, body=None, timeout: float = 150) -> Response:
    conn = _UnixConnection(SOCK, timeout)
    try:
        payload = json.dumps(body).encode() if body is not None else None
        headers = {"Content-Type": "application/json"} if payload else {}
        conn.request(method, path, body=payload, headers=headers)
        res = conn.getresponse()
        return Response(res.read(), status=res.status, mimetype="application/json")
    except (FileNotFoundError, ConnectionRefusedError):
        return Response(json.dumps({"error": "setup helper is not running (docker compose up -d setup)"}),
                        status=503, mimetype="application/json")
    except OSError as e:
        return Response(json.dumps({"error": f"setup helper: {e}"}), status=502, mimetype="application/json")
    finally:
        conn.close()


def init_setup_proxy(app: Flask) -> None:
    @app.get("/setup/discover")
    def setup_discover():
        return _forward("GET", "/discover")

    @app.get("/setup/job")
    def setup_job():
        return _forward("GET", "/job", timeout=10)

    @app.post("/setup/apply")
    def setup_apply():
        return _forward("POST", "/apply", request.get_json(silent=True) or {})

    @app.post("/setup/password")
    def setup_password():
        return _forward("POST", "/password", request.get_json(silent=True) or {})
