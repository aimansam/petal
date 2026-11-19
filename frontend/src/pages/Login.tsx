import { FormEvent, useState } from "react";
import { API_BASE_PATH } from "../const";

export function Login({ onLoggedIn }: { onLoggedIn: () => Promise<void> }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await fetch(`${API_BASE_PATH}/auth/login`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });
      if (res.ok) {
        await onLoggedIn();
        return;
      }
      const data = await res.json().catch(() => ({}));
      setError(data.error ?? `login failed (${res.status})`);
    } catch {
      setError("cannot reach the server");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="h-screen flex items-center justify-center bg-gray-50">
      <form onSubmit={submit} className="bg-white shadow-md rounded-lg p-8 w-80 flex flex-col gap-4">
        <div className="text-center">
          <div className="text-4xl">🌸</div>
          <h1 className="text-2xl text-gray-700 mt-1">Petal</h1>
        </div>
        <input
          type="password"
          autoFocus
          autoComplete="current-password"
          placeholder="Team password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          className="border rounded-md px-3 py-2"
        />
        {error && <div className="text-red-600 text-sm">{error}</div>}
        <button
          type="submit"
          disabled={busy || !password}
          className="bg-pink-200 text-gray-800 rounded-md px-3 py-2 disabled:opacity-50"
        >
          {busy ? "Logging in…" : "Log in"}
        </button>
      </form>
    </div>
  );
}
