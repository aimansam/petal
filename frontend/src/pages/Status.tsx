import { ReactNode, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { API_BASE_PATH } from "../const";

// Read-only Status page: capture health, imports, flows per service, containers and settings.

type Container = { service: string; state: string; status: string; health: string };
type StatusData = {
  now: number;
  flows: { total: number; last_flow: number | null; last_5m: number; last_1h: number; flag_in_1h: number; flag_out_1h: number };
  services: { name: string; ip: string; port: number; flows_1h: number; last: number | null }[];
  other_flows_1h: number;
  pcaps: { files: number; bytes: number; pending_import: number; newest_file: number | null; capturing_files: number; last_packet: number | null };
  helper: { error?: string; containers?: Container[]; settings?: Record<string, string>; job?: { state: string; error: string | null } };
};
type Check = { level: "ok" | "warn" | "bad"; text: string };

const REFRESH_MS = 10000;

function ago(now: number, t: number | null): string {
  if (!t) return "never";
  const s = Math.max(0, Math.round(now - t));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} days ago`;
}

const size = (b: number) =>
  b < 1024 ? `${b} B` : b < 1048576 ? `${(b / 1024).toFixed(1)} KB` : b < 1073741824 ? `${(b / 1048576).toFixed(1)} MB` : `${(b / 1073741824).toFixed(2)} GB`;

function checks(d: StatusData): Check[] {
  const out: Check[] = [];
  const st = d.helper.settings ?? {};
  if (d.helper.error) out.push({ level: "warn", text: `Setup helper: ${d.helper.error}` });
  const down = (d.helper.containers ?? []).filter((c) => c.state !== "running");
  for (const c of down) out.push({ level: "bad", text: `Container ${c.service} is ${c.state} (${c.status})` });
  if (d.helper.settings && !st.CAPTURE_IFACE) out.push({ level: "bad", text: "Capture is off: pick services in Settings" });
  const quiet = d.pcaps.last_packet ? d.now - d.pcaps.last_packet : Infinity;
  if (st.CAPTURE_IFACE && quiet > 600) {
    out.push({ level: "warn", text: `No traffic captured for ${d.pcaps.last_packet ? ago(d.now, d.pcaps.last_packet).replace(" ago", "") : "ever"}. Nobody attacking, or the CTF containers were recreated (apply Settings again)?` });
  }
  if (d.pcaps.pending_import > 2) out.push({ level: "warn", text: `${d.pcaps.pending_import} pcaps waiting to be imported: the assembler is behind or stopped` });
  if (d.helper.job?.state === "failed") out.push({ level: "warn", text: `Last settings change failed: ${d.helper.job.error ?? "see Settings"}` });
  if (out.length === 0) out.push({ level: "ok", text: "All good: capturing, importing, every container running" });
  return out;
}

function Card({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="bg-white rounded-lg shadow-sm p-4">
      <h2 className="text-gray-700 mb-2">{title}</h2>
      {children}
    </section>
  );
}

function Row({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="flex justify-between gap-4 text-sm py-0.5">
      <span className="text-gray-500">{k}</span>
      <span className="text-gray-800 text-right break-all">{v}</span>
    </div>
  );
}

export function Status() {
  const [data, setData] = useState<StatusData | null>(null);
  const [error, setError] = useState("");
  const [loadedAt, setLoadedAt] = useState(0);
  const [, tick] = useState(0);

  useEffect(() => {
    let alive = true;
    async function load() {
      const ctl = new AbortController();
      const timer = setTimeout(() => ctl.abort(), 15000); // the API may be restarting
      try {
        const r = await fetch(`${API_BASE_PATH}/status`, { credentials: "same-origin", signal: ctl.signal });
        const d = await r.json();
        if (!r.ok) throw new Error(d.error ?? `error ${r.status}`);
        if (alive) { setData(d); setError(""); setLoadedAt(Date.now()); }
      } catch (e: any) {
        if (alive) setError(e?.name === "AbortError" ? "the server is not answering (restarting?)" : String(e?.message ?? e));
      } finally {
        clearTimeout(timer);
      }
    }
    load();
    const id = setInterval(load, REFRESH_MS);
    const clock = setInterval(() => tick((n) => n + 1), 1000);
    return () => { alive = false; clearInterval(id); clearInterval(clock); };
  }, []);

  const colors = { ok: "bg-green-100 text-green-800", warn: "bg-amber-100 text-amber-900", bad: "bg-red-100 text-red-800" };
  const st = data?.helper.settings ?? {};

  return (
    <div className="min-h-screen bg-gray-50 py-8">
      <div className="max-w-5xl mx-auto px-4">
        <div className="flex items-baseline gap-4 mb-4">
          <h1 className="text-3xl text-gray-700">🌸 Status</h1>
          <span className="text-sm text-gray-500">
            {loadedAt ? `updated ${Math.round((Date.now() - loadedAt) / 1000)}s ago, refreshes every ${REFRESH_MS / 1000}s` : "loading…"}
          </span>
          <span className="ml-auto flex gap-4 text-sm">
            <Link className="text-blue-600" to="/setup">Settings</Link>
            <Link className="text-blue-600" to="/">← dashboard</Link>
          </span>
        </div>

        {error && <div className="bg-red-100 text-red-800 rounded-md px-3 py-2 mb-4">Cannot load the status: {error}</div>}

        {data && (
          <>
            <div className="flex flex-col gap-2 mb-4">
              {checks(data).map((c, i) => (
                <div key={i} className={`rounded-md px-3 py-2 ${colors[c.level]}`}>{c.text}</div>
              ))}
            </div>

            <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mb-4">
              <Card title="Capture">
                <Row k="Interfaces" v={st.CAPTURE_IFACE ? st.CAPTURE_IFACE.split(",").length : "off"} />
                <Row k="Last packet" v={ago(data.now, data.pcaps.last_packet)} />
                <Row k="Pcaps imported" v={data.pcaps.files - data.pcaps.pending_import} />
                <Row k="Waiting to import" v={data.pcaps.pending_import} />
                <Row k="Being written" v={data.pcaps.capturing_files} />
                <Row k="Pcap folder size" v={size(data.pcaps.bytes)} />
              </Card>
              <Card title="Flows">
                <Row k="Total" v={data.flows.total.toLocaleString()} />
                <Row k="Last 5 minutes" v={data.flows.last_5m} />
                <Row k="Last hour" v={data.flows.last_1h} />
                <Row k="Flags in / out (1h)" v={`${data.flows.flag_in_1h} / ${data.flows.flag_out_1h}`} />
                <Row k="Newest flow" v={ago(data.now, data.flows.last_flow)} />
              </Card>
              <Card title="Containers">
                {(data.helper.containers ?? []).map((c) => (
                  <Row key={c.service} k={c.service} v={
                    <span className={c.state === "running" ? "text-green-700" : "text-red-700"}>{c.status || c.state}</span>
                  } />
                ))}
                {!data.helper.containers && <p className="text-sm text-gray-500">unknown (setup helper not reachable)</p>}
              </Card>
            </div>

            <Card title="Services (last hour)">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-left text-gray-500">
                    <th className="font-normal py-1">Service</th><th className="font-normal">Address</th>
                    <th className="font-normal text-right">Flows</th><th className="font-normal text-right">Last flow</th>
                  </tr>
                </thead>
                <tbody>
                  {data.services.map((s) => (
                    <tr key={`${s.ip}:${s.port}`} className="border-t">
                      <td className="py-1">{s.name}</td>
                      <td className="text-gray-500">{s.ip}:{s.port}</td>
                      <td className="text-right">{s.flows_1h}</td>
                      <td className="text-right text-gray-500">{ago(data.now, s.last)}</td>
                    </tr>
                  ))}
                  <tr className="border-t text-gray-500">
                    <td className="py-1">other</td><td /><td className="text-right">{data.other_flows_1h}</td><td />
                  </tr>
                </tbody>
              </table>
            </Card>

            <div className="mt-4">
              <Card title="Settings">
                <div className="grid grid-cols-1 md:grid-cols-2 gap-x-8">
                  <Row k="Vulnbox IP" v={st.VM_IP || "-"} />
                  <Row k="Dashboard" v={`${st.UI_BIND || "0.0.0.0"}:${st.UI_PORT || "3000"}`} />
                  <Row k="Attacker tags" v={st.VPN_RANGES ? `VPN ${st.VPN_RANGES}` : "every client IP"} />
                  <Row k="Flag regex" v={<code>{st.FLAG_REGEX || "-"}</code>} />
                  <Row k="CTF start" v={st.TICK_START || "-"} />
                  <Row k="Tick length" v={st.TICK_LENGTH ? `${Number(st.TICK_LENGTH) / 1000}s` : "-"} />
                  <Row k="Seconds per pcap" v={st.CAPTURE_ROTATE || "-"} />
                  <Row k="Capture filter" v={
                    st.CAPTURE_FILTER && st.CAPTURE_FILTER.length > 60 ? (
                      <details>
                        <summary className="cursor-pointer text-gray-600">{(st.CAPTURE_FILTER.match(/\bhost\b/g) ?? []).length} hosts (show)</summary>
                        <code className="text-xs block text-left mt-1">{st.CAPTURE_FILTER}</code>
                      </details>
                    ) : <code className="text-xs">{st.CAPTURE_FILTER || "-"}</code>
                  } />
                </div>
              </Card>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
