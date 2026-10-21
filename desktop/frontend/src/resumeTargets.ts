/**
 * Planning for the toolbar's bulk "继续" action.
 *
 * Manager.Resume only accepts paused, failed, canceled and queued tasks; it
 * rejects a downloading or completed one by design. The toolbar used to fire
 * resume at every selected id and swallow the per-row error, so a selection
 * holding an in-flight or finished download produced a bare "继续失败" with no
 * explanation. The gate lives here, as pure functions, so the notice can say
 * what was skipped and why — and so the rules are testable without a bridge.
 */

/** The part of a task the resume plan needs. */
export interface ResumeCandidate {
  id: string;
  /** TaskStatus on the wire, kept as a string so a new backend state cannot
   *  be mistaken for a resumable one at compile time. */
  status: string;
}

export interface ResumeSkip {
  id: string;
  reason: string;
}

export interface ResumePlan {
  /** Ids that api.resumeTask should be called on, in selection order. */
  started: string[];
  /** Ids deliberately left alone, each with a user-facing reason. */
  skipped: ResumeSkip[];
}

/**
 * Returns why a bulk resume must skip this status, or null when Resume accepts
 * it. Mirrors the switch in Manager.Resume (manager.go).
 */
export function resumeSkipReason(status: string): string | null {
  switch (status) {
    case "paused":
    case "failed":
    case "canceled":
    case "queued":
      return null;
    case "downloading":
      return "正在下载";
    case "completed":
      return "已完成";
    default:
      return "状态未知";
  }
}

/**
 * Splits a selection into the ids worth resuming and the ones the backend
 * would refuse anyway.
 *
 * Statuses come from the list already on screen rather than from an extra
 * round-trip: the same array drives every row, and a stale entry is still
 * caught by the per-id error handling in the caller.
 */
export function partitionResumeTargets(
  tasks: ReadonlyArray<ResumeCandidate>,
  ids: readonly string[],
): ResumePlan {
  const byId = new Map(tasks.map((t) => [t.id, t]));
  const plan: ResumePlan = { started: [], skipped: [] };
  for (const id of ids) {
    const task = byId.get(id);
    if (!task) {
      // The selection is pruned against the list, but a row can vanish
      // between the click and this call (removed on the other side).
      plan.skipped.push({ id, reason: "任务不存在" });
      continue;
    }
    const reason = resumeSkipReason(task.status);
    if (reason) plan.skipped.push({ id, reason });
    else plan.started.push(id);
  }
  return plan;
}

/** Groups skipped ids by reason: "正在下载 2 个、已完成 1 个". */
function reasonSummary(skipped: readonly ResumeSkip[]): string {
  const counts = new Map<string, number>();
  for (const s of skipped) {
    counts.set(s.reason, (counts.get(s.reason) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([reason, n]) => `${reason} ${n} 个`)
    .join("、");
}

/**
 * Builds the notice line for a bulk resume.
 *
 * `started` is how many of plan.started actually went through; `failures`
 * holds one message per id that did not, so the first real reason is shown
 * instead of being swallowed.
 */
export function summarizeResume(
  plan: ResumePlan,
  started: number,
  failures: readonly string[],
): string {
  const parts: string[] = [];
  if (started > 0) parts.push(`已继续 ${started} 个`);
  if (plan.skipped.length > 0) {
    parts.push(
      `跳过 ${plan.skipped.length} 个（${reasonSummary(plan.skipped)}）`,
    );
  }
  if (failures.length > 0) {
    parts.push(`失败 ${failures.length} 个：${failures[0]}`);
  }
  if (parts.length === 0) return "没有可继续的任务";
  return parts.join("，");
}
