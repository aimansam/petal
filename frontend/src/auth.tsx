import { createContext, ReactNode, useCallback, useContext, useEffect, useState } from "react";
import { API_BASE_PATH } from "./const";
import { Login } from "./pages/Login";

// Fired by the API client whenever the server answers 401 (logged out / password changed).
export const UNAUTHORIZED_EVENT = "petal-unauthorized";

type AuthState = { loading: boolean; loggedIn: boolean; configured: boolean };

type AuthContextValue = AuthState & {
  refresh: () => Promise<void>;
  logout: () => Promise<void>;
};

const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth outside AuthGate");
  return ctx;
}

export function AuthGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ loading: true, loggedIn: false, configured: true });

  const refresh = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE_PATH}/auth/status`, { credentials: "same-origin" });
      const data = await res.json();
      setState({ loading: false, loggedIn: !!data.logged_in, configured: !!data.configured });
    } catch {
      setState((s) => ({ ...s, loading: false, loggedIn: false }));
    }
  }, []);

  const logout = useCallback(async () => {
    await fetch(`${API_BASE_PATH}/auth/logout`, { method: "POST", credentials: "same-origin" }).catch(() => {});
    await refresh();
  }, [refresh]);

  useEffect(() => {
    refresh();
    const onUnauthorized = () => refresh();
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, [refresh]);

  if (state.loading) {
    return <div className="h-screen flex items-center justify-center text-gray-500">Loading…</div>;
  }
  return (
    <AuthContext.Provider value={{ ...state, refresh, logout }}>
      {state.loggedIn ? children : <Login onLoggedIn={refresh} />}
    </AuthContext.Provider>
  );
}
