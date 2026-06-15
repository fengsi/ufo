"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  DndContext, DragOverlay, PointerSensor, useDraggable, useDroppable, useSensor, useSensors,
  type DragEndEvent, type DragStartEvent,
} from "@dnd-kit/core";
import { Columns3, Filter, LayoutGrid, List as ListIcon, Loader2, Rows3, SlidersHorizontal } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { StatusIcon } from "@/components/status-icon";
import { PriorityIcon } from "@/components/priority-icon";
import { PilotIcon } from "@/components/pilot-icon";
import { onFire, Flames } from "@/components/fire";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Button } from "@/components/ui/button";
import { getJSON } from "@/lib/api";
import { cn } from "@/lib/utils";
import { type Operation } from "@/lib/types";
import { ALL_STATUSES, CARD_PROPS, type CardProp, SORTS, SORT_LABEL, type SortKey, sortOps, type ViewMode, useBoardDisplay, useVisibleStatuses } from "@/lib/view";
import { assigneeHasPilot, assigneeLabel, initials, opCode, PRIORITY, PRIORITY_ACCENT, LABEL_COLOR } from "@/lib/labels";
import { timeAgo } from "@/lib/timeline";

const TAB_KIND: Record<string, string> = { all: "", members: "user", pilots: "pilot" };
const CARD_PROP_LABEL: Record<CardProp, string> = {
  priority: "Priority", description: "Description", assignee: "Assignee",
  dates: "Dates", mission: "Mission", labels: "Labels", sub: "Sub-progress",
};

const LIMIT = 50;
const STATUS_LABEL: Record<string, string> = {
  backlog: "Backlog", todo: "Todo", in_progress: "In Progress",
  in_review: "In Review", done: "Done", blocked: "Blocked", cancelled: "Cancelled",
};
// Column tints match each status's icon hue (STATUS_TEXT).
const TINT: Record<string, string> = {
  backlog: "bg-muted/30", todo: "bg-muted/30", in_progress: "bg-info/5",
  in_review: "bg-warning/5", done: "bg-success/5", blocked: "bg-destructive/5", cancelled: "bg-muted/20",
};

type ColState = { items: Operation[]; cursor: string; done: boolean };

type Filters = { tab: string; priority: number | null; assignee: string; creator: string; label: string; archived: boolean };

