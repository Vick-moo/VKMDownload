import { describe, expect, it } from "vitest";
import {
  partitionResumeTargets,
  resumeSkipReason,
  summarizeResume,
} from "./resumeTargets";
import type { ResumeCandidate } from "./resumeTargets";

const task = (id: string, status: string): ResumeCandidate => ({ id, status });

describe("partitionResumeTargets", () => {
  it("starts only the statuses Manager.Resume accepts", () => {
    const tasks = [
      task("a", "paused"),
      task("b", "downloading"),
      task("c", "completed"),
      task("d", "failed"),
      task("e", "canceled"),
      task("f", "queued"),
    ];
    const plan = partitionResumeTargets(
      tasks,
      tasks.map((t) => t.id),
    );
    expect(plan.started).toEqual(["a", "d", "e", "f"]);
    expect(plan.skipped).toEqual([
      { id: "b", reason: "正在下载" },
      { id: "c", reason: "已完成" },
    ]);
  });

  it("keeps the caller's selection order and does not mutate it", () => {
    const ids = ["f", "a"];
    const plan = partitionResumeTargets(
      [task("a", "paused"), task("f", "paused")],
      ids,
    );
    expect(plan.started).toEqual(["f", "a"]);
    expect(ids).toEqual(["f", "a"]);
  });

  it("reports an id missing from the list as gone, not resumable", () => {
    const plan = partitionResumeTargets([task("a", "paused")], ["a", "gone"]);
    expect(plan.started).toEqual(["a"]);
    expect(plan.skipped).toEqual([{ id: "gone", reason: "任务不存在" }]);
  });

  it("treats an unknown status as a skip rather than attempting it", () => {
    const plan = partitionResumeTargets([task("a", "mystery")], ["a"]);
    expect(plan.started).toEqual([]);
    expect(plan.skipped).toEqual([{ id: "a", reason: "状态未知" }]);
  });
});

describe("resumeSkipReason", () => {
  it("lets the four resumable states through", () => {
    for (const status of ["paused", "failed", "canceled", "queued"]) {
      expect(resumeSkipReason(status)).toBeNull();
    }
  });
  it("names the two states Resume refuses", () => {
    expect(resumeSkipReason("downloading")).toBe("正在下载");
    expect(resumeSkipReason("completed")).toBe("已完成");
  });
});

describe("summarizeResume", () => {
  it("reports both the resumed and the skipped counts", () => {
    const plan = partitionResumeTargets(
      [task("a", "paused"), task("b", "downloading"), task("c", "completed")],
      ["a", "b", "c"],
    );
    expect(summarizeResume(plan, 1, [])).toBe(
      "已继续 1 个，跳过 2 个（正在下载 1 个、已完成 1 个）",
    );
  });

  it("shows the first per-row failure instead of swallowing it", () => {
    const plan = partitionResumeTargets([task("a", "paused")], ["a"]);
    expect(
      summarizeResume(plan, 0, ["当前状态无法继续（completed）"]),
    ).toBe("失败 1 个：当前状态无法继续（completed）");
  });

  it("counts a partial failure alongside the successes", () => {
    const plan = partitionResumeTargets(
      [task("a", "paused"), task("b", "failed")],
      ["a", "b"],
    );
    expect(summarizeResume(plan, 1, ["任务不存在"])).toBe(
      "已继续 1 个，失败 1 个：任务不存在",
    );
  });

  it("says there was nothing to resume when both lists are empty", () => {
    expect(summarizeResume({ started: [], skipped: [] }, 0, [])).toBe(
      "没有可继续的任务",
    );
  });
});
