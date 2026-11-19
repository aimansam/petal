import { FormEvent, ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { API_BASE_PATH } from "../const";
import { useAuth } from "../auth";

// Setup page: first-run wizard and, later, the Settings page. Everything is applied by the setup
// helper (install.sh with PETAL_MODE=apply) through the API's /setup/* routes.

type Container = { name: string; container: string; host_port: number; container_port: number; project: string };
type Discover = {
  defaults: Record<string, string>;
  env: Record<string, string>;
  host_ips: { iface: string; ip: string }[];
  interfaces: string[];
  containers: Container[];
};
type Job = { state: "idle" | "running" | "done" | "failed"; log: string[]; error: string | null };

// Requests can stall while the API container is being replaced: always give up after a while.
function api(path: string, init?: RequestInit, timeoutMs = 15000): Promise<Response> {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs);
  return fetch(`${API_BASE_PATH}${path}`, { credentials: "same-origin", ...init, signal: ctl.signal })
    .finally(() => clearTimeout(timer));
}

const postJson = (path: string, body: unknown) =>
  api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });

const key = (c: Container) => `${c.container}:${c.host_port}`;

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="bg-white rounded-lg shadow-sm p-5 mb-4">
      <h2 className="text-lg text-gray-700">{title}</h2>
      {hint && <p className="text-sm text-gray-500 mb-3">{hint}</p>}
      {children}
    </section>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="flex flex-col text-sm text-gray-600 gap-1">
      {label}
      {children}
    </label>
  );
}

const input = "border rounded-md px-2 py-1 text-gray-800";

