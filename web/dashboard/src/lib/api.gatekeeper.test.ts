import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchGatekeeperAgreements } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchGatekeeperAgreements", () => {
  it("requests the optional project filter and limit through the daemon route", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      expect(String(input)).toBe(
        "/api/v1/gatekeeper/agreements?projectId=project_a&limit=20",
      );
      return new Response(JSON.stringify({ ok: true, data: { items: [] } }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      fetchGatekeeperAgreements({ projectId: "project_a", limit: 20 }),
    ).resolves.toEqual({ items: [] });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
