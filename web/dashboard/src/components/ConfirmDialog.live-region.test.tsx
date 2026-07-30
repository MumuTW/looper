import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Shell } from "@/components/layout/Shell";
import { ToastProvider } from "@/lib/toast";

vi.mock("@/lib/DashboardDataContext", () => ({
  useDashboardData: () => ({
    health: { data: null, error: new Error("health unavailable") },
    activeRuns: { data: null, error: null },
    projects: { data: null, error: null },
    healthy: false,
  }),
}));

vi.mock("@/lib/ProjectFilterContext", () => ({
  useProjectFilter: () => ({
    projectId: "",
    projectsReady: false,
    setProjectId: vi.fn(),
  }),
}));

function DialogRoute() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open confirmation
      </button>
      <ConfirmDialog
        open={open}
        title="Confirm reload"
        onConfirm={() => setOpen(false)}
        onCancel={() => setOpen(false)}
      >
        Reload the daemon.
      </ConfirmDialog>
    </>
  );
}

afterEach(() => vi.clearAllMocks());

describe("ConfirmDialog live-region boundary", () => {
  it("keeps the real Shell background modal-hidden when its stale banner is live", () => {
    const { container, unmount } = render(
      <ToastProvider>
        <MemoryRouter>
          <Routes>
            <Route element={<Shell hostPort="127.0.0.1:17310" healthy={false} />}>
              <Route index element={<DialogRoute />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </ToastProvider>,
    );

    try {
      expect(screen.getByText(/Stale data/)).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Open confirmation" }));

      expect(container.hasAttribute("inert")).toBe(true);
      expect(container.getAttribute("aria-hidden")).toBe("true");
      expect(document.querySelector("[data-toast-container]")?.getAttribute("aria-hidden")).not.toBe("true");
    } finally {
      unmount();
    }
  });
});
