import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider, useToast } from "./toast";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function Pusher({ message }: { message: string }) {
  const toast = useToast();
  return (
    <button type="button" onClick={() => toast.info(message)}>
      push
    </button>
  );
}

describe("ToastProvider", () => {
  it("auto-dismisses a toast after the timeout", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Pusher message="hello" />
      </ToastProvider>,
    );

    act(() => {
      screen.getByText("push").click();
    });
    expect(screen.getByText("hello")).toBeDefined();

    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.queryByText("hello")).toBeNull();
  });

  it("leaves no pending timer when a toast is dismissed by hand", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Pusher message="hello" />
      </ToastProvider>,
    );

    act(() => {
      screen.getByText("push").click();
    });
    act(() => {
      screen.getByLabelText("Dismiss").click();
    });

    expect(vi.getTimerCount()).toBe(0);
  });

  // Regression: an auto-dismiss timer outliving the provider fires a state
  // setter on an unmounted tree — in vitest that lands after the jsdom
  // environment is torn down and throws "window is not defined".
  it("cancels pending auto-dismiss timers on unmount", () => {
    vi.useFakeTimers();
    const { unmount } = render(
      <ToastProvider>
        <Pusher message="hello" />
      </ToastProvider>,
    );

    act(() => {
      screen.getByText("push").click();
      screen.getByText("push").click();
    });
    expect(vi.getTimerCount()).toBe(2);

    unmount();

    expect(vi.getTimerCount()).toBe(0);
  });
});
