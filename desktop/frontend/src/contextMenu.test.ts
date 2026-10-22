import { afterEach, describe, expect, it, vi } from "vitest";
import {
  clampMenuPosition,
  contextActions,
  DEFAULT_SORT,
  parseStoredFilter,
  parseStoredSidebarCollapsed,
  parseStoredSort,
  readStored,
  selectForContextMenu,
  STORAGE_KEYS,
  writeStored,
} from "./contextMenu";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("selectForContextMenu", () => {
  it("adds an unselected row while keeping the other rows", () => {
    const next = selectForContextMenu(new Set(["a", "b"]), "c");
    expect([...next]).toEqual(["a", "b", "c"]);
  });

  it("leaves an already selected row untouched", () => {
    const selected = new Set(["a", "b"]);
    expect(selectForContextMenu(selected, "a")).toBe(selected);
    expect([...selectForContextMenu(selected, "a")]).toEqual(["a", "b"]);
  });

  it("starts a selection from an empty set", () => {
    expect([...selectForContextMenu(new Set(), "a")]).toEqual(["a"]);
  });

  it("does not mutate the set it was given", () => {
    const selected = new Set(["a"]);
    selectForContextMenu(selected, "b");
    expect([...selected]).toEqual(["a"]);
  });
});

describe("clampMenuPosition", () => {
  it("keeps a menu that fits exactly where the pointer is", () => {
    expect(clampMenuPosition(100, 120, 180, 190, 1000, 800)).toEqual({
      left: 100,
      top: 120,
    });
  });

  it("pulls the menu back from the right and bottom edges", () => {
    expect(clampMenuPosition(950, 780, 180, 190, 1000, 800)).toEqual({
      left: 812,
      top: 602,
    });
  });

  it("keeps the default margin at the top-left edge", () => {
    expect(clampMenuPosition(-20, -10, 180, 190, 1000, 800)).toEqual({
      left: 8,
      top: 8,
    });
  });

  it("pins an oversized menu to the margin", () => {
    expect(clampMenuPosition(400, 300, 1200, 900, 1000, 800)).toEqual({
      left: 8,
      top: 8,
    });
  });
});

describe("parseStoredSort", () => {
  it("falls back to added/desc for missing or corrupt entries", () => {
    expect(parseStoredSort(null)).toEqual(DEFAULT_SORT);
    expect(parseStoredSort("")).toEqual(DEFAULT_SORT);
    expect(parseStoredSort("{not json")).toEqual(DEFAULT_SORT);
    expect(parseStoredSort("null")).toEqual(DEFAULT_SORT);
    expect(parseStoredSort("[]")).toEqual(DEFAULT_SORT);
  });

  it("accepts a complete, known sort", () => {
    expect(parseStoredSort(JSON.stringify({ key: "name", dir: "asc" })))
      .toEqual({ key: "name", dir: "asc" });
  });

  it("rejects an unknown field or direction", () => {
    expect(parseStoredSort(JSON.stringify({ key: "bogus", dir: "asc" })))
      .toEqual(DEFAULT_SORT);
    expect(parseStoredSort(JSON.stringify({ key: "size", dir: "sideways" })))
      .toEqual(DEFAULT_SORT);
    expect(parseStoredSort(JSON.stringify({ key: "size" }))).toEqual(
      DEFAULT_SORT,
    );
  });

  it("returns a fresh default rather than a shared object", () => {
    const a = parseStoredSort(null);
    a.key = "name";
    expect(parseStoredSort(null)).toEqual(DEFAULT_SORT);
  });
});

describe("parseStoredFilter", () => {
  it("falls back to all for missing or invalid entries", () => {
    expect(parseStoredFilter(null)).toBe("all");
    expect(parseStoredFilter("")).toBe("all");
    expect(parseStoredFilter("nonsense")).toBe("all");
    expect(parseStoredFilter("ext:")).toBe("all");
    expect(parseStoredFilter("ext:two words")).toBe("all");
  });

  it("accepts the status views and a concrete extension", () => {
    for (const f of ["all", "active", "completed", "failed", "canceled"]) {
      expect(parseStoredFilter(f)).toBe(f);
    }
    expect(parseStoredFilter("ext:pdf")).toBe("ext:pdf");
    expect(parseStoredFilter("ext:7z")).toBe("ext:7z");
  });
});

describe("parseStoredSidebarCollapsed", () => {
  it("only treats the exact true marker as collapsed", () => {
    expect(parseStoredSidebarCollapsed("true")).toBe(true);
    expect(parseStoredSidebarCollapsed("false")).toBe(false);
    expect(parseStoredSidebarCollapsed("1")).toBe(false);
    expect(parseStoredSidebarCollapsed(null)).toBe(false);
  });
});

describe("contextActions", () => {
  it("labels a queued row 开始 and the other resumable states 继续", () => {
    expect(contextActions("queued").resumeLabel).toBe("开始");
    for (const status of ["paused", "failed", "canceled"]) {
      expect(contextActions(status).resumeLabel).toBe("继续");
    }
  });

  it("enables resume only for the states Manager.Resume accepts", () => {
    for (const status of ["paused", "failed", "canceled", "queued"]) {
      expect(contextActions(status).canResume).toBe(true);
    }
    for (const status of ["downloading", "completed", "mystery"]) {
      expect(contextActions(status).canResume).toBe(false);
    }
  });

  it("enables pause only while transferring or queued", () => {
    expect(contextActions("downloading").canPause).toBe(true);
    expect(contextActions("queued").canPause).toBe(true);
    for (const status of ["paused", "failed", "canceled", "completed"]) {
      expect(contextActions(status).canPause).toBe(false);
    }
  });
});

describe("STORAGE_KEYS", () => {
  it("uses a distinct vkm.* key per stored view setting", () => {
    expect(new Set(Object.values(STORAGE_KEYS)).size).toBe(
      Object.values(STORAGE_KEYS).length,
    );
    for (const key of Object.values(STORAGE_KEYS)) {
      expect(key.startsWith("vkm.")).toBe(true);
    }
  });
});

describe("readStored / writeStored", () => {
  it("round-trips a value through localStorage", () => {
    const store = new Map<string, string>();
    vi.stubGlobal("localStorage", {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => {
        store.set(key, value);
      },
    });
    writeStored("k", "v");
    expect(readStored("k")).toBe("v");
  });

  it("treats unavailable storage as absent instead of throwing", () => {
    vi.stubGlobal("localStorage", {
      getItem: () => {
        throw new Error("storage disabled");
      },
      setItem: () => {
        throw new Error("quota exceeded");
      },
    });
    expect(readStored("k")).toBeNull();
    expect(() => writeStored("k", "v")).not.toThrow();
  });
});
