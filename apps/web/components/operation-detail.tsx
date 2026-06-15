"use client";

import { useEffect, useState } from "react";
import { Archive, ArchiveRestore, ArrowLeft, GitPullRequest, Link2, Loader2, MessageCircleQuestion, Play, Plus, ScrollText, SmilePlus, Terminal, X } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { StatusIcon } from "@/components/status-icon";
import { PriorityIcon } from "@/components/priority-icon";
import { PilotIcon } from "@/components/pilot-icon";
import { onFire, DetailFire } from "@/components/fire";
import { Markdown } from "@/components/markdown";
import { TelemetryDialog } from "@/components/telemetry-dialog";
import { Button } from "@/components/ui/button";
import { TagEditor } from "@/components/tag-editor";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { assigneeHasPilot, commentAuthor, pilotLabel, opAssigneeValue, opCode, PRIORITY, LABEL_COLOR } from "@/lib/labels";
import { elapsed } from "@/lib/timeline";
import type { Comment, Member, OpRef, Operation, Reaction, Relation, Run } from "@/lib/types";

const RUN_BADGE: Record<string, "secondary" | "brand" | "destructive" | "default"> = {
  queued: "secondary", claimed: "brand", starting: "brand", running: "brand",
  succeeded: "default", failed: "destructive", blocked: "destructive", canceled: "secondary",
};
const ACTIVE = new Set(["queued", "claimed", "starting", "running"]);
const isActive = (r: Run) => ACTIVE.has(r.state);
const STATUSES = ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "cancelled"];
const STATUS_LABEL: Record<string, string> = {
  backlog: "Backlog", todo: "Todo", in_progress: "In Progress", in_review: "In Review",
  done: "Done", blocked: "Blocked", cancelled: "Cancelled",
};
const EMOJI = ["👍", "🎉", "👀", "✅", "❤️", "🚀"];