export function Board() {
  const app = useApp();
  const { visible, toggle } = useVisibleStatuses();
  const { cardProps, toggleProp, mode, setMode, sort, setSort } = useBoardDisplay();
  const [mission, setMission] = useState("all");
  const [filters, setFilters] = useState<Filters>({ tab: "all", priority: null, assignee: "", creator: "", label: "", archived: false });
  const [cols, setCols] = useState<Record<string, ColState>>({});
  const [counts, setCounts] = useState<Record<string, number>>({});
  const [working, setWorking] = useState(0);
  const [dragId, setDragId] = useState<string | null>(null);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 6 } }));
  const missionParam = mission === "all" ? "" : mission;

  // Shared filter query string for column fetch + counts.
  const filterQS = useMemo(() => {
    let qs = `&assignee_kind=${TAB_KIND[filters.tab] ?? ""}`;
    if (filters.priority != null) qs += `&priority=${filters.priority}`;
    if (filters.assignee) qs += `&assignee=${filters.assignee}`;
    if (filters.creator) qs += `&creator=${filters.creator}`;
    if (filters.label) qs += `&label=${filters.label}`;
    if (filters.archived) qs += `&archived=1`;
    return qs;
  }, [filters]);

  const fetchColumn = useCallback(
    async (status: string, before: string): Promise<Operation[]> =>
      (await getJSON<Operation[]>(`/api/operations?fleet=${app.fleet}&status=${status}&mission=${missionParam}&before=${before}&limit=${LIMIT}${filterQS}`)) ?? [],
    [app.fleet, missionParam, filterQS],
  );

  // (Re)load visible columns' first pages + counts + working pill on changes.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      const c = await getJSON<Record<string, number>>(`/api/operations/counts?fleet=${app.fleet}&mission=${missionParam}${filterQS}`);
      if (!cancelled && c) setCounts(c);
      const wk = await getJSON<{ count: number }>(`/api/operations/working?fleet=${app.fleet}`);
      if (!cancelled && wk) setWorking(wk.count);
      const entries = await Promise.all(
        visible.map(async (s) => {
          const items = await fetchColumn(s, "");
          return [s, { items, cursor: items.at(-1)?.id ?? "", done: items.length < LIMIT }] as const;
        }),
      );
      if (!cancelled) setCols(Object.fromEntries(entries));
    })();
    return () => { cancelled = true; };
  }, [app.fleet, app.boardTick, missionParam, visible, fetchColumn, filterQS]);

  const loadMore = useCallback(async (status: string) => {
    const col = cols[status];
    if (!col || col.done) return;
    const more = await fetchColumn(status, col.cursor);
    setCols((prev) => {
      const p = prev[status];
      if (!p) return prev;
      return { ...prev, [status]: { items: [...p.items, ...more], cursor: more.at(-1)?.id ?? p.cursor, done: more.length < LIMIT } };
    });
  }, [cols, fetchColumn]);

  const onDragEnd = (e: DragEndEvent) => {
    setDragId(null);
    const opId = String(e.active.id);
    const to = e.over?.id ? String(e.over.id) : null;
    if (!to) return;
    let from: string | undefined;
    let op: Operation | undefined;
    for (const s of Object.keys(cols)) {
      const found = cols[s].items.find((o) => o.id === opId);
      if (found) { from = s; op = found; break; }
    }
    if (!op || !from || from === to) return;
    const moved = { ...op, status: to };
    const fromKey = from;
    setCols((prev) => {
      const next = { ...prev };
      next[fromKey] = { ...prev[fromKey], items: prev[fromKey].items.filter((o) => o.id !== opId) };
      next[to] = prev[to] ? { ...prev[to], items: [moved, ...prev[to].items] } : { items: [moved], cursor: moved.id, done: true };
      return next;
    });
    setCounts((c) => ({ ...c, [fromKey]: Math.max(0, (c[fromKey] ?? 1) - 1), [to]: (c[to] ?? 0) + 1 }));
    app.moveOp(opId, to);
  };

  const dragging = dragId != null ? Object.values(cols).flatMap((c) => c.items).find((o) => o.id === dragId) ?? null : null;

  // Display order (drag/pagination still operate on the raw `cols` by id).
  const view = useMemo(() => {
    const out: Record<string, ColState> = {};
    for (const s of Object.keys(cols)) out[s] = { ...cols[s], items: sortOps(cols[s].items, sort) };
    return out;
  }, [cols, sort]);

  const header = (
    <div className="flex items-center gap-2 px-4 pt-3">
      {/* assignee quick-tabs */}
      <div className="flex rounded-lg border border-border p-0.5">
        {["all", "members", "pilots"].map((t) => (
          <button
            key={t}
            onClick={() => setFilters((f) => ({ ...f, tab: t }))}
            className={cn("rounded-md px-2.5 py-1 text-xs capitalize", filters.tab === t ? "bg-accent font-medium" : "text-muted-foreground")}
          >
            {t}
          </button>
        ))}
      </div>
      <Select value={mission} onValueChange={setMission}>
        <SelectTrigger className="h-8 w-48 text-xs"><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem value="all">All missions</SelectItem>
          {app.missions.map((m) => (
            <SelectItem key={m.id} value={String(m.id)}><span className="font-mono text-xs">{m.key}</span> · {m.name}</SelectItem>
          ))}
        </SelectContent>
      </Select>

      <div className="ml-auto flex items-center gap-2">
        {working > 0 && <span className="rounded-full bg-info/10 px-2 py-1 text-xs font-medium text-info">{working} working</span>}
        <FilterMenu filters={filters} setFilters={setFilters} />
        <DisplayMenu cardProps={cardProps} toggleProp={toggleProp} mode={mode} setMode={setMode} sort={sort} setSort={setSort} />
        <Popover>
          <PopoverTrigger asChild><Button variant="outline" size="sm"><Columns3 /> Columns</Button></PopoverTrigger>
          <PopoverContent align="end" className="w-52 space-y-1 text-xs">
            {ALL_STATUSES.map((s) => (
              <label key={s} className="flex cursor-pointer items-center gap-2 py-1">
                <input type="checkbox" checked={visible.includes(s)} onChange={() => toggle(s)} />
                <StatusIcon status={s} /> <span className="flex-1">{STATUS_LABEL[s]}</span>
                <span className="text-muted-foreground">{counts[s] ?? 0}</span>
              </label>
            ))}
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );

  if (mode === "list") {
    return (
      <div className="bg-board flex h-full flex-col">
        {header}
        <div className="flex-1 overflow-y-auto p-4 pt-3">
          {visible.map((s) => <ListSection key={s} status={s} count={counts[s] ?? 0} col={view[s]} cardProps={cardProps} onLoadMore={() => loadMore(s)} />)}
        </div>
      </div>
    );
  }

  if (mode === "swimlane") {
    return (
      <div className="bg-board flex h-full flex-col">
        {header}
        <Swimlane visible={visible} cols={view} cardProps={cardProps} />
      </div>
    );
  }

  return (
    <DndContext sensors={sensors} onDragStart={(e: DragStartEvent) => setDragId(String(e.active.id))} onDragEnd={onDragEnd} onDragCancel={() => setDragId(null)}>
      <div className="bg-board flex h-full flex-col">
        {header}
        <div className="flex flex-1 gap-3 overflow-x-auto p-4 pt-3">
          {visible.map((s) => (
            <Column key={s} status={s} count={counts[s] ?? 0} col={view[s]} cardProps={cardProps} onLoadMore={() => loadMore(s)} />
          ))}
        </div>
      </div>
      <DragOverlay>{dragging ? <CardBody op={dragging} cardProps={cardProps} dragging /> : null}</DragOverlay>
    </DndContext>
  );
}

