import { useCallback, useEffect, useMemo, useState } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { Shell } from "@/components/layout/Shell";
import {
  ApiError,
  clearDashboardToken,
  exchangeBootstrapCodeIfPresent,
  fetchHealthz,
} from "@/lib/api";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import { LoopDetailPage } from "@/pages/LoopDetail";
import { LoopsPage } from "@/pages/Loops";
import { OverviewPage } from "@/pages/Overview";
import { ProjectsPage } from "@/pages/Projects";
import { ConfigPage } from "@/pages/Config";

function resolveHostPort(): string {
  // Prefer the browser's authority as displayed in the address bar.
  if (typeof window !== "undefined" && window.location.host) {
    return window.location.host;
  }
  const { hostname, port } = window.location;
  if (port) {
    return `${hostname}:${port}`;
  }
  if (hostname === "localhost" || hostname === "127.0.0.1") {
    return `${hostname}:17310`;
  }
  return hostname;
}

/**
 * True when the daemon has no bootstrap exchange route.
 *
 * The route is registered only for `server.authMode: "local-token"`; under
 * `"none"` the daemon answers 404 ROUTE_NOT_FOUND. There is nothing to recover
 * in that case — a daemon in `none` mode wants no token, and every request the
 * dashboard makes will succeed without one — so a stale `?code=` in the URL
 * must not block the app behind a failure screen whose recovery step (minting a
 * new code) 404s for exactly the same reason.
 */
export function isBootstrapRouteAbsent(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    err.status === 404 &&
    err.code === "ROUTE_NOT_FOUND"
  );
}

export default function App() {
  const [bootstrapped, setBootstrapped] = useState(false);
  const [bootstrapError, setBootstrapError] = useState<string | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [version, setVersion] = useState<string | undefined>();
  const hostPort = useMemo(() => resolveHostPort(), []);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      let bootstrapExchangeError: unknown;
      try {
        await exchangeBootstrapCodeIfPresent();
      } catch (err) {
        // An expired/replayed URL must not discard an otherwise valid restored
        // session. The authenticated health request below is the authority for
        // whether the browser token remains usable.
        if (!isBootstrapRouteAbsent(err)) {
          bootstrapExchangeError = err;
        }
      }

      try {
        // One read verifies both a fresh browser and a restored session token.
        // A rotated/expired session must reach the same recovery UI as a browser
        // with no token instead of rendering several unrelated panel-level 401s.
        await fetchHealthz();
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          clearDashboardToken();
          if (!cancelled) {
            const displayError = bootstrapExchangeError ?? err;
            const message =
              displayError instanceof Error
                ? displayError.message
                : String(displayError);
            setBootstrapError(message);
          }
        }
      } finally {
        if (!cancelled) {
          setBootstrapped(true);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const onHealthChange = useCallback(
    (nextHealthy: boolean | null, nextVersion?: string) => {
      setHealthy(nextHealthy);
      if (nextVersion) {
        setVersion(nextVersion);
      }
    },
    [],
  );

  if (!bootstrapped) {
    return (
      <div className="px-3 py-3 text-[12px] text-[var(--text-muted)]">
        Starting dashboard…
      </div>
    );
  }

  if (bootstrapError) {
    return (
      <div className="mx-auto flex max-w-lg flex-col gap-3 px-3 py-10">
        <h1 className="m-0 text-[16px] font-semibold">
          Dashboard login required
        </h1>
        <p className="m-0 text-[var(--text-muted)]">
          The dashboard could not establish an authenticated session. The
          details below may indicate a missing or expired code, an
          authentication-mode mismatch, a connectivity or Origin/Host
          rejection, or a daemon error.
        </p>
        <pre className="m-0 overflow-auto rounded border border-[var(--border)] bg-[var(--bg-muted)] p-2 mono text-[12px] text-[var(--danger)]">
          {bootstrapError}
        </pre>
        <div className="rounded border border-[var(--border)] bg-[var(--bg-elevated)] p-3 text-[12px]">
          <p className="m-0 mb-1 font-medium">Recovery</p>
          <ol className="m-0 list-decimal pl-4 text-[var(--text-muted)]">
            <li>Confirm the daemon is reachable and its health endpoint responds</li>
            <li>
              If the daemon uses{" "}
              <code className="mono text-[var(--text)]">
                server.authMode = &quot;local-token&quot;
              </code>
              , run{" "}
              <code className="mono text-[var(--text)]">
                looper dashboard
              </code>
              {" "}
              with the matching config or{" "}
              <code className="mono text-[var(--text)]">LOOPER_TOKEN</code>,
              then open the URL it prints
            </li>
            <li>
              A code is single-use and short-lived; a reused or expired one
              fails here
            </li>
          </ol>
        </div>
      </div>
    );
  }

  return (
    <BrowserRouter basename="/dashboard">
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route
                element={
                  <Shell
                    hostPort={hostPort}
                    healthy={healthy}
                    version={version}
                    onHealthChange={onHealthChange}
                  />
                }
              >
                <Route
                  index
                  element={<OverviewPage onHealthChange={onHealthChange} />}
                />
                <Route path="running" element={<Navigate to="/loops" replace />} />
                <Route path="loops" element={<LoopsPage />} />
                <Route path="loops/:selector" element={<LoopDetailPage />} />
                <Route path="projects" element={<ProjectsPage />} />
                <Route path="config" element={<ConfigPage />} />
                <Route path="*" element={<Navigate to="/" replace />} />
              </Route>
            </Routes>
          </ProjectFilterProvider>
        </DashboardDataProvider>
      </ToastProvider>
    </BrowserRouter>
  );
}
