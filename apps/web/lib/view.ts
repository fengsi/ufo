"use client";

import { useEffect, useState } from "react";
import type { Operation } from "@/lib/types";

export const ALL_STATUSES = ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "canceled"];
export const DRAFT_SAVE_DELAY_SECONDS = 10;
const DEFAULT_VISIBLE = ["backlog", "todo", "in_progress", "in_review", "done"];
const KEY = "ufo.visibleStatuses";

// Persisted, customizable board view: which status columns are shown. blocked +
// canceled are hidden by default and live behind the "Columns" menu.
export function useVisibleStatuses() {
  const [visible, setVisible] = useState<string[]>(DEFAULT_VISIBLE);

  useEffect(() => {
    try {
      const s = localStorage.getItem(KEY);
      if (s) setVisible(JSON.parse(s));
    } catch {
      /* ignore */
    }
  }, []);

  const persist = (next: string[]) => {
    // keep canonical board order
    const ordered = ALL_STATUSES.filter((s) => next.includes(s));
    setVisible(ordered);
    try {
      localStorage.setItem(KEY, JSON.stringify(ordered));
    } catch {
      /* ignore */
    }
  };

  const toggle = (status: string) =>
    persist(visible.includes(status) ? visible.filter((s) => s !== status) : [...visible, status]);

  return { visible, toggle };
}

// Card properties that can be shown/hidden on board cards + list rows.
export const CARD_PROPS = ["priority", "description", "assignee", "dates", "mission", "labels", "subOperationProgress"] as const;
export type CardProp = (typeof CARD_PROPS)[number];
export type ViewMode = "board" | "list" | "swimlane";
export type TimeFormat = "12h" | "24h";
export type CommsOrder = "oldest_top" | "oldest_bottom";
export type AssetViewMode = "grid" | "compact_grid" | "list";

export const SORTS = ["created_desc", "created_asc", "priority", "due", "title"] as const;
export type SortKey = (typeof SORTS)[number];
export const SORT_LABEL: Record<SortKey, string> = {
  created_desc: "Newest", created_asc: "Oldest", priority: "Priority", due: "Due date", title: "Title",
};

const CP_KEY = "ufo.cardProps";
const MODE_KEY = "ufo.boardMode";
const SORT_KEY = "ufo.boardSort";
const TIME_KEY = "ufo.timeFormat";
const COMMS_ORDER_KEY = "ufo.commsOrder";
const ASSET_VIEW_KEY = "ufo.assetView";
const ASSET_PANEL_OPEN_KEY = "ufo.assetPanelOpen";
const TELEMETRY_SHOW_ALL_KEY = "ufo.telemetryShowAll";

// Order loaded operations within a column/lane. Default (created_desc) is the
// native fetch order; the rest sort the currently-loaded set client-side.
export function sortOperations(items: Operation[], sort: SortKey): Operation[] {
  const a = [...items];
  switch (sort) {
    case "created_asc":
      return a.sort((x, y) => x.created_at.localeCompare(y.created_at));
    case "priority":
      return a.sort((x, y) => y.priority - x.priority || y.created_at.localeCompare(x.created_at));
    case "due":
      return a.sort((x, y) => {
        if (!x.due_date && !y.due_date) return 0;
        if (!x.due_date) return 1;
        if (!y.due_date) return -1;
        return x.due_date.localeCompare(y.due_date);
      });
    case "title":
      return a.sort((x, y) => x.title.localeCompare(y.title));
    default:
      return a;
  }
}