function FilterMenu({ filters, setFilters }: { filters: Filters; setFilters: React.Dispatch<React.SetStateAction<Filters>> }) {
  const app = useApp();
  const active = (filters.priority != null ? 1 : 0) + (filters.assignee ? 1 : 0) + (filters.creator ? 1 : 0) + (filters.label ? 1 : 0);
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm"><Filter /> Filter{active > 0 && <span className="ml-1 rounded-full bg-brand/15 px-1.5 text-[10px] text-brand">{active}</span>}</Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-56 space-y-2 text-xs">
        <FilterSelect label="Priority" value={filters.priority == null ? "any" : String(filters.priority)} onChange={(v) => setFilters((f) => ({ ...f, priority: v === "any" ? null : Number(v) }))}
          options={[{ v: "any", l: "Any" }, ...PRIORITY.map((p, i) => ({ v: String(i), l: p.label }))]} />
        <FilterSelect label="Assignee" value={filters.assignee || "any"} onChange={(v) => setFilters((f) => ({ ...f, assignee: v === "any" ? "" : v }))}
          options={[
            { v: "any", l: "Any" },
            ...app.members.map((m) => ({ v: m.id, l: m.name || m.email })),
            ...app.pilots.map((a) => ({ v: a.id, l: <span className="flex items-center gap-2"><PilotIcon kind={a.kind} /> {a.name}</span> })),
            ...app.crews.map((c) => ({ v: c.id, l: `👥 ${c.name}` })),
          ]} />
        <FilterSelect label="Creator" value={filters.creator || "any"} onChange={(v) => setFilters((f) => ({ ...f, creator: v === "any" ? "" : v }))}
          options={[{ v: "any", l: "Any" }, ...app.members.map((m) => ({ v: m.id, l: m.name || m.email }))]} />
        <FilterSelect label="Label" value={filters.label || "any"} onChange={(v) => setFilters((f) => ({ ...f, label: v === "any" ? "" : v }))}
          options={[{ v: "any", l: "Any" }, ...app.labels.map((l) => ({ v: l.id, l: l.name }))]} />
        <label className="flex cursor-pointer items-center justify-between gap-2 pt-1">
          <span className="text-muted-foreground">Show archived</span>
          <input type="checkbox" checked={filters.archived} onChange={(e) => setFilters((f) => ({ ...f, archived: e.target.checked }))} />
        </label>
        {active > 0 && <Button variant="ghost" size="sm" className="w-full" onClick={() => setFilters((f) => ({ tab: f.tab, priority: null, assignee: "", creator: "", label: "", archived: false }))}>Clear filters</Button>}
      </PopoverContent>
    </Popover>
  );
}

