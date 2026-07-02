"use client";

import { useEffect, useRef, useState } from "react";
import { Check, ChevronRight, Copy, Download, Loader2 } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { PilotIcon } from "@/components/pilot-icon";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { cn, hideFlowControlFlags } from "@/lib/utils";
import { apiFetch } from "@/lib/api";
import { pilotLabel } from "@/lib/labels";
import { TYPE_META, elapsed, toolSummary } from "@/lib/timeline";
import { useTelemetryShowAll, useTimeFormat, type TimeFormat } from "@/lib/view";
import type { Artifact, Run, RunEvent, RunMessage } from "@/lib/types";

const ACTIVE = new Set(["queued", "accepted", "starting", "running"]);

function logTime(value: string, timeFormat: TimeFormat) {
  return new Date(value).toLocaleTimeString(undefined, {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: timeFormat === "12h",
  });
}

// "Telemetry" = the rover-streamed, step-by-step record of what the pilot did.
export function TelemetryDialog({ run, open, onOpenChange }: { run: Run | null; open: boolean; onOpenChange: (o: boolean) => void }) {
  const app = useApp();
  const { timeFormat } = useTimeFormat();
  const { telemetryShowAll, setTelemetryShowAll } = useTelemetryShowAll();
  const detail = run && app.runDetail?.run.id === run.id ? app.runDetail : null;
  const liveRun = detail?.run ?? run;
  const msgs = detail?.messages ?? [];
  const visibleMsgs = telemetryShowAll ? msgs : msgs.filter((m) => m.type === "text");
  const events = detail?.events ?? [];
  const active = liveRun ? ACTIVE.has(liveRun.status) : false;
  const bottomRef = useRef<HTMLDivElement>(null);
  const rowRefs = useRef<Map<number, HTMLDivElement>>(new Map());

  useEffect(() => {
    if (open && active) bottomRef.current?.scrollIntoView({ block: "end" });
  }, [open, active, visibleMsgs.length]);

  const duration = liveRun
    ? active
      ? elapsed(liveRun.created_at, Date.now())
      : elapsed(liveRun.created_at, new Date(liveRun.updated_at).getTime())
    : "";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[80vh] w-full max-w-2xl flex-col">
        <DialogHeader className="mb-0">
          <DialogTitle className="flex items-center gap-2 pr-8 text-base">
            {active && <Loader2 className="size-4 animate-spin text-info" />}
            Telemetry — run #{run?.id}
            {duration && <span className="ml-auto text-xs font-normal tabular-nums text-muted-foreground">{duration}</span>}
          </DialogTitle>
        </DialogHeader>

        <label className="mt-2 inline-flex w-fit items-center gap-1.5 text-xs text-muted-foreground">
          <input type="checkbox" checked={telemetryShowAll} onChange={(e) => setTelemetryShowAll(e.target.checked)} />
          Show all
        </label>

        {telemetryShowAll && msgs.length > 0 && <TimelineBar msgs={msgs} rowRefs={rowRefs} />}

        <div className="-mr-2 mt-3 flex-1 space-y-2.5 overflow-y-auto pr-2">
          {visibleMsgs.length === 0 && (
            <p className="text-sm text-muted-foreground">{msgs.length > 0 && !telemetryShowAll ? "No pilot replies in this run." : active ? "Waiting for the pilot to report…" : "No telemetry for this run."}</p>
          )}
          {telemetryShowAll && events.length > 0 && (
            <div className="space-y-1.5 rounded-md border border-border bg-muted/30 p-2">
              <p className="text-xs font-semibold text-muted-foreground">Run events</p>
              {events.map((e, i) => <RunEventRow key={`${e.created_at}-${i}`} event={e} timeFormat={timeFormat} />)}
            </div>
          )}
          {visibleMsgs.map((m) => (
            <div key={m.sequence} ref={(el) => { if (el) rowRefs.current.set(m.sequence, el); }}>
              <TelemetryRow m={m} pilot={run?.pilot} timeFormat={timeFormat} />
            </div>
          ))}
          {telemetryShowAll && active && (
            <div className="flex items-center gap-1.5 text-xs text-info"><Loader2 className="size-3 animate-spin" /> Running</div>
          )}
          {telemetryShowAll && detail?.artifacts.map((a) => <ArtifactBlock key={a.id || a.name} artifact={a} />)}
          <div ref={bottomRef} />
        </div>
      </DialogContent>
    </Dialog>
  );
}

function assetFilePath(assetId: string) {
  return `/v1/assets/${assetId}/file`;
}

function artifactContentPath(artifactId: string) {
  return `/api/v1/artifacts/${artifactId}/content`;
}