// Persisted display prefs: enabled card properties + view mode + sort.
export function useBoardDisplay() {
  const [cardProps, setCardProps] = useState<Set<CardProp>>(new Set(CARD_PROPS));
  const [mode, setModeState] = useState<ViewMode>("board");
  const [sort, setSortState] = useState<SortKey>("created_desc");

  useEffect(() => {
    try {
      const c = localStorage.getItem(CP_KEY);
      if (c) setCardProps(new Set(JSON.parse(c)));
      const m = localStorage.getItem(MODE_KEY) as ViewMode | null;
      if (m === "board" || m === "list" || m === "swimlane") setModeState(m);
      const s = localStorage.getItem(SORT_KEY) as SortKey | null;
      if (s && (SORTS as readonly string[]).includes(s)) setSortState(s);
    } catch {
      /* ignore */
    }
  }, []);

  const toggleProp = (p: CardProp) => {
    setCardProps((prev) => {
      const next = new Set(prev);
      next.has(p) ? next.delete(p) : next.add(p);
      try { localStorage.setItem(CP_KEY, JSON.stringify([...next])); } catch { /* ignore */ }
      return next;
    });
  };
  const setMode = (m: ViewMode) => {
    setModeState(m);
    try { localStorage.setItem(MODE_KEY, m); } catch { /* ignore */ }
  };
  const setSort = (s: SortKey) => {
    setSortState(s);
    try { localStorage.setItem(SORT_KEY, s); } catch { /* ignore */ }
  };

  return { cardProps, toggleProp, mode, setMode, sort, setSort };
}

export function useTimeFormat() {
  const [timeFormat, setTimeFormatState] = useState<TimeFormat>("12h");
  useEffect(() => {
    const saved = localStorage.getItem(TIME_KEY);
    if (saved === "12h" || saved === "24h") setTimeFormatState(saved);
  }, []);
  const setTimeFormat = (next: TimeFormat) => {
    setTimeFormatState(next);
    try { localStorage.setItem(TIME_KEY, next); } catch { /* ignore */ }
  };
  return { timeFormat, setTimeFormat };
}

export function formatTimestamp(value: string, timeFormat: TimeFormat) {
  return new Date(value).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: timeFormat === "12h",
  });
}

export function useCommsOrder() {
  const [commsOrder, setCommsOrderState] = useState<CommsOrder>("oldest_top");
  useEffect(() => {
    const saved = localStorage.getItem(COMMS_ORDER_KEY);
    if (saved === "oldest_top" || saved === "oldest_bottom") setCommsOrderState(saved);
  }, []);
  const setCommsOrder = (next: CommsOrder) => {
    setCommsOrderState(next);
    try { localStorage.setItem(COMMS_ORDER_KEY, next); } catch { /* ignore */ }
  };
  return { commsOrder, setCommsOrder };
}

export function useAssetViewMode() {
  const [assetView, setAssetViewState] = useState<AssetViewMode>("grid");
  useEffect(() => {
    const saved = localStorage.getItem(ASSET_VIEW_KEY);
    if (saved === "grid" || saved === "compact_grid" || saved === "list") setAssetViewState(saved);
  }, []);
  const setAssetView = (next: AssetViewMode) => {
    setAssetViewState(next);
    try { localStorage.setItem(ASSET_VIEW_KEY, next); } catch { /* ignore */ }
  };
  return { assetView, setAssetView };
}

export function useAssetPanelOpen() {
  const [assetPanelOpen, setAssetPanelOpenState] = useState<boolean | null>(null);
  useEffect(() => {
    const saved = localStorage.getItem(ASSET_PANEL_OPEN_KEY);
    if (saved === "open") setAssetPanelOpenState(true);
    if (saved === "closed") setAssetPanelOpenState(false);
  }, []);
  const setAssetPanelOpen = (next: boolean) => {
    setAssetPanelOpenState(next);
    try { localStorage.setItem(ASSET_PANEL_OPEN_KEY, next ? "open" : "closed"); } catch { /* ignore */ }
  };
  return { assetPanelOpen, setAssetPanelOpen };
}

export function useTelemetryShowAll() {
  const [telemetryShowAll, setTelemetryShowAllState] = useState(false);
  useEffect(() => {
    const saved = localStorage.getItem(TELEMETRY_SHOW_ALL_KEY);
    if (saved === "true") setTelemetryShowAllState(true);
  }, []);
  const setTelemetryShowAll = (next: boolean) => {
    setTelemetryShowAllState(next);
    try { localStorage.setItem(TELEMETRY_SHOW_ALL_KEY, next ? "true" : "false"); } catch { /* ignore */ }
  };
  return { telemetryShowAll, setTelemetryShowAll };
}
