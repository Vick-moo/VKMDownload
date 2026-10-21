/**
 * Column-width math for the task list, extracted verbatim from
 * `components/TaskList.tsx` so it can be unit-tested.
 *
 * These are *preferences*, not fixed sizes: when they do not fit the available
 * width every column shrinks in proportion down to MIN_WIDTHS, and when there is
 * room to spare the name column takes it. That is what keeps the list free of a
 * horizontal scrollbar at any window size.
 */

/** Preferred column widths in pixels, COLUMNS order plus the action track. */
export const DEFAULT_WIDTHS = [280, 76, 124, 132, 120, 140];

/** Smallest a column may be. */
export const MIN_WIDTHS = [140, 62, 104, 110, 96, 112];

/**
 * Returns the widths to render, in column order.
 *
 * `prefs` is kept balanced against the available width, so in the steady state
 * this returns it unchanged. The two branches below are the exceptions: the
 * window was resized past what the preferences fit, or a drag has pushed the
 * total over the container.
 */
export function layoutWidths(
  preference: number[],
  available: number,
  mins: readonly number[] = MIN_WIDTHS,
): number[] {
  // Before the first measure there is no container width yet. Falling back
  // to a floor here would squeeze every column to its minimum for one
  // frame; rendering the preferences as-is avoids that flicker.
  if (!(available > 0)) return [...preference];
  const floor = mins.reduce((a, b) => a + b, 0);
  const space = Math.max(available, floor);
  const out = [...preference];
  const total = out.reduce((a, b) => a + b, 0);

  if (total === space) return out;

  if (total < space) {
    // Room to spare: the name column takes it, since it carries the most text
    // and shrinking it is the last thing anyone wants.
    out[0] += space - total;
    return out;
  }

  // Too wide: everything shrinks in proportion, but never below its minimum.
  const need = total - space;
  const shrinkable = out.reduce((sum, w, i) => sum + (w - mins[i]), 0);
  if (shrinkable <= 0) return [...mins];
  const share = Math.min(need, shrinkable);
  for (let i = 0; i < out.length; i += 1) {
    const room = out[i] - mins[i];
    if (room > 0) out[i] -= (share * room) / shrinkable;
  }
  return out;
}