export function Setup() {
  const { configured, refresh } = useAuth();
  const navigate = useNavigate();
  const [info, setInfo] = useState<Discover | null>(null);
  const [loadError, setLoadError] = useState("");

  const [vmIp, setVmIp] = useState("");
  const [uiBind, setUiBind] = useState("0.0.0.0");
  const [uiPort, setUiPort] = useState("3000");
  const [picked, setPicked] = useState<Record<string, string>>({}); // container:hostport -> service name
  const [extraServices, setExtraServices] = useState("");
  const [vpnRanges, setVpnRanges] = useState("");
  const [tickStart, setTickStart] = useState("");
  const [tickLength, setTickLength] = useState("120000");
  const [flagRegex, setFlagRegex] = useState("[A-Z0-9]{31}=");
  const [autoCapture, setAutoCapture] = useState(true);
  const [captureIface, setCaptureIface] = useState("");
  const [captureFilter, setCaptureFilter] = useState("");
  const [rotate, setRotate] = useState("60");

  const [job, setJob] = useState<Job | null>(null);
  const [applyError, setApplyError] = useState("");
  const [newUrl, setNewUrl] = useState("");
  const polling = useRef(false);

  const [pw, setPw] = useState("");
  const [pw2, setPw2] = useState("");
  const [pwMsg, setPwMsg] = useState("");

  useEffect(() => {
    api("/setup/discover", undefined, 150000)
      .then(async (r) => {
        const d = await r.json();
        if (!r.ok) throw new Error(d.error ?? `error ${r.status}`);
        return d as Discover;
      })
      .then((d) => {
        setInfo(d);
        const env = d.env, def = d.defaults;
        setVmIp(env.VM_IP || def.VM_IP || "");
        setUiBind(env.UI_BIND || "0.0.0.0");
        setUiPort(env.UI_PORT || def.UI_PORT || "3000");
        setVpnRanges(env.VPN_RANGES || def.VPN_RANGES || "");
        setTickStart(env.TICK_START || def.TICK_START || "");
        setTickLength(env.TICK_LENGTH || "120000");
        setFlagRegex(env.FLAG_REGEX || "[A-Z0-9]{31}=");
        setRotate(env.CAPTURE_ROTATE || "60");
        setCaptureIface(env.CAPTURE_IFACE || "");
        setCaptureFilter(env.CAPTURE_FILTER || "");
        // Saved services "name:hostport" -> tick the matching containers, keep the rest as text
        const sel: Record<string, string> = {};
        const extra: string[] = [];
        for (const entry of (env.SERVICES || "").split(",").filter(Boolean)) {
          const parts = entry.split(":");
          const c = parts.length === 2 ? d.containers.find((c) => String(c.host_port) === parts[1]) : undefined;
          if (c) sel[key(c)] = parts[0];
          else extra.push(entry);
        }
        setPicked(sel);
        setExtraServices(extra.join(","));
      })
      .catch((e) => setLoadError(String(e.message ?? e)));
  }, []);

  const projects = useMemo(() => {
    const groups: Record<string, Container[]> = {};
    for (const c of info?.containers ?? []) (groups[c.project] ??= []).push(c);
    return Object.entries(groups);
  }, [info]);

  function toggle(c: Container, on: boolean) {
    setPicked((p) => {
      const n = { ...p };
      if (on) n[key(c)] = n[key(c)] ?? c.name;
      else delete n[key(c)];
      return n;
    });
  }

  function servicesValue(): string {
    const fromPicks = (info?.containers ?? [])
      .filter((c) => key(c) in picked)
      .map((c) => `${(picked[key(c)] || c.name).replace(/[^A-Za-z0-9_.-]/g, "-")}:${c.host_port}`);
    const extra = extraServices.split(",").map((s) => s.trim()).filter(Boolean);
    return [...fromPicks, ...extra].join(",");
  }

  async function pollJob() {
    if (polling.current) return;
    polling.current = true;
    try {
      for (;;) {
        await new Promise((r) => setTimeout(r, 2000));
        let j: Job | null = null;
        try {
          const r = await api("/setup/job", undefined, 8000);
          if (r.ok) j = await r.json();
        } catch {
          // the API restarts while settings are applied: keep trying
        }
        if (j) {
          setJob(j);
          if (j.state !== "running") return j;
        }
      }
    } finally {
      polling.current = false;
    }
  }

  async function apply(e: FormEvent) {
    e.preventDefault();
    setApplyError("");
    setNewUrl("");
    const settings: Record<string, string> = {
      VM_IP: vmIp, UI_BIND: uiBind, UI_PORT: uiPort, SERVICES: servicesValue(),
      VPN_RANGES: vpnRanges, TICK_START: tickStart, TICK_LENGTH: tickLength, FLAG_REGEX: flagRegex,
      CAPTURE_ROTATE: rotate,
      CAPTURE_IFACE: autoCapture ? "" : captureIface,
      CAPTURE_FILTER: autoCapture ? "" : captureFilter,
    };
    const r = await postJson("/setup/apply", { settings });
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      setApplyError(d.error ?? `error ${r.status}`);
      return;
    }
    setJob({ state: "running", log: [], error: null });
    const j = await pollJob();
    if (j?.state === "done") {
      const host = uiBind === "0.0.0.0" ? window.location.hostname : uiBind;
      const url = `${window.location.protocol}//${host}:${uiPort}/`;
      if (host !== window.location.hostname || uiPort !== (window.location.port || "80")) {
        setNewUrl(url);
      } else {
        await refresh();
        navigate("/");
      }
    }
  }

  async function changePassword(e: FormEvent) {
    e.preventDefault();
    if (pw !== pw2) return setPwMsg("the passwords don't match");
    const r = await postJson("/setup/password", { password: pw });
    const d = await r.json().catch(() => ({}));
    if (r.ok) {
      setPwMsg("Password changed. Everyone (you too) will be asked to log in again.");
      setPw(""); setPw2("");
    } else setPwMsg(d.error ?? `error ${r.status}`);
  }

  if (loadError) return <div className="p-8 text-red-600">Cannot load the setup data: {loadError}</div>;
  if (!info) return <div className="p-8 text-gray-500">Looking at this box (containers, interfaces)…</div>;

  const running = job?.state === "running";
  return (
    <div className="min-h-screen bg-gray-50 py-8">
      <div className="max-w-3xl mx-auto px-4">
        <h1 className="text-3xl text-gray-700 mb-1">🌸 {configured ? "Settings" : "Set up Petal"}</h1>
        <p className="text-gray-500 mb-6">
          {configured
            ? "Change what Petal captures and how it runs. Applying recreates the Petal containers (a few seconds)."
            : "Pick the services to watch and check the defaults, then apply. You can change all of this later under Settings."}
        </p>

        <form onSubmit={apply}>
          <Section title="Services" hint="Running containers that publish a port. Traffic to the ticked ones is captured (also through nginx/tunnels) and gets its own tab.">
            {projects.length === 0 && <p className="text-sm text-gray-500">No running containers with published ports found.</p>}
            {projects.map(([project, list]) => (
              <div key={project} className="mb-3">
                <div className="flex items-center gap-3 text-sm text-gray-700 font-medium">
                  {project}
                  <button type="button" className="text-xs text-blue-600" onClick={() => list.forEach((c) => toggle(c, true))}>select all</button>
                  <button type="button" className="text-xs text-blue-600" onClick={() => list.forEach((c) => toggle(c, false))}>none</button>
                </div>
                {list.map((c) => (
                  <div key={key(c)} className="flex items-center gap-2 text-sm ml-2 my-1">
                    <input type="checkbox" checked={key(c) in picked} onChange={(e) => toggle(c, e.target.checked)} />
                    <input
                      className={`${input} w-40`}
                      disabled={!(key(c) in picked)}
                      value={picked[key(c)] ?? c.name}
                      onChange={(e) => setPicked((p) => ({ ...p, [key(c)]: e.target.value }))}
                    />
                    <span className="text-gray-500">{c.container} · port {c.host_port} → {c.container_port}</span>
                  </div>
                ))}
              </div>
            ))}
            <Field label="Other services (not in Docker), name:port or name:ip:port, comma separated">
              <input className={input} value={extraServices} onChange={(e) => setExtraServices(e.target.value)} />
            </Field>
          </Section>

          <Section title="Network">
            <div className="grid grid-cols-3 gap-3">
              <Field label="Vulnbox IP">
                <input className={input} value={vmIp} onChange={(e) => setVmIp(e.target.value)} />
              </Field>
              <Field label="Dashboard IP">
                <select className={input} value={uiBind} onChange={(e) => setUiBind(e.target.value)}>
                  <option value="0.0.0.0">all IPs (0.0.0.0)</option>
                  {info.host_ips.map((h) => (
                    <option key={h.ip} value={h.ip}>{h.ip} ({h.iface})</option>
                  ))}
                </select>
              </Field>
              <Field label="Dashboard port">
                <input className={input} value={uiPort} onChange={(e) => setUiPort(e.target.value)} />
              </Field>
            </div>
          </Section>

          <Section title="Attacker tags" hint="Requests through nginx/tunnels get a src:<ip> tag with the real attacker. Limit it to the competition VPN (CIDR, comma separated) or leave empty to tag every client.">
            <input className={`${input} w-full`} placeholder="e.g. 10.60.0.0/16" value={vpnRanges} onChange={(e) => setVpnRanges(e.target.value)} />
          </Section>

          <Section title="Game">
            <div className="grid grid-cols-3 gap-3">
              <Field label="CTF start (ISO 8601)">
                <div className="flex gap-1">
                  <input className={`${input} flex-1 min-w-0`} value={tickStart} onChange={(e) => setTickStart(e.target.value)} />
                  <button type="button" className="text-xs text-blue-600" onClick={() => setTickStart(info.defaults.TICK_START || "")}>now</button>
                </div>
              </Field>
              <Field label="Tick length (ms)">
                <input className={input} value={tickLength} onChange={(e) => setTickLength(e.target.value)} />
              </Field>
              <Field label="Flag regex">
                <input className={input} value={flagRegex} onChange={(e) => setFlagRegex(e.target.value)} />
              </Field>
            </div>
          </Section>

          <Section title="Capture">
            <label className="flex items-center gap-2 text-sm text-gray-700">
              <input type="checkbox" checked={autoCapture} onChange={(e) => setAutoCapture(e.target.checked)} />
              Automatic (recommended): capture on every Docker network of the selected services
            </label>
            {!autoCapture && (
              <div className="grid gap-3 mt-3">
                <Field label={`Interfaces, comma separated (available: ${info.interfaces.filter((i) => !i.startsWith("veth")).join(", ")})`}>
                  <input className={input} value={captureIface} onChange={(e) => setCaptureIface(e.target.value)} />
                </Field>
                <Field label="BPF filter">
                  <input className={input} value={captureFilter} onChange={(e) => setCaptureFilter(e.target.value)} />
                </Field>
              </div>
            )}
            <div className="mt-3 w-40">
              <Field label="Seconds per pcap file">
                <input className={input} value={rotate} onChange={(e) => setRotate(e.target.value)} />
              </Field>
            </div>
          </Section>

          {applyError && <p className="text-red-600 mb-3">{applyError}</p>}
          <button type="submit" disabled={running} className="bg-pink-200 text-gray-800 rounded-md px-4 py-2 disabled:opacity-50">
            {running ? "Applying…" : configured ? "Apply settings" : "Finish setup"}
          </button>
        </form>

        {job && job.state !== "idle" && (
          <Section title={running ? "Applying…" : job.state === "done" ? "Done" : "Failed"}>
            {job.error && <p className="text-red-600 mb-2">{job.error}</p>}
            {newUrl && (
              <p className="mb-2">
                The dashboard moved: <a className="text-blue-600 underline" href={newUrl}>{newUrl}</a>
              </p>
            )}
            <pre className="text-xs bg-gray-900 text-gray-100 rounded-md p-3 max-h-64 overflow-auto whitespace-pre-wrap">
              {job.log.slice(-25).join("\n") || "starting…"}
            </pre>
          </Section>
        )}

        {configured && (
          <Section title="Team password">
            <form onSubmit={changePassword} className="flex flex-wrap items-end gap-3">
              <Field label="New password">
                <input type="password" autoComplete="new-password" className={input} value={pw} onChange={(e) => setPw(e.target.value)} />
              </Field>
              <Field label="Repeat">
                <input type="password" autoComplete="new-password" className={input} value={pw2} onChange={(e) => setPw2(e.target.value)} />
              </Field>
              <button type="submit" disabled={pw.length < 8} className="bg-gray-200 rounded-md px-3 py-1 disabled:opacity-50">Change password</button>
            </form>
            {pwMsg && <p className="text-sm text-gray-600 mt-2">{pwMsg}</p>}
          </Section>
        )}

        {configured && (
          <button className="text-sm text-blue-600" onClick={() => navigate("/")}>← back to the dashboard</button>
        )}
      </div>
    </div>
  );
}
