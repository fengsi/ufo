// Public ids are opaque strings; the API never exposes internal numeric ids.
export type User = { id: string; email: string; name: string };
export type Fleet = { id: string; name: string; kind: string };
export type Member = { id: string; email: string; name: string; role: string };
export type Invitation = { id: string; invitee_email: string; role: string; status: string; expires_at: string };
export type MyInvite = { id: string; fleet_id: string; fleet_name: string; role: string; invitee_email: string };
export type Mission = { id: string; name: string; key: string };
export type Pilot = { id: string; name: string; kind: string };
export type CrewMember = { member_type: string; member_id: string; role: string };
export type Crew = { id: string; name: string; members?: CrewMember[] };
export type Label = { id: string; name: string; color: string };
export type PullRequest = { id: string; url: string; title: string; state: string; number?: number | null };
export type OpRef = { id: string; title: string; status: string; seq: number; mission_id: string };
export type RelationKind = "blocks" | "blocked_by" | "relates" | "duplicate" | "duplicated_by";
export type Relation = { id: string; kind: RelationKind; operation: OpRef };
export type SubProgress = { total: number; done: number };
export type Operation = {
  id: string;
  title: string;
  body: string;
  status: string;
  mission_id: string | null;
  seq: number;
  priority: number;
  assignee_type: string | null;
  assignee_id: string | null;
  required_tags: string[];
  excluded_tags: string[];
  labels: Label[];
  reactions: Reaction[];
  sub: SubProgress;
  start_date: string | null;
  due_date: string | null;
  parent_id: string | null;
  created_by: string | null;
  archived: boolean;
  created_at: string;
  updated_at: string;
};
export type Reaction = { emoji: string; count: number; mine: boolean; users: string[] };
export type Comment = {
  id: string;
  author_type: string;
  author_id: string | null;
  body: string;
  reactions: Reaction[];
  created_at: string;
};
export type Run = {
  id: string;
  operation_id: string;
  state: string;
  pilot?: string;
  needs_input?: boolean;
  created_at: string;
  updated_at: string;
};
export type RunEvent = { kind: string; message: string; created_at: string };
export type Artifact = { kind: string; name: string; content: string; created_at: string };
export type RunMessage = {
  seq: number;
  type: "text" | "thinking" | "tool_use" | "tool_result" | "error";
  tool?: string;
  content?: string;
  input?: Record<string, unknown> | null;
  output?: string;
  created_at: string;
};
export type Rover = { id: string; name: string; status: string; tags: string[]; auto_tags: string[]; last_seen_at?: string; created_at: string };
export type EnrollmentCode = {
  id: string;
  code: string;
  label: string;
  reusable: boolean;
  expires_at: string | null;
};
export type Signal = {
  id: string;
  operation_id: string | null;
  type: string;
  severity: string;
  title: string;
  body: string;
  read: boolean;
  created_at: string;
};

export type OperationDetail = { operation: Operation; comments: Comment[]; runs: Run[]; children: Operation[]; pull_requests: PullRequest[]; relations: Relation[] };
export type RunDetail = { run: Run; events: RunEvent[]; artifacts: Artifact[]; messages: RunMessage[] };

export const BOARD_COLUMNS: { key: string; label: string }[] = [
  { key: "backlog", label: "Backlog" },
  { key: "todo", label: "Todo" },
  { key: "in_progress", label: "In Progress" },
  { key: "in_review", label: "In Review" },
  { key: "done", label: "Done" },
  { key: "blocked", label: "Blocked" },
];

// Tailwind text/bg color classes per status (semantic tokens).
export const STATUS_TEXT: Record<string, string> = {
  backlog: "text-muted-foreground",
  todo: "text-foreground",
  in_progress: "text-info",
  in_review: "text-warning",
  done: "text-success",
  blocked: "text-destructive",
  cancelled: "text-muted-foreground",
};