export function OperationDetail() {
  const app = useApp();
  const open = app.selectedOp != null;
  const d = app.opDetail;
  const [comment, setComment] = useState("");
  const [telemetryRun, setTelemetryRun] = useState<Run | null>(null);

  const runs = d?.runs ?? [];
  const activeRun = runs.find(isActive);
  const ordered = [...runs].sort((a, b) => Number(isActive(b)) - Number(isActive(a)) || b.created_at.localeCompare(a.created_at));

  function openTelemetry(run: Run) {
    app.setSelectedRun(run.id);
    setTelemetryRun(run);
  }
  function assigneeChange(v: string) {
    if (!d) return;
    if (v === "me") app.reassign(d.operation.id, "user", app.user.id);
    else { const [k, id] = v.split(":"); app.reassign(d.operation.id, k, id); }
  }

  if (!open || !d) {
    return <TelemetryDialog run={telemetryRun} open={telemetryRun != null} onOpenChange={(o) => !o && setTelemetryRun(null)} />;
  }

  const fire = onFire(d.operation);

  return (
    <>
      {/* Centered full-page detail over the content column (sidebar stays). */}
      <div className="absolute inset-0 z-20 flex flex-col bg-background">
        <header className="flex h-12 shrink-0 items-center gap-3 border-b border-border px-4">
          <Button variant="ghost" size="icon-sm" onClick={() => app.openOp(null)} title="Back"><ArrowLeft /></Button>
          <StatusIcon status={d.operation.status} className="size-4" />
          <span className="font-mono text-[11px] font-medium uppercase text-muted-foreground">{opCode(d.operation, app.missions)}</span>
          <span className="truncate text-sm font-medium">{d.operation.title}</span>
          {activeRun && <WorkingPill />}
          <Button size="sm" variant="outline" className="ml-auto" onClick={() => app.runOp(d.operation.id)} disabled={!!activeRun}><Play /> Run</Button>
          {(d.operation.status === "done" || d.operation.status === "cancelled") && (
            <Button size="sm" variant="ghost" onClick={() => app.setArchived(d.operation.id, !d.operation.archived)} title={d.operation.archived ? "Unarchive" : "Archive"}>
              {d.operation.archived ? <ArchiveRestore /> : <Archive />} {d.operation.archived ? "Unarchive" : "Archive"}
            </Button>
          )}
        </header>

        <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="mx-auto flex max-w-5xl">
            {/* main */}
            <div className="min-w-0 flex-1">
              <div className="space-y-4 p-6">
                <h1 className="text-xl font-semibold leading-snug">{d.operation.title}</h1>
                {d.operation.body && <Markdown>{d.operation.body}</Markdown>}
                <ReactionBar reactions={d.operation.reactions ?? []} onToggle={(e) => app.react("operations", d.operation.id, e, d.operation.id)} />
                {d.operation.status === "in_review" && d.runs.some((r) => r.needs_input) && (
                  <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 p-3 text-sm">
                    <MessageCircleQuestion className="size-4 shrink-0 text-warning" />
                    <span>A pilot is waiting for your input — reply below to continue.</span>
                  </div>
                )}

                <SubOperations parentId={d.operation.id} missionId={d.operation.mission_id} children={d.children} />

                <Tabs defaultValue="activity">
                      <TabsList>
                        <TabsTrigger value="activity">Activity</TabsTrigger>
                        <TabsTrigger value="runs">
                          Runs
                          {activeRun && <span className="ml-1.5 inline-block size-1.5 animate-pulse rounded-full bg-info" />}
                        </TabsTrigger>
                      </TabsList>

                      <TabsContent value="activity" className="space-y-3">
                        {d.comments.length === 0 && <p className="text-sm text-muted-foreground">No messages yet.</p>}
                        {d.comments.map((c) => <CommentRow key={c.id} c={c} opId={d.operation.id} />)}
                        <form
                          className="flex gap-2 pt-1"
                          onSubmit={(e) => { e.preventDefault(); if (comment.trim()) { app.addComment(d.operation.id, comment); setComment(""); } }}
                        >
                          <Input value={comment} onChange={(e) => setComment(e.target.value)} placeholder="Reply…" className="h-9" />
                          <Button type="submit" size="sm">Send</Button>
                        </form>
                        {assigneeHasPilot(d.operation, app.crews) && <p className="text-[11px] text-muted-foreground">Replying resumes the pilot&apos;s session.</p>}
                      </TabsContent>

                      <TabsContent value="runs" className="space-y-1.5">
                        {runs.length === 0 && <p className="text-sm text-muted-foreground">No runs yet. Assign a pilot or hit Run.</p>}
                        {ordered.map((r) => <RunRow key={r.id} run={r} onTelemetry={() => openTelemetry(r)} />)}
                      </TabsContent>
                    </Tabs>
              </div>
            </div>

            {/* properties rail */}
            <div className="w-72 shrink-0 border-l border-border bg-muted/20">
              <div className="divide-y divide-border/60 text-sm">
                <div className="space-y-0.5 p-4">
                  <PropRow label="Status">
                    <RailSelect value={d.operation.status} onValueChange={(v) => app.moveOp(d.operation.id, v)}>
                      {STATUSES.map((s) => <SelectItem key={s} value={s}><span className="flex items-center gap-2"><StatusIcon status={s} className="size-3.5" /> {STATUS_LABEL[s]}</span></SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Assignee">
                    <RailSelect value={opAssigneeValue(d.operation, app.user)} onValueChange={assigneeChange} placeholder="Unassigned">
                      <SelectItem value="me">Me</SelectItem>
                      {app.members.filter((m) => m.id !== app.user.id).map((m) => <SelectItem key={`u${m.id}`} value={`user:${m.id}`}>🧑 {m.name || m.email}</SelectItem>)}
                      {app.pilots.map((a) => <SelectItem key={`a${a.id}`} value={`pilot:${a.id}`}><span className="flex items-center gap-2"><PilotIcon kind={a.kind} /> {a.name}</span></SelectItem>)}
                      {app.crews.map((c) => <SelectItem key={`c${c.id}`} value={`crew:${c.id}`}>👥 {c.name}</SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Priority">
                    <RailSelect value={String(d.operation.priority)} onValueChange={(v) => app.setPriority(d.operation.id, Number(v))}>
                      {PRIORITY.map((p, i) => <SelectItem key={i} value={String(i)}><span className="flex items-center gap-2"><PriorityIcon level={i} className="size-3.5" /> {p.label}</span></SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Mission">
                    <span className="truncate text-xs">{app.missions.find((m) => m.id === d.operation.mission_id)?.name ?? "—"}</span>
                  </PropRow>
                  <PropRow label="Start">
                    <DateField value={d.operation.start_date} onChange={(v) => app.setDates(d.operation.id, v, d.operation.due_date)} />
                  </PropRow>
                  <PropRow label="Due">
                    <DateField value={d.operation.due_date} onChange={(v) => app.setDates(d.operation.id, d.operation.start_date, v)} />
                  </PropRow>
                </div>

                <div className="p-4">
                  <p className="mb-1.5 text-[11px] font-medium uppercase text-muted-foreground">Labels</p>
                  <Labels op={d.operation} />
                </div>

                <div className="p-4">
                  <p className="mb-1.5 text-[11px] font-medium uppercase text-muted-foreground">Dispatch · rover tags</p>
                  <div className="space-y-1.5">
                    <div className="flex items-start gap-2 text-xs">
                      <span className="w-12 shrink-0 pt-1 text-muted-foreground">need</span>
                      <TagEditor tags={d.operation.required_tags ?? []} onChange={(t) => app.setOperationTags(d.operation.id, t, d.operation.excluded_tags ?? [])} placeholder="any" />
                    </div>
                    <div className="flex items-start gap-2 text-xs">
                      <span className="w-12 shrink-0 pt-1 text-muted-foreground">avoid</span>
                      <TagEditor tags={d.operation.excluded_tags ?? []} onChange={(t) => app.setOperationTags(d.operation.id, d.operation.required_tags ?? [], t)} placeholder="none" />
                    </div>
                  </div>
                </div>

                <div className="p-4">
                  <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><Link2 className="size-3.5" /> Relationships</p>
                  <Relationships op={d.operation} relations={d.relations ?? []} />
                </div>

                <div className="p-4">
                  <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><GitPullRequest className="size-3.5" /> Pull requests</p>
                  <PullRequests opId={d.operation.id} />
                </div>

                <div className="space-y-1.5 p-4 text-xs text-muted-foreground">
                  <div className="flex items-center justify-between"><span>Created by</span><span className="text-foreground">{memberDisplay(d.operation.created_by, app.members, app.user.id)}</span></div>
                  <div className="flex items-center justify-between"><span>Created</span><span>{new Date(d.operation.created_at).toLocaleDateString(undefined, { month: "short", day: "numeric" })}</span></div>
                  <div className="flex items-center justify-between"><span>Updated</span><span>{new Date(d.operation.updated_at).toLocaleDateString(undefined, { month: "short", day: "numeric" })}</span></div>
                </div>
              </div>
            </div>
          </div>
        </div>
        {fire && <DetailFire />}
      </div>
      <TelemetryDialog run={telemetryRun} open={telemetryRun != null} onOpenChange={(o) => !o && setTelemetryRun(null)} />
    </>
  );
}

// A compact label-left / control-right property row for the detail rail.
function PropRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-8 items-center justify-between gap-2">
      <span className="text-xs text-muted-foreground">{label}</span>
      <div className="flex min-w-0 justify-end">{children}</div>
    </div>
  );
}

// Borderless compact Select for the rail (value reads as inline text until hovered).
function RailSelect({ value, onValueChange, placeholder, children }: { value: string; onValueChange: (v: string) => void; placeholder?: string; children: React.ReactNode }) {
  return (
    <Select value={value} onValueChange={onValueChange}>
      <SelectTrigger className="h-7 w-auto gap-1 border-0 bg-transparent px-1.5 text-xs hover:bg-accent focus:ring-0"><SelectValue placeholder={placeholder} /></SelectTrigger>
      <SelectContent>{children}</SelectContent>
    </Select>
  );
}

// Low-key date control: shows a muted "—" / "Jun 12", with a transparent native
// date picker overlaid so the mm/dd/yyyy field isn't visible until interacted with.
function DateField({ value, onChange }: { value: string | null; onChange: (v: string | null) => void }) {
  return (
    <label className="relative inline-flex cursor-pointer items-center rounded-md px-1.5 py-0.5 text-xs hover:bg-accent">
      <span className={value ? "" : "text-muted-foreground/40"}>
        {value ? new Date(value + "T00:00:00").toLocaleDateString(undefined, { month: "short", day: "numeric" }) : "—"}
      </span>
      <input type="date" value={value ?? ""} onChange={(e) => onChange(e.target.value || null)} className="absolute inset-0 cursor-pointer opacity-0" />
    </label>
  );
}

function memberDisplay(id: string | null, members: Member[], userId: string): string {
  if (!id) return "—";
  if (id === userId) return "You";
  const m = members.find((x) => x.id === id);
  return m ? m.name || m.email : "—";
}

function CommentRow({ c, opId }: { c: Comment; opId: string }) {
  const app = useApp();
  const isPilot = c.author_type === "pilot";
  const pilot = isPilot ? app.pilots.find((a) => a.id === c.author_id) : null;
  const isSystem = c.author_type === "system";
  return (
    <div className="flex gap-2.5">
      <Avatar className="size-6">
        <AvatarFallback className={cn(isPilot && "bg-brand/15 text-brand", isSystem && "bg-muted text-muted-foreground")}>
          {pilot ? <PilotIcon kind={pilot.kind} size={13} /> : isPilot ? "P" : isSystem ? "·" : "U"}
        </AvatarFallback>
      </Avatar>
      <div className="flex-1">
        <div className="flex items-baseline gap-2">
          <span className={cn("text-sm font-medium", isPilot && "text-brand", isSystem && "text-muted-foreground")}>
            {commentAuthor(c, app.user.id, app.pilots)}
          </span>
          <span className="text-[11px] text-muted-foreground">{new Date(c.created_at).toLocaleString([], { hour12: false })}</span>
        </div>
        {isSystem ? <p className="text-sm text-muted-foreground">{c.body}</p> : <Markdown>{c.body}</Markdown>}
        <ReactionBar reactions={c.reactions} onToggle={(e) => app.react("comments", c.id, e, opId)} />
      </div>
    </div>
  );
}

// Shared reaction strip: existing reactions (hover → reactors) + an add-emoji menu.
function ReactionBar({ reactions, onToggle }: { reactions: Reaction[]; onToggle: (emoji: string) => void }) {
  return (
    <TooltipProvider delayDuration={150}>
      <div className="mt-1 flex flex-wrap items-center gap-1">
        {reactions.map((r) => (
          <Tooltip key={r.emoji}>
            <TooltipTrigger asChild>
              <button onClick={() => onToggle(r.emoji)} className={cn("rounded-full border px-1.5 py-0.5 text-xs", r.mine ? "border-brand bg-brand/10" : "border-border")}>
                {r.emoji} {r.count}
              </button>
            </TooltipTrigger>
            <TooltipContent>{(r.users ?? []).join(", ") || r.emoji}</TooltipContent>
          </Tooltip>
        ))}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button className="rounded-full p-1 text-muted-foreground hover:text-foreground"><SmilePlus className="size-3.5" /></button>
          </DropdownMenuTrigger>
          <DropdownMenuContent className="flex gap-1 p-1">
            {EMOJI.map((e) => <button key={e} onClick={() => onToggle(e)} className="rounded px-1 text-base hover:bg-accent">{e}</button>)}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </TooltipProvider>
  );
}

function Labels({ op }: { op: Operation }) {
  const app = useApp();
  const [name, setName] = useState("");
  const onOp = new Set(op.labels.map((l) => l.id));
  const available = app.labels.filter((l) => !onOp.has(l.id));
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap gap-1">
        {op.labels.map((l) => (
          <span key={l.id} className={cn("inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px]", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)}>
            {l.name}
            <button onClick={() => app.detachLabel(op.id, l.id)} className="opacity-70 hover:opacity-100"><X className="size-2.5" /></button>
          </span>
        ))}
      </div>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" className="h-6 px-1.5 text-xs text-muted-foreground"><Plus className="size-3" /> label</Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent className="w-48">
          {available.map((l) => (
            <DropdownMenuItem key={l.id} onClick={() => app.attachLabel(op.id, l.id)}>
              <span className={cn("mr-2 size-2 rounded-full", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)} />{l.name}
            </DropdownMenuItem>
          ))}
          <form
            className="flex gap-1 p-1"
            onSubmit={async (e) => { e.preventDefault(); if (!name.trim()) return; const l = await app.createLabel(name.trim(), "blue"); if (l) app.attachLabel(op.id, l.id); setName(""); }}
          >
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="new label" className="h-7 text-xs" />
          </form>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

function PullRequests({ opId }: { opId: string }) {
  const app = useApp();
  const prs = app.opDetail?.pull_requests ?? [];
  const [url, setUrl] = useState("");
  return (
    <div className="space-y-1.5">
      {prs.map((p) => (
        <div key={p.id} className="flex items-center gap-1.5 text-xs">
          <a href={p.url} target="_blank" rel="noreferrer" className="min-w-0 flex-1 truncate text-info hover:underline">{p.title || p.url}</a>
          <button onClick={() => app.deletePR(p.id, opId)} className="text-muted-foreground hover:text-destructive"><X className="size-3" /></button>
        </div>
      ))}
      {prs.length === 0 && <p className="text-xs text-muted-foreground">No linked PRs.</p>}
      <form
        onSubmit={(e) => { e.preventDefault(); if (url.trim()) { app.addPR(opId, url.trim(), ""); setUrl(""); } }}
      >
        <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="Link a PR url…" className="h-7 text-xs" />
      </form>
    </div>
  );
}

const REL_LABEL: Record<string, string> = {
  blocks: "Blocks", blocked_by: "Blocked by", relates: "Relates to",
  duplicate: "Duplicate of", duplicated_by: "Duplicated by",
};
const REL_ORDER = ["blocks", "blocked_by", "relates", "duplicate", "duplicated_by"];

function Relationships({ op, relations }: { op: Operation; relations: Relation[] }) {
  const app = useApp();
  const [addKind, setAddKind] = useState<string | null>(null);
  const [q, setQ] = useState("");
  const [results, setResults] = useState<OpRef[]>([]);
  useEffect(() => {
    if (addKind === null) return;
    let active = true;
    app.searchOps(q).then((r) => { if (active) setResults(r.filter((o) => o.id !== op.id)); });
    return () => { active = false; };
  }, [q, addKind, op.id, app]);
  const groups = REL_ORDER.map((k) => ({ k, items: relations.filter((r) => r.kind === k) })).filter((g) => g.items.length > 0);
  return (
    <div className="space-y-2">
      {groups.map((g) => (
        <div key={g.k} className="space-y-0.5">
          <p className="text-[10px] uppercase tracking-wide text-muted-foreground/70">{REL_LABEL[g.k]}</p>
          {g.items.map((r) => (
            <div key={r.id} className="group flex items-center gap-1.5 text-xs">
              <StatusIcon status={r.operation.status} className="size-3.5 shrink-0" />
              <button onClick={() => app.openOp(r.operation.id)} className="flex min-w-0 flex-1 items-center gap-1.5 text-left hover:underline">
                <span className="font-mono text-[10px] text-muted-foreground">{opCode(r.operation as Operation, app.missions)}</span>
                <span className="truncate">{r.operation.title}</span>
              </button>
              <button onClick={() => app.removeRelation(r.id, op.id)} className="shrink-0 text-muted-foreground opacity-0 hover:text-destructive group-hover:opacity-100"><X className="size-3" /></button>
            </div>
          ))}
        </div>
      ))}
      {addKind === null ? (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="sm" className="text-xs text-muted-foreground"><Plus className="size-3" /> Add relation</Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            {REL_ORDER.map((k) => <DropdownMenuItem key={k} onClick={() => { setAddKind(k); setQ(""); setResults([]); }}>{REL_LABEL[k]}…</DropdownMenuItem>)}
          </DropdownMenuContent>
        </DropdownMenu>
      ) : (
        <div className="space-y-1">
          <div className="flex items-center gap-1 text-[10px] uppercase tracking-wide text-muted-foreground/70">
            {REL_LABEL[addKind]}
            <button onClick={() => setAddKind(null)} className="ml-auto hover:text-foreground"><X className="size-3" /></button>
          </div>
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search operations…" className="h-7 text-xs" autoFocus />
          <div className="max-h-40 space-y-0.5 overflow-auto">
            {results.map((o) => (
              <button key={o.id} onClick={() => { app.addRelation(op.id, addKind, o.id); setAddKind(null); }} className="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left text-xs hover:bg-accent">
                <StatusIcon status={o.status} className="size-3.5 shrink-0" />
                <span className="font-mono text-[10px] text-muted-foreground">{opCode(o as Operation, app.missions)}</span>
                <span className="truncate">{o.title}</span>
              </button>
            ))}
            {q && results.length === 0 && <p className="px-1.5 py-1 text-xs text-muted-foreground">No matches.</p>}
          </div>
        </div>
      )}
    </div>
  );
}

function SubOperations({ parentId, missionId, children }: { parentId: string; missionId: string | null; children: Operation[] }) {
  const app = useApp();
  const [title, setTitle] = useState("");
  const [adding, setAdding] = useState(false);
  if (children.length === 0 && !adding) {
    return <Button variant="ghost" size="sm" className="text-xs text-muted-foreground" onClick={() => setAdding(true)}><Plus className="size-3" /> Add sub-operation</Button>;
  }
  return (
    <div className="space-y-1">
      <p className="text-[11px] font-medium uppercase text-muted-foreground">Sub-operations {children.length > 0 && `· ${children.filter((c) => c.status === "done").length}/${children.length}`}</p>
      {children.map((c) => (
        <button key={c.id} onClick={() => app.openOp(c.id)} className="flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-sm hover:bg-accent/50">
          <StatusIcon status={c.status} className="size-3.5" />
          <span className="font-mono text-[10px] text-muted-foreground">{opCode(c, app.missions)}</span>
          <span className="truncate">{c.title}</span>
        </button>
      ))}
      <form
        className="flex gap-1"
        onSubmit={(e) => { e.preventDefault(); if (title.trim() && missionId) { app.createOperation({ title: title.trim(), body: "", mission_id: missionId, assignee_type: "user", assignee_id: app.user.id, parent_id: parentId }); setTitle(""); } }}
      >
        <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="New sub-operation…" className="h-7 text-xs" autoFocus />
      </form>
    </div>
  );
}

function WorkingPill() {
  return (
    <span className="flex shrink-0 items-center gap-1.5 rounded-full bg-info/10 px-2 py-0.5 text-xs font-medium text-info">
      <Loader2 className="size-3 animate-spin" /> Working
    </span>
  );
}

function RunRow({ run, onTelemetry }: { run: Run; onTelemetry: () => void }) {
  const active = isActive(run);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);

  return (
    <div className={cn("flex items-center gap-2 rounded-lg border px-3 py-2 text-sm", active ? "border-info/40" : "border-border")}>
      {active ? <Loader2 className="size-3.5 animate-spin text-info" /> : <Terminal className="size-3.5 text-muted-foreground" />}
      <Badge variant={RUN_BADGE[run.state] ?? "secondary"}>{run.state}</Badge>
      {run.pilot && <span className="flex items-center gap-1.5 text-xs text-muted-foreground"><PilotIcon kind={run.pilot} size={13} /> {pilotLabel(run.pilot)}</span>}
      <span className={cn("text-xs tabular-nums", active ? "text-info" : "text-muted-foreground")}>
        {active ? elapsed(run.created_at, now) : elapsed(run.created_at, new Date(run.updated_at).getTime())}
      </span>
      <Button variant="ghost" size="sm" className="ml-auto" onClick={onTelemetry}>
        <ScrollText /> Telemetry
      </Button>
    </div>
  );
}
