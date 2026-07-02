"use client";

import { useEffect, useState } from "react";
import { Check, Circle, CircleDot, CircleOff, X, type LucideIcon } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TagEditor, TagList } from "@/components/tag-editor";
import { PilotIcon } from "@/components/pilot-icon";
import { pilotLabel } from "@/lib/labels";
import { SECTION_ICONS } from "@/lib/section-icons";
import { cn } from "@/lib/utils";

const ROVER_STATUS: Record<string, { label: string; icon: LucideIcon; color: string }> = {
  online: { label: "Online", icon: Circle, color: "text-success" },
  full: { label: "Full", icon: CircleDot, color: "text-warning" },
  offline: { label: "Offline", icon: CircleOff, color: "text-muted-foreground" },
};
const EXPIRY_OPTIONS = [
  ["1", "1 day"],
  ["3", "3 days"],
  ["7", "7 days"],
  ["15", "15 days"],
  ["30", "30 days"],
  ["90", "90 days"],
  ["180", "180 days"],
  ["365", "1 year"],
  ["never", "Never"],
] as const;
const MAX_ROVER_UNITS = 100;
const MAX_ENROLLMENT_CODE_USES = 100;
const WEB_ENROLLMENT_CODE_RE = /^[a-f0-9]{40}$/;

function shortDate(value?: string | null) {
  return value ? new Date(value).toLocaleDateString() : "Never";
}

function expiryISO(value: string) {
  if (value === "never") return undefined;
  const days = Number(value);
  return new Date(Date.now() + days * 24 * 60 * 60 * 1000).toISOString();
}

function clampUnits(value: unknown) {
  const n = typeof value === "number" ? value : Number.parseInt(String(value ?? ""), 10);
  return Number.isInteger(n) && n > 0 ? Math.min(n, MAX_ROVER_UNITS) : 1;
}

function metadataTags(metadata: Record<string, unknown> | undefined) {
  const tags = metadata?.tags;
  return Array.isArray(tags) ? tags.filter((tag): tag is string => typeof tag === "string" && tag.trim() !== "") : [];
}

type PendingEnrollment = {
  code: string;
  name: string;
  units: string;
  tags: string[];
  fleet: string;
};

function pendingEnrollmentFromHash(fleet: string): PendingEnrollment | null {
  if (typeof window === "undefined" || !window.location.hash.startsWith("#enroll=")) return null;
  const params = new URLSearchParams(window.location.hash.slice(1));
  const code = params.get("enroll")?.trim() ?? "";
  if (!code) return null;
  if (!WEB_ENROLLMENT_CODE_RE.test(code)) {
    clearPendingEnrollmentHash();
    return null;
  }
  const tags = params.getAll("tag").map((tag) => tag.trim()).filter(Boolean);
  return {
    code,
    name: params.get("name")?.trim() ?? "",
    units: params.get("units")?.trim() ?? "",
    tags,
    fleet,
  };
}

