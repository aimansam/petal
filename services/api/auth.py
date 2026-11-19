"""Team password login for the Petal API.

One shared password, stored only as a hash (PETAL_PASSWORD_HASH, written by install.sh). A signed
session cookie keeps people logged in. Every route except /auth/* needs a logged-in session.
"""

import hashlib
import hmac
import os
import time
from datetime import timedelta

from flask import Flask, jsonify, request, session
from werkzeug.security import check_password_hash

OPEN_PATHS = ("/auth/",)


def _password_hash() -> str:
    return os.environ.get("PETAL_PASSWORD_HASH", "")


def configured() -> bool:
    return os.environ.get("PETAL_CONFIGURED", "0") == "1"


def init_auth(app: Flask) -> None:
    secret = os.environ.get("PETAL_SECRET", "")
    if not secret:
        # Same value in every gunicorn worker (a random one per worker would break sessions).
        secret = hashlib.sha256(("petal-session:" + _password_hash()).encode()).hexdigest()
    app.config.update(
        SECRET_KEY=secret,
        SESSION_COOKIE_NAME="petal_session",
        SESSION_COOKIE_HTTPONLY=True,
        SESSION_COOKIE_SAMESITE="Strict",
        PERMANENT_SESSION_LIFETIME=timedelta(days=7),
    )

    @app.before_request
    def require_login():
        if request.path.startswith(OPEN_PATHS):
            return None
        if not _password_hash():
            return jsonify(error="no password set: run ./install.sh on the server"), 503
        if not _session_valid(app):
            session.clear()
            return jsonify(error="login required"), 401
        return None

    @app.post("/auth/login")
    def login():
        hashed = _password_hash()
        if not hashed:
            return jsonify(error="no password set: run ./install.sh on the server"), 503
        data = request.get_json(silent=True) or {}
        password = data.get("password", "")
        if not isinstance(password, str) or not check_password_hash(hashed, password):
            time.sleep(1)  # slow down guessing
            return jsonify(error="wrong password"), 401
        session.clear()
        session.permanent = True
        session["auth"] = True
        # Tie the session to the current password: changing it logs everyone out.
        session["pw"] = hmac.new(app.config["SECRET_KEY"].encode(), hashed.encode(), "sha256").hexdigest()[:16]
        return jsonify(logged_in=True, configured=configured())

    @app.post("/auth/logout")
    def logout():
        session.clear()
        return jsonify(logged_in=False)

    @app.get("/auth/status")
    def status():
        return jsonify(logged_in=_session_valid(app), configured=configured())


def _session_valid(app: Flask) -> bool:
    hashed = _password_hash()
    if not (hashed and session.get("auth")):
        return False
    want = hmac.new(app.config["SECRET_KEY"].encode(), hashed.encode(), "sha256").hexdigest()[:16]
    return hmac.compare_digest(session.get("pw", ""), want)
