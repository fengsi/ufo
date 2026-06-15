import type { RunMessage } from "@/lib/types";

// A concise one-line summary of a tool call's input (command, file, query, …).
export function toolSummary(input?: Record<string, unknown> | null): string {
  if (!input) return "";
  const keys = ["command", "query", "file_path", "path", "pattern", "description", "prompt", "skill", "url"];
  for (const k of keys) {
    const v = input[k];
    if (typeof v === "string" && v) return v.length > 140 ? v.slice(0, 140) + "…" : v;
  }
  for (const v of Object.values(input)) {
    if (typeof v === "string" && v && v.length < 140) return v;
  }
  return "";
}

// Telemetry type → label + on-theme color (semantic tokens, no literal palette).
export const TYPE_META: Record<RunMessage["type"], { label: string; dot: string; text: string }> = {
  thinking: { label: "Thinking", dot: "bg-muted-foreground", text: "text-muted-foreground italic" },
  text: { label: "Pilot", dot: "bg-brand", text: "text-foreground" },
  tool_use: { label: "Tool", dot: "bg-info", text: "text-info" },
  tool_result: { label: "Result", dot: "bg-muted-foreground", text: "text-muted-foreground" },
  error: { label: "Error", dot: "bg-destructive", text: "text-destructive" },
};

export function timeAgo(iso: string): string {
  const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function elapsed(fromISO: string, now: number): string {
  const ms = Math.max(0, now - new Date(fromISO).getTime());
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${s % 60}s`;
}
