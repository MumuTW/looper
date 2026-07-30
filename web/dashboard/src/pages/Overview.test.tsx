import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@/lib/toast";
import { OverviewPage } from "./Overview";

describe("Overview page contracts", () => {
  it("renders loading state", () => {
    render(<ToastProvider><OverviewPage /></ToastProvider>);
    expect(screen.getByText(/loading/i)).toBeTruthy();
  });

  it("renders healthy state", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: {
          healthy: true,
          degraded: false,
          degradedReasons: [],
          quarantineActive: 0,
          quarantineRunning: 0,
          schedulerActive: true,
          loops: {},
          activeRuns: [],
        },
      }),
    });
    global.fetch = fetchMock;

    render(<ToastProvider><OverviewPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/healthy/i)).toBeTruthy();
    });
  });

  it("renders degraded state with reasons", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: {
          healthy: false,
          degraded: true,
          degradedReasons: ["quarantine_orphan_debt"],
          quarantineActive: 2,
          quarantineRunning: 1,
          schedulerActive: true,
          loops: {},
          activeRuns: [],
        },
      }),
    });
    global.fetch = fetchMock;

    render(<ToastProvider><OverviewPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/degraded/i)).toBeTruthy();
      expect(screen.getByText(/quarantine_orphan_debt/i)).toBeTruthy();
    });
  });

  it("renders error state", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 503,
      json: async () => ({ error: "service unavailable" }),
    });
    global.fetch = fetchMock;

    render(<ToastProvider><OverviewPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/error|unavailable/i)).toBeTruthy();
    });
  });
});
