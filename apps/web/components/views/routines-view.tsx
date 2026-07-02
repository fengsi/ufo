"use client";

import { useRef, useState } from "react";
import { Activity, Loader2, Paperclip, Pencil, Play, Plus, Tags, Trash2, UserRound, X, type LucideIcon } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { AssetChipStrip } from "@/components/asset-display";
import { PriorityIcon } from "@/components/priority-icon";
import { appendAssetLink } from "@/lib/assets";
import { CrewOption, PilotOption } from "@/components/assignee-select";
import { TagEditor, TagList } from "@/components/tag-editor";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { PRIORITY, memberLabel, pilotLabel, userLabel } from "@/lib/labels";
import { SECTION_ICONS } from "@/lib/section-icons";
import type { Asset, AssigneeType, Crew, Routine, RoutineTriggerType } from "@/lib/types";

const CRON_PRESETS = [
  { value: "@hourly", label: "Hourly" },
  { value: "@daily", label: "Daily" },
  { value: "@weekly", label: "Weekly" },
  { value: "*/15 * * * *", label: "Every 15 minutes" },
  { value: "0 9 * * *", label: "Daily at 09:00 UTC" },
];

function crewCanDispatch(crew: Crew | undefined) {
  return !!crew?.members?.some((m) => m.member_type === "pilot");
}

function canDispatchAssignee(value: string, crews: Crew[]) {
  return value.startsWith("pilot:") || (value.startsWith("crew:") && crewCanDispatch(crews.find((c) => `crew:${c.id}` === value)));
}

function assigneeInput(value: string, userId: string): { type: AssigneeType; id: string } {
  if (value === "me") return { type: "user", id: userId };
  const [type, id] = value.split(":") as [AssigneeType, string];
  return { type, id };
}

