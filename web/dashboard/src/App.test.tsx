import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "@/App";

/**
 * Mirrors the daemon's envelope, including the shape it uses for errors, so a
 * test cannot pass against a response the API never sends.
 */
function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400 ? { ok: false, error: data } : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
}

/**
 * The daemon registers the bootstrap routes only for
 * `server.authMode: "local-token"`; see internal/api/bootstrap_routes.go, which
 * answers 404 ROUTE_NOT_FOUND for every other mode.
 */
function bootstrapRouteAbsent(): Response {
  return response(
    {
      code: "ROUTE_NOT_FOUND",
      message: "Unknown route: /api/v1/dashboard/bootstrap/exchange",
    },
    404,
  );
}

function stubDaemon(
  exchange: () => Response,
  health: () => Response = () => response({ healthy: true }),
): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const path = String(input);
    if (path === "/api/v1/dashboard/bootstrap/exchange") return exchange();
    if (path === "/api/v1/healthz") return health();
    if (path === "/api/v1/runs/active") return response({ items: [] });
    if (path === "/api/v1/projects") return response({ items: [] });
    if (path.startsWith("/api/v1/loops")) return response({ items: [] });
    return response({ items: [] });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

describe("bootstrap code exchange", () => {
  it("loads the dashboard when the daemon has no bootstrap route", async () => {
    // A daemon in authMode "none" needs no token and serves no bootstrap
    // route, so a leftover ?code= must not strand the operator on a failure
    // screen whose only recovery step 404s for the same reason.
    window.history.replaceState({}, "", "/dashboard/?code=stale");
    stubDaemon(bootstrapRouteAbsent);

    render(<App />);

    expect(await screen.findByRole("navigation")).toBeTruthy();
    expect(screen.queryByText("Dashboard login required")).toBeNull();
    expect(sessionStorage.getItem("looper.dashboard.token")).toBeNull();
  });

  it("reports a rejected code when the daemon does serve the route", async () => {
    window.history.replaceState({}, "", "/dashboard/?code=expired");
    stubDaemon(() =>
      response({ code: "UNAUTHORIZED", message: "Invalid bootstrap code" }, 401),
    );

    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Dashboard login required" }),
    ).toBeTruthy();
    expect(screen.getByText(/Invalid bootstrap code/)).toBeTruthy();
    expect(screen.getByText(/missing or expired code/)).toBeTruthy();
    expect(screen.getByText("looper dashboard")).toBeTruthy();
    expect(screen.getByText("LOOPER_TOKEN")).toBeTruthy();
  });

  it("points an unauthenticated local-token browser to the CLI login flow", async () => {
    window.history.replaceState({}, "", "/dashboard/");
    stubDaemon(
      () => response({ token: "unused" }),
      () =>
        response(
          { code: "UNAUTHORIZED", message: "Authorization token is required" },
          401,
        ),
    );

    render(<App />);

    expect(
      await screen.findByRole("heading", { name: "Dashboard login required" }),
    ).toBeTruthy();
    expect(screen.getByText(/Authorization token is required/)).toBeTruthy();
    expect(screen.getByText("looper dashboard")).toBeTruthy();
    expect(screen.queryByRole("navigation")).toBeNull();
  });

  it("stores the session token when the exchange succeeds", async () => {
    window.history.replaceState({}, "", "/dashboard/?code=fresh");
    stubDaemon(() => response({ token: "tok_local" }));

    render(<App />);

    expect(await screen.findByRole("navigation")).toBeTruthy();
    expect(sessionStorage.getItem("looper.dashboard.token")).toBe("tok_local");
  });
});

describe("triage confirmation status", () => {
  it("shows every source the daemon projects as awaiting confirmation", async () => {
	window.history.replaceState({}, "", "/dashboard/");
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path === "/api/v1/healthz") return response({ healthy: true });
      if (path === "/api/v1/status") {
        return response({
          service: {
            healthy: true,
            triage: {
              awaitingConfirmation: {
                count: 2,
                sources: [
                  { repo: "acme/looper", issueNumber: 42, createdAt: "2026-07-30T10:30:00Z", ageSeconds: 5400, command: "/plan triage-confirm-a1" },
                  { repo: "acme/looper", issueNumber: 43, createdAt: "2026-07-30T11:45:00Z", ageSeconds: 900, command: "/plan triage-confirm-b2" },
                ],
              },
            },
          },
          scheduler: {},
          loops: {},
        });
      }
      if (path === "/api/v1/runs/active") return response({ items: [] });
      if (path === "/api/v1/projects") return response({ items: [] });
      if (path.startsWith("/api/v1/loops")) return response({ items: [] });
      return bootstrapRouteAbsent();
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<App />);

    expect(await screen.findByText("Awaiting human confirmation")).toBeTruthy();
    expect(screen.getByText("2")).toBeTruthy();
    expect(screen.getByText(/acme\/looper#42/)).toBeTruthy();
    expect(screen.getByText(/acme\/looper#43/)).toBeTruthy();
    expect(screen.getByText(/waiting 1h/)).toBeTruthy();
    expect(screen.getByText(/waiting 15m/)).toBeTruthy();
    // Without the token an operator can see the wait but cannot end it (#255).
    expect(screen.getByText("/plan triage-confirm-a1")).toBeTruthy();
    expect(screen.getByText("/plan triage-confirm-b2")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /copy/i })).toHaveLength(2);
  });
});
