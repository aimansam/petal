"""Read-only health and status for the dashboard's Status page.

Combines what the API can see itself (pcap folder, database) with container states and capture
settings from the setup helper (which has Docker access). Nothing here changes anything.
"""

import json
import time
from pathlib import Path

from flask import Flask, Response
from psycopg.rows import dict_row

from setup_proxy import _forward


def _pcap_stats(traffic_dir: Path, imported: set[str]) -> dict:
    files = sorted(p for p in traffic_dir.glob("*.pcap*") if p.is_file())
    staged = [p for p in (traffic_dir / ".incoming").glob("*.pcap*") if p.is_file()]
    pending = [p for p in files if str(p) not in imported]
    newest = max((p.stat().st_mtime for p in files), default=None)
    # tcpdump writes into the staged file as packets arrive: its mtime is the last captured packet
    last_packet = max((p.stat().st_mtime for p in staged if p.stat().st_size > 24), default=None)
    return {
        "files": len(files),
        "bytes": sum(p.stat().st_size for p in files),
        "pending_import": len(pending),
        "newest_file": newest,
        "capturing_files": len(staged),
        "last_packet": max(filter(None, [last_packet, newest]), default=None),
    }


def init_status(app: Flask, db, traffic_dir: Path, services: list[dict]) -> None:
    @app.get("/status")
    def system_status():
        out: dict = {"now": time.time()}

        with db.connection() as c, c.cursor(row_factory=dict_row) as cur:
            imported = {r["name"] for r in cur.execute("SELECT name FROM pcap")}
            row = cur.execute(
                """
                SELECT approximate_row_count('flow') AS total,
                       count(*) FILTER (WHERE time > now() - interval '5 minutes') AS last_5m,
                       count(*) AS last_1h,
                       count(*) FILTER (WHERE flags_in > 0) AS flag_in_1h,
                       count(*) FILTER (WHERE flags_out > 0) AS flag_out_1h
                FROM flow WHERE time > now() - interval '1 hour'
                """
            ).fetchone()
            last = cur.execute("SELECT extract(epoch FROM max(time)) AS t FROM flow").fetchone()
            # The fast estimate is 0 until the table is analysed; count exactly unless it is huge.
            if int(row["total"] or 0) < 1_000_000:
                row["total"] = cur.execute("SELECT count(*) AS n FROM flow").fetchone()["n"]
            per_dst = cur.execute(
                """
                SELECT host(ip_dst) AS ip, port_dst AS port, count(*) AS n,
                       extract(epoch FROM max(time)) AS last
                FROM flow WHERE time > now() - interval '1 hour' GROUP BY 1, 2
                """
            ).fetchall()

        out["flows"] = {
            "total": max(int(row["total"] or 0), int(row["last_1h"] or 0)),
            "last_flow": float(last["t"]) if last and last["t"] else None,
            "last_5m": row["last_5m"],
            "last_1h": row["last_1h"],
            "flag_in_1h": row["flag_in_1h"],
            "flag_out_1h": row["flag_out_1h"],
        }

        by_dst = {(r["ip"], r["port"]): r for r in per_dst}
        svc_rows = []
        for s in services:
            if s.get("port", -1) < 0:
                continue
            r = by_dst.pop((s["ip"], s["port"]), None)
            svc_rows.append({"name": s["name"], "ip": s["ip"], "port": s["port"],
                             "flows_1h": r["n"] if r else 0, "last": float(r["last"]) if r else None})
        out["services"] = svc_rows
        out["other_flows_1h"] = sum(r["n"] for r in by_dst.values())

        out["pcaps"] = _pcap_stats(traffic_dir, imported)

        helper = _forward("GET", "/status", timeout=20)
        try:
            out["helper"] = json.loads(helper.get_data()) if helper.status_code == 200 else {
                "error": json.loads(helper.get_data()).get("error", f"error {helper.status_code}")}
        except ValueError:
            out["helper"] = {"error": "setup helper gave no answer"}

        return Response(json.dumps(out, default=str), mimetype="application/json")
