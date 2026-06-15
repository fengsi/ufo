"use client";

import { useEffect, useRef } from "react";
import { ChevronRight, Loader2 } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { PilotIcon } from "@/components/pilot-icon";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { cn } from "@/lib/utils";
import { pilotLabel } from "@/lib/labels";
import { TYPE_META, elapsed, toolSummary } from "@/lib/timeline";
import type { Run, RunMessage } from "@/lib/types";

const ACTIVE = new Set(["queued", "claimed", "starting", "running"]);

// "Telemetry" = the rover-streamed, step-by-step record of what the pilot did.
export function TelemetryDialog({ run, open, onOpenChange }: { run: Run | null; open: boolean; onOpenChange: (o: boolean) => void }) {
  const app = useApp();
  const detail = run && app.runDetail?.run.id === run.id ? app.runDetail : null;
  const msgs = detail?.messages ?? [];
  const active = run ? ACTIVE.has(run.state) : false;
  const bottomRef = useRef<HTMLDivElement>(null);
  const rowRefs = useRef<Map<number, HTMLDivElement>>(new Map());

  useEffect(() => {
    if (open && active) bottomRef.current?.scrollIntoView({ block: "end" });
  }, [open, active, msgs.length]);

  const duration = run
    ? active
      ? elapsed(run.created_at, Date.now())
      : elapsed(run.created_at, new Date(run.updated_at).getTime())
    : "";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[80vh] w-full max-w-2xl flex-col">
        <DialogHeader className="mb-0">
          <DialogTitle className="flex items-center gap-2 text-base">
            {active && <Loader2 className="size-4 animate-spin text-info" />}
            Telemetry — run #{run?.id}
            {run?.pilot && <span className="flex items-center gap-1.5 text-sm font-normal text-muted-foreground"><PilotIcon kind={run.pilot} /> {pilotLabel(run.pilot)}</span>}
            {duration && <span className="ml-auto text-xs font-normal tabular-nums text-muted-foreground">{duration}</span>}
          </DialogTitle>
        </DialogHeader>

        {msgs.length > 0 && <TimelineBar msgs={msgs} rowRefs={rowRefs} />}

        <div className="-mr-2 mt-3 flex-1 space-y-2.5 overflow-y-auto pr-2">
          {msgs.length === 0 && (
            <p className="text-sm text-muted-foreground">{active ? "Waiting for the pilot to report…" : "No telemetry for this run."}</p>
          )}
          {msgs.map((m) => (
            <div key={m.seq} ref={(el) => { if (el) rowRefs.current.set(m.seq, el); }}>
              <TelemetryRow m={m} />
            </div>
          ))}
          {active && (
            <div className="flex items-center gap-1.5 text-xs text-info"><Loader2 className="size-3 animate-spin" /> running…</div>
          )}
          {detail?.artifacts.map((a) => (
            <div key={a.name}>
              <span className="text-xs font-semibold text-muted-foreground">{a.name}</span>
              <pre className="mt-1 max-h-72 overflow-auto rounded-md border border-border bg-muted/50 p-2 text-xs">
                {a.content.split("\n").map((line, i) => {
                  let c = "";
                  if (line.startsWith("+") && !line.startsWith("+++")) c = "text-success";
                  else if (line.startsWith("-") && !line.startsWith("---")) c = "text-destructive";
                  else if (line.startsWith("@@")) c = "text-info";
                  else if (line.startsWith("diff ") || line.startsWith("index ") || line.startsWith("+++") || line.startsWith("---")) c = "text-muted-foreground";
                  return <span key={i} className={cn("block whitespace-pre-wrap", c)}>{line || " "}</span>;
                })}
              </pre>
            </div>
          ))}
          <div ref={bottomRef} />
        </div>
      </DialogContent>
    </Dialog>
  );
}

// A horizontal bar of segments (consecutive same-type runs coalesced), width by
// share of the timeline — click to jump to that part.
function TimelineBar({ msgs, rowRefs }: { msgs: RunMessage[]; rowRefs: React.MutableRefObject<Map<number, HTMLDivElement>> }) {
  const segs: { seq: number; type: RunMessage["type"]; count: number }[] = [];
  for (const m of msgs) {
    const last = segs[segs.length - 1];
    if (last && last.type === m.type) last.count++;
    else segs.push({ seq: m.seq, type: m.type, count: 1 });
  }
  return (
    <div className="mt-3 flex h-2 gap-0.5 overflow-hidden rounded">
      {segs.map((s) => (
        <button
          key={s.seq}
          title={`${TYPE_META[s.type].label}${s.count > 1 ? ` ×${s.count}` : ""}`}
          onClick={() => rowRefs.current.get(s.seq)?.scrollIntoView({ behavior: "smooth", block: "center" })}
          className={cn("h-full min-w-[3px] opacity-70 transition-opacity hover:opacity-100", TYPE_META[s.type].dot)}
          style={{ width: `${(s.count / msgs.length) * 100}%` }}
        />
      ))}
    </div>
  );
}

function TelemetryRow({ m }: { m: RunMessage }) {
  const meta = TYPE_META[m.type];
  const summary = m.type === "tool_use" ? toolSummary(m.input) : "";
  const label = m.type === "tool_use" ? (m.tool ?? "Tool") : meta.label;

  return (
    <div className="flex gap-2.5">
      <span className={cn("mt-1.5 size-2 shrink-0 rounded-full", meta.dot)} />
      <div className="min-w-0 flex-1">
        <span className={cn("text-xs font-semibold", meta.text)}>{label}</span>
        {(m.type === "text" || m.type === "thinking" || m.type === "error") && m.content && (
          <p className={cn("whitespace-pre-wrap break-words text-sm", meta.text)}>{m.content}</p>
        )}
        {m.type === "tool_use" && (
          <>
            {summary && <p className="break-words font-mono text-xs text-foreground">{summary}</p>}
            {m.input && <Expandable label="input" body={JSON.stringify(m.input, null, 2)} />}
          </>
        )}
        {m.type === "tool_result" && (m.output ? <Expandable label="output" body={m.output} defaultOpen={m.output.length < 400} /> : <p className="text-xs text-muted-foreground">(no output)</p>)}
      </div>
    </div>
  );
}

function Expandable({ label, body, defaultOpen = false }: { label: string; body: string; defaultOpen?: boolean }) {
  return (
    <Collapsible defaultOpen={defaultOpen} className="mt-1">
      <CollapsibleTrigger className="group flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground">
        <ChevronRight className="size-3 transition-transform group-data-[state=open]:rotate-90" />
        {label}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <pre className="mt-1 max-h-60 overflow-auto rounded-md border border-border bg-muted/50 p-2 text-xs">{body}</pre>
      </CollapsibleContent>
    </Collapsible>
  );
}