function ArtifactBlock({ artifact }: { artifact: Artifact }) {
  const [content, setContent] = useState(artifact.content);
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const lines = content ? content.split("\n") : [];
  const previewBytes = content ? new Blob([content]).size : 0;
  const truncated = !artifact.asset_id && artifact.byte_size > previewBytes;
  const contentPath = artifactContentPath(artifact.id);
  async function ensureFullContent() {
    if (!truncated) return content;
    if (loading) return;
    setLoading(true);
    setFailed(false);
    try {
      const res = await apiFetch(contentPath);
      if (!res.ok) throw new Error(`artifact content ${res.status}`);
      const text = await res.text();
      setContent(text);
      return text;
    } catch {
      setFailed(true);
    } finally {
      setLoading(false);
    }
  }
  async function loadFullContent() {
    await ensureFullContent();
  }
  async function copyContent() {
    setCopyFailed(false);
    try {
      const text = await ensureFullContent();
      if (text == null) throw new Error("artifact content unavailable");
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    } catch {
      setCopyFailed(true);
    }
  }
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className="min-w-0 flex-1 truncate text-xs font-semibold text-muted-foreground">{artifact.name}</span>
        {truncated && (
          <button type="button" className="text-[11px] text-muted-foreground hover:text-foreground" onClick={loadFullContent} disabled={loading}>
            {loading ? "Loading..." : "Load full"}
          </button>
        )}
        {!artifact.asset_id && (
          <>
            <button type="button" className="text-muted-foreground hover:text-foreground disabled:opacity-50" title={copied ? "Copied" : "Copy"} aria-label={`Copy ${artifact.name}`} disabled={loading} onClick={copyContent}>
              {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            </button>
            <a href={contentPath} download={artifact.name || "artifact.txt"} className="text-muted-foreground hover:text-foreground" title="Download" aria-label={`Download ${artifact.name}`}>
              <Download className="size-3.5" />
            </a>
          </>
        )}
        {artifact.asset_id && (
          <a href={assetFilePath(artifact.asset_id)} className="text-muted-foreground hover:text-foreground" title="Download" aria-label={`Download ${artifact.name}`}>
            <Download className="size-3.5" />
          </a>
        )}
      </div>
      {lines.length > 0 && (
        <pre className="mt-1 max-h-72 overflow-auto rounded-md border border-border bg-muted/50 p-2 text-xs">
          {lines.map((line, i) => {
            let c = "";
            if (line.startsWith("+") && !line.startsWith("+++")) c = "text-success";
            else if (line.startsWith("-") && !line.startsWith("---")) c = "text-destructive";
            else if (line.startsWith("@@")) c = "text-info";
            else if (line.startsWith("diff ") || line.startsWith("index ") || line.startsWith("+++") || line.startsWith("---")) c = "text-muted-foreground";
            return <span key={i} className={cn("block whitespace-pre-wrap", c)}>{line || " "}</span>;
          })}
        </pre>
      )}
      {failed && <p className="mt-1 text-xs text-destructive">Could not load full artifact.</p>}
      {copyFailed && <p className="mt-1 text-xs text-destructive">Could not copy artifact.</p>}
    </div>
  );
}

// A horizontal bar of segments (consecutive same-type runs coalesced), width by
// share of the timeline — click to jump to that part.
function TimelineBar({ msgs, rowRefs }: { msgs: RunMessage[]; rowRefs: React.MutableRefObject<Map<number, HTMLDivElement>> }) {
  const segs: { sequence: number; type: RunMessage["type"]; count: number }[] = [];
  for (const m of msgs) {
    const last = segs[segs.length - 1];
    if (last && last.type === m.type) last.count++;
    else segs.push({ sequence: m.sequence, type: m.type, count: 1 });
  }
  return (
    <div className="ufo-run-log-bar mt-3 flex h-2.5 gap-0.5 overflow-hidden rounded">
      {segs.map((s) => (
        <button
          key={s.sequence}
          title={`${TYPE_META[s.type].label}${s.count > 1 ? ` ×${s.count}` : ""}`}
          onClick={() => rowRefs.current.get(s.sequence)?.scrollIntoView({ behavior: "smooth", block: "center" })}
          data-type={s.type}
          className={cn("ufo-run-log-segment h-full min-w-[4px] opacity-80 transition-opacity hover:opacity-100", TYPE_META[s.type].dot)}
          style={{ width: `${(s.count / msgs.length) * 100}%` }}
        />
      ))}
    </div>
  );
}

function RunEventRow({ event, timeFormat }: { event: RunEvent; timeFormat: TimeFormat }) {
  const bad = event.kind === "error" || event.message.includes("failed") || event.message.includes("blocked");
  return (
    <div className="flex gap-2 text-xs">
      <span className="flex h-4 shrink-0 items-center">
        <span className={cn("size-1.5 rounded-full", bad ? "bg-destructive" : "bg-muted-foreground")} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="shrink-0 font-medium text-muted-foreground">{event.kind}</span>
          <span className="ml-auto shrink-0 text-[11px] tabular-nums text-muted-foreground">{logTime(event.created_at, timeFormat)}</span>
        </div>
        <span className={cn("whitespace-pre-wrap break-words", bad ? "text-destructive" : "text-foreground")}>{hideFlowControlFlags(event.message)}</span>
      </div>
    </div>
  );
}

function TelemetryRow({ m, pilot, timeFormat }: { m: RunMessage; pilot?: string; timeFormat: TimeFormat }) {
  const meta = TYPE_META[m.type];
  const summary = m.type === "tool_use" ? toolSummary(m.input) : "";
  const label = m.type === "tool_use" ? ((m.tool ?? "Tool").replace(/^\w/, (c) => c.toUpperCase())) : m.type === "text" && pilot ? (
    <span className="inline-flex items-center gap-1.5"><PilotIcon kind={pilot} size={12} /> {pilotLabel(pilot)}</span>
  ) : meta.label;
  const dotOffset = m.type === "text" ? "" : "translate-y-px";
  const content = m.content ? hideFlowControlFlags(m.content) : "";

  return (
    <div className="flex gap-2.5">
      <span className="flex h-4 shrink-0 items-center">
        <span className={cn("size-2 rounded-full", dotOffset, meta.dot)} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className={cn("text-xs font-semibold", meta.text)}>{label}</span>
          <span className="ml-auto text-[11px] tabular-nums text-muted-foreground">{logTime(m.created_at, timeFormat)}</span>
        </div>
        {(m.type === "text" || m.type === "thinking" || m.type === "error") && content.trim() && (
          <p className={cn("whitespace-pre-wrap break-words text-sm", meta.text)}>{content}</p>
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
