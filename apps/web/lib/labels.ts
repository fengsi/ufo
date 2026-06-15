import type { Pilot, Comment, Crew, Member, Mission, Operation } from "@/lib/types";

// Display id: <mission key>-<per-mission seq>, e.g. MSJ-123.
export function opCode(op: Operation, missions: Mission[]): string {
  const key = missions.find((m) => m.id === op.mission_id)?.key ?? "OP";
  return `${key}-${op.seq}`;
}

export function assigneeLabel(op: Operation, user: { id: string }, pilots: Pilot[], crews: Crew[], members: Member[] = []): string {
  if (!op.assignee_type) return "Unassigned";
  if (op.assignee_type === "user") return memberName(op.assignee_id, user, members);
  if (op.assignee_type === "pilot") return pilots.find((a) => a.id === op.assignee_id)?.name ?? "Pilot";
  if (op.assignee_type === "crew") return crews.find((c) => c.id === op.assignee_id)?.name ?? "Crew";
  return op.assignee_type;
}

function memberName(id: string | null, user: { id: string }, members: Member[]): string {
  if (id === user.id) return "You";
  const m = members.find((x) => x.id === id);
  return m ? m.name || m.email : "User";
}

export function assigneeHasPilot(op: Operation, crews: Crew[] = []): boolean {
  if (op.assignee_type === "pilot") return true;
  if (op.assignee_type !== "crew") return false;
  return crews
    .find((c) => c.id === op.assignee_id)
    ?.members?.some((m) => m.member_type === "pilot") ?? false;
}

export function opAssigneeValue(op: Operation, user: { id: string }): string {
  if (op.assignee_type === "user" && op.assignee_id === user.id) return "me";
  if (op.assignee_type) return `${op.assignee_type}:${op.assignee_id}`;
  return "";
}

export function commentAuthor(c: Comment, userId: string, pilots: Pilot[]): string {
  if (c.author_type === "user") return c.author_id === userId ? "You" : "User";
  if (c.author_type === "pilot") return pilots.find((a) => a.id === c.author_id)?.name ?? "Pilot";
  return "System";
}

const PILOT_LABELS: Record<string, string> = {
  claude: "Claude",
  codex: "Codex",
};

export function pilotLabel(pilot: string): string {
  return PILOT_LABELS[pilot] ?? pilot;
}

// Priority rank (0 none -> 4 urgent) -> label + color.
export const PRIORITY: { label: string; color: string }[] = [
  { label: "No priority", color: "text-muted-foreground" },
  { label: "Low", color: "text-info" },
  { label: "Medium", color: "text-warning" },
  { label: "High", color: "text-destructive" },
  { label: "Urgent", color: "text-destructive" },
];

// Left-edge accent per priority.
export const PRIORITY_ACCENT = [
  "border-l-border", "border-l-info", "border-l-warning", "border-l-destructive", "border-l-destructive",
];

export const LABEL_COLOR: Record<string, string> = {
  gray: "bg-muted text-muted-foreground",
  red: "bg-destructive/15 text-destructive",
  orange: "bg-warning/15 text-warning",
  green: "bg-success/15 text-success",
  blue: "bg-info/15 text-info",
  purple: "bg-brand/15 text-brand",
};

export function initials(name: string): string {
  const parts = name.trim().split(/\s+/);
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}
