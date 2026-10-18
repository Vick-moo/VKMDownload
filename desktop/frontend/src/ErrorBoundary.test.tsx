// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ErrorBoundary } from "./ErrorBoundary";

/** A child that always throws, standing in for a render bug anywhere in App. */
function Boom(): never {
  throw new Error("boom");
}

/**
 * The boundary exists so a render error anywhere in the tree does not leave an
 * empty window. This renders one for real (jsdom) and asserts the fallback,
 * rather than only testing pure functions.
 */
describe("ErrorBoundary", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.appendChild(container);
    // React logs every caught render error; the fallback is what is under
    // test, not the log line.
    vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    container.remove();
    vi.restoreAllMocks();
  });

  it("shows the fallback instead of a white window when a child throws", async () => {
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <ErrorBoundary>
          <Boom />
        </ErrorBoundary>,
      );
    });
    expect(container.textContent).toContain("界面出错了");
    expect(container.textContent).toContain("boom");
    await act(async () => {
      root.unmount();
    });
  });

  it("renders its children unchanged when nothing throws", async () => {
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <ErrorBoundary>
          <span>正常内容</span>
        </ErrorBoundary>,
      );
    });
    expect(container.textContent).toContain("正常内容");
    expect(container.textContent).not.toContain("界面出错了");
    await act(async () => {
      root.unmount();
    });
  });
});
