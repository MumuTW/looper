import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@/lib/toast";
import { LoopsPage } from "./Loops";

describe("Loops page contracts", () => {
  it("renders loading state", () => {
    render(<ToastProvider><LoopsPage /></ToastProvider>);
    expect(screen.getByText(/loading/i)).toBeTruthy();
  });

  it("renders populated state", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: {
          items: [
            {
              id: "loop_1",
              seq: 1,
              projectId: "project_1",
              type: "fixer",
              status: "running",
            },
          ],
          totalCount: 1,
          limit: 25,
          offset: 0,
        },
      }),
    });
    global.fetch = fetchMock;

    render(<ToastProvider><LoopsPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText("loop_1")).toBeTruthy();
    });
  });

  it("renders empty state", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ data: { items: [], totalCount: 0, limit: 25, offset: 0 } }),
    });
    global.fetch = fetchMock;

    render(<ToastProvider><LoopsPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/no loops/i)).toBeTruthy();
    });
  });
});