function formatPulseTime(value: string | null, fallback: string) {
  if (!value) return fallback;
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return fallback;
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function routineTrigger(routine: Routine) {
  return routine.metadata.trigger ?? { kind: "manual" as RoutineTriggerType };
}

function routineOperation(routine: Routine) {
  return routine.metadata.operation ?? {};
}

function routineContext(routine: Routine) {
  return typeof routine.operation_metadata.context === "string" ? routine.operation_metadata.context : "";
}

function routineAssigneeValue(routine: Routine, userId: string) {
  const assignee = routineOperation(routine).assignee;
  if (!assignee?.type || !assignee.id) return "me";
  if (assignee.type === "user" && assignee.id === userId) return "me";
  return `${assignee.type}:${assignee.id}`;
}

export function RoutinesView() {
  const app = useApp();
  const [editingRoutineId, setEditingRoutineId] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [missionId, setMissionId] = useState("");
  const [assignee, setAssignee] = useState("me");
  const [dispatchAfterPulse, setDispatchAfterPulse] = useState(true);
  const [priority, setPriority] = useState("0");
  const [triggerType, setTriggerType] = useState<RoutineTriggerType>("manual");
  const [cron, setCron] = useState("@daily");
  const [context, setContext] = useState("");
  const [requiredTags, setRequiredTags] = useState<string[]>([]);
  const [excludedTags, setExcludedTags] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [assets, setAssets] = useState<Asset[]>([]);
  const uploadRef = useRef<HTMLInputElement | null>(null);

  const mission = missionId || app.missions[0]?.id || "";
  const sortedCrews = [...app.crews].sort((a, b) => a.name.localeCompare(b.name));
  const sortedPilots = [...app.pilots].sort((a, b) => pilotLabel(a.kind).localeCompare(pilotLabel(b.kind)));
  const sortedMembers = app.members.filter((m) => m.id !== app.user.id).sort((a, b) => (a.name || a.email).localeCompare(b.name || b.email));
  const dispatchAvailable = canDispatchAssignee(assignee, app.crews);
  const autoDispatch = dispatchAvailable && dispatchAfterPulse;
  const canSave = !!title.trim() && !!mission && (triggerType === "manual" || !!cron.trim());
  const editing = editingRoutineId != null;
  const SaveIcon = editing ? Pencil : Plus;

  function setAssigneeAndDispatch(value: string) {
    setAssignee(value);
    setDispatchAfterPulse(canDispatchAssignee(value, app.crews));
  }

  function routineInput() {
    const contextText = context.trim();
    return {
      mission_id: mission,
      title: title.trim(),
      body: body.trim(),
      metadata: {
        trigger: { kind: triggerType, ...(triggerType === "schedule" ? { cron: cron.trim() } : {}) },
        operation: {
          start_immediately: autoDispatch,
          priority: Number(priority),
          assignee: assigneeInput(assignee, app.user.id),
          required_tags: requiredTags,
          excluded_tags: excludedTags,
        },
      },
      operation_metadata: contextText ? { context: contextText } : {},
    };
  }

  function resetForm() {
    setEditingRoutineId(null);
    setTitle("");
    setBody("");
    setMissionId("");
    setAssignee("me");
    setDispatchAfterPulse(true);
    setPriority("0");
    setTriggerType("manual");
    setCron("@daily");
    setContext("");
    setRequiredTags([]);
    setExcludedTags([]);
    setAssets([]);
  }

  function editRoutine(routine: Routine) {
    const trigger = routineTrigger(routine);
    const operation = routineOperation(routine);
    const nextAssignee = routineAssigneeValue(routine, app.user.id);
    setEditingRoutineId(routine.id);
    setTitle(routine.title);
    setBody(routine.body);
    setMissionId(routine.mission_id);
    setAssignee(nextAssignee);
    setDispatchAfterPulse(operation.start_immediately ?? canDispatchAssignee(nextAssignee, app.crews));
    setPriority(String(operation.priority ?? 0));
    setTriggerType((trigger.kind ?? "manual") === "schedule" ? "schedule" : "manual");
    setCron(trigger.cron ?? "@daily");
    setContext(routineContext(routine));
    setRequiredTags(operation.required_tags ?? []);
    setExcludedTags(operation.excluded_tags ?? []);
    setAssets([]);
  }

  async function save(e: React.FormEvent) {
    e.preventDefault();
    if (!canSave) return;
    setSaving(true);
    const input = routineInput();
    const routine = editingRoutineId ? await app.updateRoutine(editingRoutineId, input) : await app.createRoutine(input);
    setSaving(false);
    if (routine) resetForm();
  }

  async function onFiles(files: FileList | null) {
    const selected = Array.from(files ?? []);
    if (selected.length === 0 || uploading) return;
    setUploading(true);
    try {
      for (const file of selected) {
        const asset = await app.uploadAsset(file);
        if (asset) {
          setAssets((prev) => [...prev, asset]);
          setBody((prev) => appendAssetLink(prev, asset));
        }
      }
    } finally {
      setUploading(false);
      if (uploadRef.current) uploadRef.current.value = "";
    }
  }

  return (
    <div className="mx-auto grid h-full max-w-6xl gap-4 overflow-y-auto p-4 lg:grid-cols-[minmax(0,1fr)_23rem] lg:overflow-hidden">
      <Card className="flex min-h-0 flex-col">
        <CardHeader className="p-4 pb-3">
          <CardTitle className="flex items-center justify-between gap-3 text-base">
            <span className="flex items-center gap-2"><SECTION_ICONS.routines className="size-4" /> Routines</span>
            <span className="text-xs font-normal text-muted-foreground">{app.routines.length}</span>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col p-4 pt-0">
          <div className="min-h-0 flex-1 space-y-2 overflow-y-auto pr-1">
            {app.routines.map((routine) => <RoutineRow key={routine.id} routine={routine} editing={editingRoutineId === routine.id} onEdit={editRoutine} />)}
            {app.routines.length === 0 && <p className="py-2 text-sm text-muted-foreground">No routines yet.</p>}
          </div>
        </CardContent>
      </Card>

      <Card className="flex min-h-0 flex-col">
        <CardHeader className="p-4 pb-3">
          <CardTitle className="flex items-center justify-between gap-3 text-base">
            <span className="flex items-center gap-2">{editing ? <Pencil className="size-4" /> : <Plus className="size-4" />} {editing ? "Edit routine" : "New routine"}</span>
            {editing && (
              <Button type="button" variant="ghost" size="icon-sm" title="Cancel edit" aria-label="Cancel edit" onClick={resetForm}>
                <X />
              </Button>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent className="min-h-0 flex-1 overflow-y-auto p-4 pt-0">
          {app.missions.length === 0 ? (
            <p className="text-sm text-muted-foreground">Create a mission first.</p>
          ) : (
            <form id="routine-form" className="space-y-4" onSubmit={save}>
              <FormSection title="Pulse" icon={Activity}>
                <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Pulse title" />
                <div className="space-y-1.5">
                  <Textarea value={body} onChange={(e) => setBody(e.target.value)} placeholder="Operation prompt for each pulse" rows={3} />
                  <input ref={uploadRef} type="file" multiple className="sr-only" onChange={(e) => onFiles(e.target.files)} />
                  <div className="flex items-center gap-1">
                    <Button type="button" variant="ghost" size="icon-sm" className="text-muted-foreground" title="Upload files" aria-label="Upload files" disabled={uploading} onClick={() => uploadRef.current?.click()}>
                      {uploading ? <Loader2 className="size-3 animate-spin" /> : <Paperclip className="size-3" />}
                    </Button>
                  </div>
                  <AssetChipStrip assets={assets} onInsert={(asset) => setBody((prev) => appendAssetLink(prev, asset))} />
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label className="text-xs text-muted-foreground">Pulse</Label>
                    <Select value={triggerType} onValueChange={(value) => setTriggerType(value as RoutineTriggerType)}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="manual">Manual</SelectItem>
                        <SelectItem value="schedule">Schedule</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  {triggerType === "schedule" && (
                    <div className="space-y-1.5 sm:col-span-2">
                      <Label className="text-xs text-muted-foreground">Schedule</Label>
                      <Select value={CRON_PRESETS.some((preset) => preset.value === cron) ? cron : "custom"} onValueChange={(value) => { if (value !== "custom") setCron(value); }}>
                        <SelectTrigger><SelectValue /></SelectTrigger>
                        <SelectContent>
                          {CRON_PRESETS.map((preset) => <SelectItem key={preset.value} value={preset.value}>{preset.label}</SelectItem>)}
                          <SelectItem value="custom">Custom</SelectItem>
                        </SelectContent>
                      </Select>
                      <Input value={cron} onChange={(e) => setCron(e.target.value)} placeholder="0 9 * * *" />
                      <div className="rounded-md border border-border bg-muted/30 p-2 text-[11px] leading-snug text-muted-foreground">
                        <div className="font-mono">@hourly · @daily · @weekly</div>
                        <div className="font-mono">minute hour day month weekday</div>
                        <div>Fields support *, numbers, and */n. Times are UTC.</div>
                      </div>
                    </div>
                  )}
                </div>
              </FormSection>

              <FormSection title="Dispatch" icon={UserRound}>
                <div className="space-y-1.5">
                  <Label className="text-xs text-muted-foreground">Mission</Label>
                  <Select value={mission} onValueChange={setMissionId}>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent>
                      {app.missions.map((m) => <SelectItem key={m.id} value={m.id}><span className="font-mono text-xs">{m.key}</span> - {m.name}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label className="text-xs text-muted-foreground">Assignee</Label>
                  <Select value={assignee} onValueChange={setAssigneeAndDispatch}>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="me">{userLabel(app.user)}</SelectItem>
                      {sortedCrews.map((c) => <SelectItem key={`c${c.id}`} value={`crew:${c.id}`}><CrewOption crew={c} /></SelectItem>)}
                      {sortedPilots.map((p) => <SelectItem key={`p${p.kind}`} value={`pilot:${p.kind}`} disabled={p.rovers === 0}><PilotOption kind={p.kind} unavailable={p.rovers === 0} /></SelectItem>)}
                      {sortedMembers.map((m) => <SelectItem key={`u${m.id}`} value={`user:${m.id}`}><span className="flex items-center gap-2"><UserRound className="size-3.5" /> {m.name || m.email}</span></SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label className="text-xs text-muted-foreground">Priority</Label>
                    <Select value={priority} onValueChange={setPriority}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>
                        {PRIORITY.map((p, i) => (
                          <SelectItem key={i} value={String(i)}>
                            <span className="flex items-center gap-2"><PriorityIcon level={i} className="size-3.5" /> {p.label}</span>
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <label className="flex items-end justify-between gap-3 pb-2 text-xs">
                    <span>
                      <span className="font-medium text-foreground">Dispatch after pulse</span>
                      <span className="block text-muted-foreground">{dispatchAvailable ? "Auto" : "Backlog"}</span>
                    </span>
                    <input
                      type="checkbox"
                      className="peer sr-only"
                      checked={autoDispatch}
                      disabled={!dispatchAvailable}
                      onChange={(e) => setDispatchAfterPulse(e.target.checked)}
                    />
                    <span className="relative h-5 w-9 shrink-0 rounded-full bg-muted transition after:absolute after:left-0.5 after:top-0.5 after:size-4 after:rounded-full after:bg-background after:shadow after:transition after:content-[''] peer-checked:bg-brand peer-checked:after:translate-x-4 peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-disabled:opacity-50" />
                  </label>
                </div>
              </FormSection>

              <FormSection title="Context" icon={Tags}>
                <div className="space-y-1.5">
                  <Textarea value={context} onChange={(e) => setContext(e.target.value)} placeholder="Context" rows={3} />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-xs text-muted-foreground">Required rover tags</Label>
                  <TagEditor tags={requiredTags} onChange={setRequiredTags} placeholder="tag" />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-xs text-muted-foreground">Excluded rover tags</Label>
                  <TagEditor tags={excludedTags} onChange={setExcludedTags} placeholder="tag" />
                </div>
              </FormSection>
            </form>
          )}
        </CardContent>
        {app.missions.length > 0 && (
          <CardFooter className="border-t border-border p-4">
            <Button type="submit" form="routine-form" className="w-full" disabled={saving || !canSave}><SaveIcon /> {saving ? "Saving..." : editing ? "Save changes" : "Save routine"}</Button>
          </CardFooter>
        )}
      </Card>
    </div>
  );
}

function FormSection({ title, icon: Icon, children }: { title: string; icon: LucideIcon; children: React.ReactNode }) {
  return (
    <section className="space-y-2 border-t border-border pt-3 first:border-t-0 first:pt-0">
      <div className="flex items-center gap-2 text-xs font-semibold text-muted-foreground">
        <Icon className="size-3.5" />
        {title}
      </div>
      <div className="space-y-2">{children}</div>
    </section>
  );
}

function RoutineRow({ routine, editing, onEdit }: { routine: Routine; editing: boolean; onEdit: (routine: Routine) => void }) {
  const app = useApp();
  const [pulsing, setPulsing] = useState(false);
  const mission = app.missions.find((m) => m.id === routine.mission_id);
  const trigger = routineTrigger(routine);
  const operation = routineOperation(routine);
  const requiredTags = operation.required_tags ?? [];
  const excludedTags = operation.excluded_tags ?? [];
  const context = routineContext(routine);
  const triggerType = trigger.kind ?? "manual";
  const priority = operation.priority ?? 0;

  async function pulse() {
    setPulsing(true);
    try {
      const pulse = await app.pulseRoutine(routine.id);
      if (pulse?.operation_id) app.openOperation(pulse.operation_id);
    } finally {
      setPulsing(false);
    }
  }

  return (
    <div className={`rounded-md border p-3 text-sm ${editing ? "border-brand" : "border-border"}`}>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <div className="truncate font-medium" title={routine.title}>{routine.title}</div>
            <Badge variant={triggerType === "schedule" ? "brand" : "secondary"} className="shrink-0 text-[10px]">
              {triggerType === "schedule" ? "Schedule" : "Manual"}
            </Badge>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <span className="min-w-0 truncate">
              {mission ? <><span className="font-mono">{mission.key}</span> - {mission.name}</> : "Mission"}
            </span>
            <span>{routineAssigneeLabel(routine, app)}</span>
            <span>{operation.start_immediately ?? true ? "Auto-dispatch" : "Backlog"}</span>
            <span className="flex items-center gap-1"><PriorityIcon level={priority} className="size-3.5" /> {PRIORITY[priority]?.label ?? "Priority"}</span>
          </div>
          <div className="mt-2 grid gap-1 text-[11px] text-muted-foreground sm:grid-cols-2">
            <span className="min-w-0 truncate">{triggerType === "schedule" ? `Schedule ${trigger.cron ?? ""}` : "Manual Pulse"}</span>
            <span className="min-w-0 truncate">Next Pulse {formatPulseTime(routine.next_pulse_at, triggerType === "schedule" ? "Pending" : "Manual")}</span>
            <span className="min-w-0 truncate sm:col-span-2">Last Pulsed {formatPulseTime(routine.last_pulsed_at, "Never")}</span>
          </div>
        </div>
        <Button variant="ghost" size="icon-sm" title="Edit routine" aria-label="Edit routine" onClick={() => onEdit(routine)}><Pencil /></Button>
        <Button variant="ghost" size="icon-sm" title="Pulse routine" aria-label="Pulse routine" onClick={pulse} disabled={pulsing}><Play /></Button>
        <Button variant="ghost" size="icon-sm" title="Delete routine" aria-label="Delete routine" onClick={() => app.deleteRoutine(routine.id)}><Trash2 /></Button>
      </div>

      {(requiredTags.length > 0 || excludedTags.length > 0) && (
        <div className="mt-2 space-y-1">
          {requiredTags.length > 0 && (
            <div className="flex items-center gap-2">
              <span className="w-14 shrink-0 text-[11px] uppercase text-muted-foreground">Require</span>
              <TagList tags={requiredTags} />
            </div>
          )}
          {excludedTags.length > 0 && (
            <div className="flex items-center gap-2">
              <span className="w-14 shrink-0 text-[11px] uppercase text-muted-foreground">Exclude</span>
              <TagList tags={excludedTags} />
            </div>
          )}
        </div>
      )}
      {context && <p className="mt-2 line-clamp-2 text-xs text-muted-foreground">Context: {context}</p>}
      {routine.body && <p className="mt-2 line-clamp-2 text-xs text-muted-foreground">{routine.body}</p>}
    </div>
  );
}

function routineAssigneeLabel(routine: Routine, app: ReturnType<typeof useApp>) {
  const assignee = routineOperation(routine).assignee;
  if (assignee?.type === "pilot") return pilotLabel(assignee.id ?? "");
  if (assignee?.type === "crew") return app.crews.find((c) => c.id === assignee.id)?.name ?? "Crew";
  if (assignee?.type === "user") return memberLabel(assignee.id ?? "", app.user, app.members);
  return "Unassigned";
}