function clearPendingEnrollmentHash() {
  if (typeof window === "undefined") return;
  window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}`);
}

function RoverName({ id, name, onRename }: { id: string; name: string; onRename: (id: string, name: string) => void }) {
  const [value, setValue] = useState(name);
  const [editing, setEditing] = useState(false);
  useEffect(() => setValue(name), [name]);
  const save = () => {
    const next = value.trim();
    if (next && next !== name) onRename(id, next);
    else setValue(name);
    setEditing(false);
  };
  if (!editing) {
    return (
      <button type="button" className="h-7 w-44 truncate px-1 text-left text-sm font-medium text-foreground" title={name} onClick={() => setEditing(true)}>
        {name}
      </button>
    );
  }
  return (
    <Input
      autoFocus
      aria-label="Rover name"
      className="h-7 w-44 border-transparent px-1 shadow-none"
      value={value}
      onBlur={save}
      onChange={(e) => setValue(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") e.currentTarget.blur();
        if (e.key === "Escape") {
          setValue(name);
          setEditing(false);
        }
      }}
    />
  );
}

function RoverUnits({ id, units, running, onSet }: { id: string; units: number; running: number; onSet: (id: string, units: number) => void }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(String(units));
  const label = `${units} ${units === 1 ? "unit" : "units"}`;
  useEffect(() => setValue(String(units)), [units]);
  const save = () => {
    const next = Number(value);
    if (Number.isInteger(next) && next >= 1 && next <= MAX_ROVER_UNITS && next !== units) onSet(id, next);
    else setValue(String(units));
    setEditing(false);
  };
  if (!editing) {
    return (
      <button
        type="button"
        className="h-7 w-20 truncate px-1 text-left text-xs text-muted-foreground"
        title={`${running} of ${label} running`}
        onClick={() => setEditing(true)}
      >
        {label}
      </button>
    );
  }
  return (
    <Input
      autoFocus
      type="number"
      min={1}
      max={MAX_ROVER_UNITS}
      aria-label="Rover units"
      className="h-7 w-20 border-transparent px-1 text-xs shadow-none"
      value={value}
      onBlur={save}
      onChange={(e) => setValue(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") e.currentTarget.blur();
        if (e.key === "Escape") {
          setValue(String(units));
          setEditing(false);
        }
      }}
    />
  );
}

export function RoversView() {
  const app = useApp();
  const [activePendingID, setActivePendingID] = useState<string | null>(null);
  const [pendingFleet, setPendingFleet] = useState(app.fleet);
  const [pendingName, setPendingName] = useState("");
  const [pendingUnits, setPendingUnits] = useState("1");
  const [pendingTags, setPendingTags] = useState<string[]>([]);
  const [revokingRover, setRevokingRover] = useState<{ id: string; name: string } | null>(null);
  const [enrollmentCodeName, setEnrollmentCodeName] = useState("");
  const [enrollmentCodeExpiry, setEnrollmentCodeExpiry] = useState("30");
  const [enrollmentCodeUses, setEnrollmentCodeUses] = useState("");
  const uses = Number(enrollmentCodeUses);
  const hasMultiUseFields = enrollmentCodeName.trim() !== "" || enrollmentCodeUses !== "";
  const canCreateCode = !hasMultiUseFields || (enrollmentCodeName.trim() !== "" && Number.isInteger(uses) && uses >= 2 && uses <= MAX_ENROLLMENT_CODE_USES);
  const runningSlots = app.rovers.reduce((n, r) => n + (r.running_units ?? 0), 0);
  const totalSlots = app.rovers.reduce((n, r) => n + r.units, 0);
  const pendingApprovals = app.enrollmentCodes.filter((code) => code.kind === "web:pending");
  const enrollmentCodes = app.enrollmentCodes.filter((code) => code.kind === "code:approved" && code.fleet_id === app.fleet);
  const activePending = pendingApprovals.find((code) => code.id === activePendingID) ?? null;

  useEffect(() => {
    const next = pendingEnrollmentFromHash(app.fleet);
    if (next == null) return;
    const units = clampUnits(next.units);
    let canceled = false;
    app.savePendingRover(next.code, { name: next.name, units, tags: next.tags }).then((code) => {
      if (canceled || code == null) return;
      clearPendingEnrollmentHash();
      setActivePendingID(code.id);
      setPendingFleet(next.fleet);
      setPendingName(code.name || next.name);
      setPendingUnits(String(clampUnits(code.metadata?.units ?? units)));
      setPendingTags(metadataTags(code.metadata));
    });
    return () => {
      canceled = true;
    };
  }, [app.fleet, app.savePendingRover]);

  useEffect(() => {
    if (!activePending) return;
    setPendingFleet(app.fleet);
    setPendingName(activePending.name);
    setPendingUnits(String(clampUnits(activePending.metadata?.units)));
    setPendingTags(metadataTags(activePending.metadata));
  }, [activePending?.id, app.fleet]);

  const approvePendingEnrollment = async () => {
    if (!activePending) return;
    const units = clampUnits(pendingUnits);
    const ok = await app.approvePendingRover(activePending.id, {
      fleetId: pendingFleet,
      name: pendingName,
      units,
      tags: pendingTags,
    });
    if (!ok) return;
    if (pendingFleet !== app.fleet) app.switchFleet(pendingFleet);
    setActivePendingID(null);
  };

  const denyPendingEnrollment = async () => {
    if (!activePending) return;
    const ok = await app.denyPendingRover(activePending.id);
    if (!ok) return;
    setActivePendingID(null);
  };

  return (
    <div className="mx-auto flex h-full max-w-3xl flex-col p-4">
      <Dialog open={activePending != null} onOpenChange={(open) => { if (!open) setActivePendingID(null); }}>
        <DialogContent
          onPointerDownOutside={(e) => e.preventDefault()}
          onInteractOutside={(e) => e.preventDefault()}
        >
          <DialogHeader>
            <DialogTitle>Approve rover enrollment</DialogTitle>
            <DialogDescription>This pending rover will keep waiting until it is approved or denied.</DialogDescription>
          </DialogHeader>
          {activePending && (
            <div className="space-y-4">
              <div className="grid gap-3 sm:grid-cols-[1fr_8rem]">
                <label className="space-y-1.5 text-xs font-medium text-muted-foreground">
                  Name
                  <Input value={pendingName} onChange={(e) => setPendingName(e.target.value)} placeholder="rover" />
                </label>
                <label className="space-y-1.5 text-xs font-medium text-muted-foreground">
                  Units
                  <Input type="number" min={1} max={MAX_ROVER_UNITS} value={pendingUnits} onChange={(e) => setPendingUnits(e.target.value)} />
                </label>
              </div>
              <label className="space-y-1.5 text-xs font-medium text-muted-foreground">
                Fleet
                <Select value={pendingFleet} onValueChange={setPendingFleet}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {app.fleets.map((fleet) => <SelectItem key={fleet.id} value={fleet.id}>{fleet.name}</SelectItem>)}
                  </SelectContent>
                </Select>
              </label>
              <div className="space-y-1.5">
                <p className="text-xs font-medium text-muted-foreground">Tags</p>
                <TagEditor tags={pendingTags} onChange={setPendingTags} />
              </div>
              <div className="flex justify-end gap-2">
                <Button variant="destructive" onClick={denyPendingEnrollment}><X />Deny</Button>
                <Button variant="brand" disabled={!pendingFleet} onClick={approvePendingEnrollment}><Check />Approve</Button>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>
      <Dialog open={revokingRover != null} onOpenChange={(open) => { if (!open) setRevokingRover(null); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke rover</DialogTitle>
            <DialogDescription>
              Revoking {revokingRover?.name || "this rover"} deletes its connection token, disconnects it
              immediately, and removes its local enrollment. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setRevokingRover(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => { if (revokingRover) app.revokeRover(revokingRover.id); setRevokingRover(null); }}>Revoke</Button>
          </div>
        </DialogContent>
      </Dialog>
      <Card className="flex min-h-0 flex-1 flex-col">
        <CardHeader><CardTitle className="flex items-center gap-2 text-base"><SECTION_ICONS.rovers className="size-4" /> Rovers</CardTitle></CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col space-y-4">
          {pendingApprovals.length > 0 && (
            <div className="space-y-2 rounded-md border border-brand/30 bg-brand/10 p-3">
              <p className="text-sm font-medium text-foreground">Pending rover approvals</p>
              {pendingApprovals.map((code) => {
                const units = clampUnits(code.metadata?.units);
                const tags = metadataTags(code.metadata);
                return (
                  <div key={code.id} className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span className="min-w-0 truncate">
                      {code.name || "Unnamed rover"} · {units} {units === 1 ? "unit" : "units"}
                      {tags.length > 0 && ` · ${tags.join(", ")}`}
                      {` · expires ${shortDate(code.expires_at)}`}
                    </span>
                    <span className="flex items-center gap-2">
                      <Button size="sm" variant="outline" onClick={() => setActivePendingID(code.id)}>Review</Button>
                      <Button size="sm" variant="ghost" className="text-destructive" onClick={() => app.denyPendingRover(code.id)}>Deny</Button>
                    </span>
                  </div>
                );
              })}
            </div>
          )}
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={() => app.createEnrollmentCode({ name: enrollmentCodeName.trim(), expiresAt: expiryISO(enrollmentCodeExpiry), uses })} disabled={!canCreateCode}>Create enrollment code</Button>
            <Input value={enrollmentCodeName} onChange={(e) => setEnrollmentCodeName(e.target.value)} className="h-8 w-40" placeholder="Name" />
            <Select value={enrollmentCodeExpiry} onValueChange={setEnrollmentCodeExpiry}>
              <SelectTrigger className="h-8 w-32"><SelectValue /></SelectTrigger>
              <SelectContent>
                {EXPIRY_OPTIONS.map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}
              </SelectContent>
            </Select>
            <Input type="number" min={2} max={MAX_ENROLLMENT_CODE_USES} value={enrollmentCodeUses} onChange={(e) => setEnrollmentCodeUses(e.target.value)} className="h-8 w-28" placeholder="Uses" />
          </div>
          {app.newEnrollmentCode && (
            <pre className="overflow-x-auto rounded-md bg-foreground/90 p-3 text-xs text-background">
              {`UFO_ROVER_ENROLLMENT_CODE=${app.newEnrollmentCode} ufo rover enroll`}
            </pre>
          )}
          <div className="text-sm text-muted-foreground">
            Hub slots: <span className="font-medium text-foreground">{runningSlots}</span>/{totalSlots} active
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto divide-y divide-border pr-1">
            {app.rovers.map((r) => {
              const status = ROVER_STATUS[r.status] ?? { label: r.status, icon: Circle, color: "text-muted-foreground" };
              const StatusIcon = status.icon;
              return (
                <div key={r.id} className="space-y-2 py-3 text-sm">
                  <div className="flex items-center justify-between">
                    <span className="flex items-center gap-2">
                      <RoverName id={r.id} name={r.name} onRename={app.renameRover} />
                      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground" title={`Rover ${status.label}`}>
                        <StatusIcon aria-hidden className={cn("size-3.5", status.color)} />
                        {status.label}
                      </span>
                      <RoverUnits id={r.id} units={r.units} running={r.running_units ?? 0} onSet={app.setRoverUnits} />
                    </span>
                    <Button variant="ghost" size="sm" className="text-destructive" onClick={() => setRevokingRover({ id: r.id, name: r.name })}>Revoke</Button>
                  </div>
                  <div className="space-y-1 pl-4">
                    {(() => {
                      const auto = r.auto_tags ?? [];
                      const pilots = auto.filter((t) => t.startsWith("pilot:")).map((t) => t.slice(6));
                      const autoTags = auto.filter((t) => !t.startsWith("pilot:"));
                      return (
                        <div className="grid gap-x-3 gap-y-1.5 text-xs sm:grid-cols-[4.75rem_minmax(0,1fr)]">
                          {pilots.length > 0 && (
                            <>
                              <span className="pt-1 text-[11px] uppercase text-muted-foreground">pilots</span>
                              <PilotIconList pilots={pilots} />
                            </>
                          )}
                          {autoTags.length > 0 && (
                            <>
                              <span className="pt-1 text-[11px] uppercase text-muted-foreground">auto tags</span>
                              <TagList tags={autoTags} />
                            </>
                          )}
                          <span className="pt-1 text-[11px] uppercase text-muted-foreground">user tags</span>
                          <TagEditor tags={r.tags ?? []} onChange={(t) => app.setRoverTags(r.id, t)} />
                        </div>
                      );
                    })()}
                  </div>
                </div>
              );
            })}
            {app.rovers.length === 0 && <p className="py-2 text-sm text-muted-foreground">No rovers enrolled.</p>}
          </div>
          {enrollmentCodes.length > 0 && (
            <div className="space-y-1 border-t border-border pt-3">
              <p className="text-xs font-medium text-muted-foreground">Enrollment codes</p>
              {enrollmentCodes.map((t) => (
                <div key={t.id} className="flex items-center justify-between text-xs text-muted-foreground">
                  <span>{t.name || "one-time"} · {t.code} · {t.remaining_uses} {t.remaining_uses === 1 ? "use" : "uses"} left · created {shortDate(t.created_at)} · expires {shortDate(t.expires_at)}</span>
                  <Button variant="ghost" size="icon-sm" onClick={() => app.revokeEnrollmentCode(t.id)}>×</Button>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function PilotIconList({ pilots }: { pilots: string[] }) {
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-1.5">
      {pilots.map((kind) => (
        <span key={kind} className="inline-flex size-6 items-center justify-center rounded-full border border-border bg-muted/30 text-muted-foreground" aria-label={pilotLabel(kind)}>
          <PilotIcon kind={kind} size={14} />
        </span>
      ))}
    </span>
  );
}