function FilterSelect({ label, value, onChange, options }: { label: string; value: string; onChange: (v: string) => void; options: { v: string; l: React.ReactNode }[] }) {
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="text-muted-foreground">{label}</span>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger className="h-7 w-32 text-xs"><SelectValue /></SelectTrigger>
        <SelectContent>{options.map((o) => <SelectItem key={o.v || "any"} value={o.v}>{o.l}</SelectItem>)}</SelectContent>
      </Select>
    </div>
  );
}

function DisplayMenu({ cardProps, toggleProp, mode, setMode, sort, setSort }: { cardProps: Set<CardProp>; toggleProp: (p: CardProp) => void; mode: ViewMode; setMode: (m: ViewMode) => void; sort: SortKey; setSort: (s: SortKey) => void }) {
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm"><SlidersHorizontal /> Display</Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-52 space-y-3 text-xs">
        <div>
          <p className="mb-1.5 font-medium text-muted-foreground">View</p>
          <div className="grid grid-cols-3 gap-1">
            <Button variant={mode === "board" ? "secondary" : "ghost"} size="sm" onClick={() => setMode("board")}><LayoutGrid /> Board</Button>
            <Button variant={mode === "list" ? "secondary" : "ghost"} size="sm" onClick={() => setMode("list")}><ListIcon /> List</Button>
            <Button variant={mode === "swimlane" ? "secondary" : "ghost"} size="sm" onClick={() => setMode("swimlane")}><Rows3 /> Lanes</Button>
          </div>
        </div>
        <div className="flex items-center justify-between gap-2">
          <span className="font-medium text-muted-foreground">Sort by</span>
          <Select value={sort} onValueChange={(v) => setSort(v as SortKey)}>
            <SelectTrigger className="h-7 w-32 text-xs"><SelectValue /></SelectTrigger>
            <SelectContent>{SORTS.map((s) => <SelectItem key={s} value={s}>{SORT_LABEL[s]}</SelectItem>)}</SelectContent>
          </Select>
        </div>
        <div>
          <p className="mb-1.5 font-medium text-muted-foreground">Card properties</p>
          {CARD_PROPS.map((p) => (
            <label key={p} className="flex cursor-pointer items-center justify-between py-1">
              <span>{CARD_PROP_LABEL[p]}</span>
              <input type="checkbox" checked={cardProps.has(p)} onChange={() => toggleProp(p)} />
            </label>
          ))}
        </div>
      </PopoverContent>
    </Popover>
  );
}

