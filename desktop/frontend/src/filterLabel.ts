import { filterExt } from "./types";
import type { Filter } from "./types";

/**
 * Names a filter for the bulk-delete button and its confirmation.
 * Extracted verbatim from `App.tsx` so it can be unit-tested.
 */
export function filterLabel(filter: Filter): string {
  switch (filter) {
    case "all":
      return "全部";
    case "active":
      return "下载中";
    case "completed":
      return "已完成";
    case "failed":
      return "失败";
    case "canceled":
      return "已取消";
    default: {
      const ext = filterExt(filter);
      return ext ? `${ext} 文件` : "当前列表";
    }
  }
}
