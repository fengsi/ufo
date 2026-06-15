"use client";

import { useEffect, useState } from "react";
import type { Operation } from "@/lib/types";

export const ALL_STATUSES = ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled"];
const DEFAULT_VISIBLE = ["backlog", "todo", "in_progress", "in_review", "done"];
const KEY = "ufo.visibleStatuses";

// Persisted, customizable board view: which status columns are shown. blocked +
// cancelled are hidden by default and live behind the "Columns" menu.
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
export const CARD_PROPS = ["priority", "description", "assignee", "dates", "mission", "labels", "sub"] as const;
export type CardProp = (typeof CARD_PROPS)[number];
export type ViewMode = "board" | "list" | "swimlane";

export const SORTS = ["created_desc", "created_asc", "priority", "due", "title"] as const;
export type SortKey = (typeof SORTS)[number];
export const SORT_LABEL: Record<SortKey, string> = {
  created_desc: "Newest", created_asc: "Oldest", priority: "Priority", due: "Due date", title: "Title",
};

const CP_KEY = "ufo.cardProps";
const MODE_KEY = "ufo.boardMode";
const SORT_KEY = "ufo.boardSort";

// Order loaded operations within a column/lane. Default (created_desc) is the
// native fetch order; the rest sort the currently-loaded set client-side.
export function sortOps(items: Operation[], sort: SortKey): Operation[] {
  const a = [...items];
  switch (sort) {
    case "created_asc":
      return a.sort((x, y) => x.created_at.localeCompare(y.created_at));
    case "priority":
      return a.sort((x, y) => y.priority - x.priority || y.created_at.localeCompare(x.created_at));
    case "due":
      return a.sort((x, y) => {
        if (!x.due_date && !y.due_date) return 0;
        if (!x.due_date) return 1; // nulls last
        if (!y.due_date) return -1;
        return x.due_date.localeCompare(y.due_date);
      });
    case "title":
      return a.sort((x, y) => x.title.localeCompare(y.title));
    default:
      return a; // created_desc = as fetched (id desc)
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