// Swimlane: lanes = missions, columns = statuses. Groups the already-fetched per-status
// pages by mission (first page per column — deep pagination lives in Board view).
function Swimlane({ visible, cols, cardProps }: { visible: string[]; cols: Record<string, ColState>; cardProps: Set<CardProp> }) {
  const app = useApp();
  // Missions present in the fetched data, in board order.
  const missionIds = useMemo(() => {
    const seen = new Set<string>();
    for (const s of visible) for (const op of cols[s]?.items ?? []) if (op.mission_id) seen.add(op.mission_id);
    return app.missions.filter((m) => seen.has(m.id)).map((m) => m.id);
  }, [visible, cols, app.missions]);

  return (
    <div className="flex-1 space-y-5 overflow-auto p-4 pt-3">
      {missionIds.length === 0 && <p className="text-sm text-muted-foreground">No operations.</p>}
      {missionIds.map((mid) => {
        const mission = app.missions.find((m) => m.id === mid);
        return (
          <div key={mid}>
            <div className="mb-1.5 flex items-center gap-2 text-sm font-medium">
              <span className="font-mono text-xs text-muted-foreground">{mission?.key}</span> {mission?.name}
            </div>
            <div className="flex gap-3 overflow-x-auto">
              {visible.map((s) => {
                const items = (cols[s]?.items ?? []).filter((o) => o.mission_id === mid);
                return (
                  <div key={s} className="w-64 shrink-0">
                    <div className="mb-1 flex items-center gap-2 px-1 text-xs text-muted-foreground">
                      <StatusIcon status={s} /> {STATUS_LABEL[s]} <span>{items.length}</span>
                    </div>
                    <div className={cn("flex flex-col gap-2 rounded-xl border border-border p-2 shadow-sm", TINT[s] ?? "bg-muted/30")}>
                      {items.map((op) => (
                        <div key={op.id} onClick={() => app.openOp(op.id)} className="cursor-pointer">
                          <CardBody op={op} cardProps={cardProps} />
                        </div>
                      ))}
                      {items.length === 0 && <p className="py-3 text-center text-[11px] text-muted-foreground/60">—</p>}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function ListSection({ status, count, col, cardProps, onLoadMore }: { status: string; count: number; col?: ColState; cardProps: Set<CardProp>; onLoadMore: () => void }) {
  const app = useApp();
  const sentinel = useRef<HTMLDivElement>(null);
  const items = col?.items ?? [];
  const done = col?.done ?? true;
  useEffect(() => {
    const el = sentinel.current;
    if (!el || done) return;
    const io = new IntersectionObserver((es) => { if (es[0].isIntersecting) onLoadMore(); }, { rootMargin: "200px" });
    io.observe(el);
    return () => io.disconnect();
  }, [done, onLoadMore]);
  if (items.length === 0) return null;
  return (
    <div className="mb-4">
      <div className="mb-1 flex items-center gap-2 px-1 text-sm font-medium">
        <StatusIcon status={status} /> {STATUS_LABEL[status]} <span className="text-xs text-muted-foreground">{count}</span>
      </div>
      <div className="divide-y divide-border rounded-lg border border-border bg-card shadow-sm">
        {items.map((op) => {
          const fire = onFire(op);
          return (
          <button key={op.id} onClick={() => app.openOp(op.id)} className={cn("relative flex w-full items-center gap-2 px-3 py-2 text-left text-sm hover:bg-accent/40", fire && "border-l-2 border-l-destructive")}>
            {cardProps.has("priority") && op.priority > 0 && <PriorityIcon level={op.priority} className="size-3.5 shrink-0" />}
            <span className="font-mono text-[10px] text-muted-foreground">{opCode(op, app.missions)}</span>
            <span className="truncate">{op.title}</span>
            {cardProps.has("labels") && op.labels?.map((l) => <span key={l.id} className={cn("rounded-full px-1.5 text-[10px]", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)}>{l.name}</span>)}
            {cardProps.has("sub") && op.sub?.total > 0 && <span className="text-[10px] text-muted-foreground">☑ {op.sub.done}/{op.sub.total}</span>}
            <span className="ml-auto shrink-0 text-[11px] text-muted-foreground">{timeAgo(op.created_at)}</span>
            {fire && <Flames seed={op.id} />}
          </button>
          );
        })}
        {!done && <div ref={sentinel} className="flex justify-center py-2 text-muted-foreground"><Loader2 className="size-4 animate-spin" /></div>}
      </div>
    </div>
  );
}

function Column({ status, count, col, cardProps, onLoadMore }: { status: string; count: number; col?: ColState; cardProps: Set<CardProp>; onLoadMore: () => void }) {
  const { setNodeRef, isOver } = useDroppable({ id: status });
  const sentinel = useRef<HTMLDivElement>(null);
  const items = col?.items ?? [];
  const done = col?.done ?? true;

  useEffect(() => {
    const el = sentinel.current;
    if (!el || done) return;
    const io = new IntersectionObserver((es) => { if (es[0].isIntersecting) onLoadMore(); }, { rootMargin: "200px" });
    io.observe(el);
    return () => io.disconnect();
  }, [done, onLoadMore]);

  return (
    <div className="flex w-64 shrink-0 flex-col">
      <div className="mb-1 flex items-center gap-2 px-1 text-xs text-muted-foreground">
        <StatusIcon status={status} /> {STATUS_LABEL[status]} <span>{count}</span>
      </div>
      <div
        ref={setNodeRef}
        className={cn("flex flex-1 flex-col gap-2 overflow-y-auto rounded-xl border border-border p-2 shadow-sm transition-colors", TINT[status] ?? "bg-muted/30", isOver && "ring-2 ring-inset ring-brand/50")}
      >
        {items.map((op) => <Card key={op.id} op={op} cardProps={cardProps} />)}
        {items.length === 0 && <p className="pt-6 text-center text-xs text-muted-foreground/70">No operations</p>}
        {!done && <div ref={sentinel} className="flex justify-center py-2 text-muted-foreground"><Loader2 className="size-4 animate-spin" /></div>}
      </div>
    </div>
  );
}

function Card({ op, cardProps }: { op: Operation; cardProps: Set<CardProp> }) {
  const app = useApp();
  const { attributes, listeners, setNodeRef, isDragging } = useDraggable({ id: op.id });
  return (
    <div
      ref={setNodeRef}
      {...listeners}
      {...attributes}
      onClick={() => app.openOp(op.id)}
      className={cn("cursor-grab active:cursor-grabbing", isDragging && "opacity-40")}
    >
      <CardBody op={op} cardProps={cardProps} />
    </div>
  );
}

const ALL_PROPS = new Set(CARD_PROPS);

function CardBody({ op, cardProps = ALL_PROPS, dragging }: { op: Operation; cardProps?: Set<CardProp>; dragging?: boolean }) {
  const app = useApp();
  const name = assigneeLabel(op, app.user, app.pilots, app.crews, app.members);
  const pilot = op.assignee_type === "pilot" ? app.pilots.find((a) => a.id === op.assignee_id) : null;
  const pilotBacked = assigneeHasPilot(op, app.crews);
  const selected = app.selectedOp === op.id;
  const preview = op.body.split("\n").find((l) => l.trim()) ?? "";
  const show = (p: CardProp) => cardProps.has(p);
  const fire = onFire(op);
  return (
    <div
      className={cn(
        "relative rounded-lg border border-border bg-card p-3 shadow-sm transition-colors hover:border-brand/50",
        op.priority > 0 && cn("border-l-2", PRIORITY_ACCENT[op.priority]),
        selected && "border-brand ring-1 ring-brand/30",
        dragging && "rotate-2 shadow-lg",
      )}
    >
      <div className="flex items-center justify-between">
        <span className="flex items-center gap-1.5">
          {show("priority") && op.priority > 0 && <PriorityIcon level={op.priority} className="size-3.5" />}
          <span className="font-mono text-[11px] font-medium uppercase text-muted-foreground">{opCode(op, app.missions)}</span>
        </span>
        <span className="flex items-center gap-1.5">
          {op.status === "in_progress" && (
            <span className="flex items-center gap-1 text-[11px] font-medium text-info"><Loader2 className="size-3 animate-spin" /> Working</span>
          )}
        </span>
      </div>
      <p className="mt-1 text-sm font-medium leading-snug">{op.title}</p>
      {show("description") && preview && <p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{preview}</p>}
      {show("labels") && op.labels?.length > 0 && (
        <div className="mt-1.5 flex flex-wrap gap-1">
          {op.labels.map((l) => (
            <span key={l.id} className={cn("rounded-full px-1.5 py-0.5 text-[10px]", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)}>{l.name}</span>
          ))}
        </div>
      )}
      <div className="mt-2.5 flex items-center justify-between">
        {show("sub") && op.sub?.total > 0 && <span className="text-[10px] text-muted-foreground">☑ {op.sub.done}/{op.sub.total}</span>}
        {show("assignee") && (op.assignee_type ? (
          <div className="flex items-center gap-1.5">
            <Avatar className="size-5">
              <AvatarFallback className={cn("text-[9px]", pilotBacked && "bg-brand/15 text-brand")}>
                {pilot ? <PilotIcon kind={pilot.kind} size={12} /> : initials(name)}
              </AvatarFallback>
            </Avatar>
            <span className="text-xs text-muted-foreground">{name}</span>
          </div>
        ) : (
          <span className="text-xs text-muted-foreground/60">Unassigned</span>
        ))}
        {show("dates") && op.due_date && <span className="text-[10px] text-muted-foreground">due {op.due_date.slice(5)}</span>}
        <span className="text-[11px] text-muted-foreground/80">{timeAgo(op.created_at)}</span>
      </div>
      {fire && <Flames seed={op.id} />}
    </div>
  );
}
