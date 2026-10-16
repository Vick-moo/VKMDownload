import { describe, expect, it } from "vitest";
import { DEFAULT_WIDTHS, MIN_WIDTHS, layoutWidths } from "./columns";

describe("layoutWidths", () => {
  const total = DEFAULT_WIDTHS.reduce((a, b) => a + b, 0);
  const floor = MIN_WIDTHS.reduce((a, b) => a + b, 0);

  it("is a no-op when prefs already fit", () => {
    expect(layoutWidths(DEFAULT_WIDTHS, total)).toEqual(DEFAULT_WIDTHS);
  });
  it("gives the surplus to the name column", () => {
    const out = layoutWidths(DEFAULT_WIDTHS, total + 100);
    expect(out[0]).toBe(DEFAULT_WIDTHS[0] + 100);
    expect(out.slice(1)).toEqual(DEFAULT_WIDTHS.slice(1));
  });
  it("shrinks proportionally but never below minimums", () => {
    const out = layoutWidths(DEFAULT_WIDTHS, total - 100);
    expect(out.reduce((a, b) => a + b, 0)).toBeCloseTo(total - 100, 6);
    out.forEach((w, i) => expect(w).toBeGreaterThanOrEqual(MIN_WIDTHS[i]));
    // The total never exceeds the container: no horizontal scrollbar.
    expect(out.reduce((a, b) => a + b, 0)).toBeLessThanOrEqual(total - 100 + 1);
  });
  it("renders prefs as-is before the first measure", () => {
    expect(layoutWidths(DEFAULT_WIDTHS, 0)).toEqual(DEFAULT_WIDTHS);
    expect(layoutWidths(DEFAULT_WIDTHS, -5)).toEqual(DEFAULT_WIDTHS);
  });
  it("clamps small-but-measured containers to the minimums", () => {
    expect(layoutWidths(DEFAULT_WIDTHS, 10)).toEqual(MIN_WIDTHS);
    expect(floor).toBeGreaterThan(0);
  });
  it("does not mutate its input", () => {
    const prefs = [...DEFAULT_WIDTHS];
    layoutWidths(prefs, total + 50);
    layoutWidths(prefs, total - 50);
    expect(prefs).toEqual(DEFAULT_WIDTHS);
  });
});
