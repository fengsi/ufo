"use client";

import { useEffect, useState } from "react";
import { Bot, Plus, Shield, Trash2, UserRound, X } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { PilotIcon } from "@/components/pilot-icon";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { pilotLabel, userLabel } from "@/lib/labels";
import { SECTION_ICONS } from "@/lib/section-icons";
import { cn } from "@/lib/utils";
import type { Crew, CrewMember, Pilot } from "@/lib/types";

export function CrewsView() {
  const app = useApp();
  const [crewName, setCrewName] = useState("");
  const canManage = app.myRole === "owner" || app.myRole === "admin";

  return (
    <div className="mx-auto grid h-full max-w-5xl gap-4 overflow-y-auto p-4 lg:grid-cols-[minmax(0,1fr)_20rem] lg:overflow-hidden">
      <Card className="flex min-h-0 flex-col">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base"><SECTION_ICONS.crews className="size-4" /> Crews</CardTitle>
        </CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col space-y-3">
          <p className="text-xs text-muted-foreground">Crews are dispatch groups. Staff them with people and pilots, then assign operations to the crew.</p>
          {canManage && (
            <form
              className="flex gap-2"
              onSubmit={(e) => { e.preventDefault(); if (crewName.trim()) { app.addCrew(crewName); setCrewName(""); } }}
            >
              <Input value={crewName} onChange={(e) => setCrewName(e.target.value)} placeholder="New crew name" className="flex-1" />
              <Button type="submit"><Plus /> Crew</Button>
            </form>
          )}
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto pr-1">
            {app.crews.length === 0 && <p className="text-sm text-muted-foreground">{canManage ? "No crews yet. Create one, then add people and pilots to it." : "No crews yet."}</p>}
            {app.crews.map((c) => <CrewCard key={c.id} crew={c} canManage={canManage} />)}
          </div>
        </CardContent>
      </Card>

      <Card className="flex min-h-0 flex-col">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base"><Bot className="size-4" /> Pilots</CardTitle>
        </CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col space-y-3">
          <p className="text-xs text-muted-foreground">Each pilot shows how many enrolled rovers it can start, and how many are online.</p>
          {app.pilots.length > 0 && (
            <div className="grid grid-cols-[minmax(0,1fr)_6.75rem] px-2 text-[9px] font-medium uppercase text-muted-foreground">
              <span>Pilot</span>
              <span className="grid grid-cols-[1fr_1px_1fr] items-center gap-1.5 px-1.5 text-center">
                <span>Online</span>
                <span aria-hidden />
                <span>Enrolled</span>
              </span>
            </div>
          )}
          <ul className="min-h-0 flex-1 space-y-1 overflow-y-auto pr-1">
            {app.pilots.map((p) => (
              <li key={p.kind} className="flex items-center gap-2 rounded-md px-2 py-1 text-sm">
                <span className={cn("flex min-w-0 flex-1 items-center gap-2", p.rovers === 0 && "opacity-50")}>
                  <PilotIcon kind={p.kind} />
                  <span className="truncate">{pilotLabel(p.kind)}</span>
                </span>
                <PilotAvailability pilot={p} />
              </li>
            ))}
            {app.pilots.length === 0 && <p className="text-sm text-muted-foreground">No pilots yet. Enroll a rover to make a pilot available.</p>}
          </ul>
        </CardContent>
      </Card>
    </div>
  );
}

function PilotAvailability({ pilot }: { pilot: Pilot }) {
  const unavailable = pilot.rovers === 0;
  if (unavailable) {
    return (
      <span
        aria-label="0 online, 0 enrolled"
        className="inline-flex h-5 w-[6.75rem] shrink-0 items-center justify-center rounded-full border border-destructive/20 bg-destructive/5 px-1.5 text-[10px] font-medium uppercase text-destructive/75"
      >
        no rovers
      </span>
    );
  }
  const label = `${pilot.online_rovers} online, ${pilot.rovers} enrolled`;
  return (
    <span
      aria-label={label}
      className="inline-grid h-5 w-[6.75rem] shrink-0 grid-cols-[1fr_1px_1fr] items-center gap-1.5 rounded-full border border-border bg-muted/30 px-1.5 text-[11px] tabular-nums"
    >
      <span className="grid min-w-0 grid-cols-[0.375rem_1fr] items-center gap-1 font-medium text-success">
        <span className="size-1.5 rounded-full bg-success" aria-hidden />
        <span className="text-right">{displayCount(pilot.online_rovers)}</span>
      </span>
      <span className="h-3 w-px bg-border" aria-hidden />
      <span className="grid min-w-0 grid-cols-[0.375rem_1fr] items-center gap-1 font-medium text-info">
        <span className="size-1.5 rounded-full bg-info" aria-hidden />
        <span className="text-right">{displayCount(pilot.rovers)}</span>
      </span>
    </span>
  );
}

function displayCount(value: number) {
  return value > 99 ? "99+" : String(value);
}

function CrewName({ id, name, canManage, onRename }: { id: string; name: string; canManage: boolean; onRename: (id: string, name: string) => void }) {
  const [value, setValue] = useState(name);
  const [editing, setEditing] = useState(false);
  useEffect(() => setValue(name), [name]);
  const save = () => {
    const next = value.trim();
    if (next && next !== name) onRename(id, next);
    else setValue(name);
    setEditing(false);
  };
  if (!canManage) {
    return <span className="block h-7 w-full truncate px-1 text-sm font-medium text-foreground" title={name}>{name}</span>;
  }
  if (!editing) {
    return (
      <button type="button" className="block h-7 w-full truncate px-1 text-left text-sm font-medium text-foreground" title={name} onClick={() => setEditing(true)}>
        {name}
      </button>
    );
  }
  return (
    <Input
      autoFocus
      aria-label="Crew name"
      className="h-7 w-full border-transparent px-1 shadow-none"
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

function CrewCard({ crew, canManage }: { crew: Crew; canManage: boolean }) {
  const app = useApp();
  const members = crew.members ?? [];
  const sortedMembers = [...members].sort((a, b) => memberTypeRank(a) - memberTypeRank(b) || Number(b.role === "captain") - Number(a.role === "captain") || memberName(a, app).localeCompare(memberName(b, app)));
  const captain = sortedMembers.find((m) => m.role === "captain");
  const captainValue = captain ? memberValue(captain, app.user.id) : "";

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 space-y-0 py-3">
        <CardTitle className="min-w-0 flex-1 text-sm"><CrewName id={crew.id} name={crew.name} canManage={canManage} onRename={app.renameCrew} /></CardTitle>
        {canManage && <Button variant="ghost" size="icon-sm" onClick={() => app.delCrew(crew.id)}><Trash2 /></Button>}
      </CardHeader>
      <CardContent className="space-y-2">
        {members.length === 0 && <p className="text-xs text-muted-foreground">{canManage ? "No members yet — add people and pilots below." : "No members yet."}</p>}
        {canManage && members.length > 0 && (
          <div className="flex items-center gap-2 rounded-md border border-border/70 bg-muted/20 px-2 py-2">
            <div className="flex w-24 items-center gap-1.5 text-xs font-medium uppercase text-muted-foreground">
              <Shield className="size-3.5" /> Captain
            </div>
            <Select value={captainValue} onValueChange={(v) => app.addMember(crew.id, v, "captain", app.user.id)}>
              <SelectTrigger className="h-8 flex-1 text-xs"><SelectValue placeholder="Select captain" /></SelectTrigger>
              <SelectContent>
                {sortedMembers.map((m) => (
                  <SelectItem key={`${m.member_type}${m.member_id}`} value={memberValue(m, app.user.id)}>
                    <span className="flex items-center gap-2">
                      {m.member_type === "pilot" ? <PilotIcon kind={m.member_id} /> : <UserRound className="size-4 text-muted-foreground" />}
                      {memberName(m, app)}
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}
        <ul className="space-y-1">
          {sortedMembers.map((m) => <MemberRow key={`${m.member_type}${m.member_id}`} crewId={crew.id} m={m} canManage={canManage} />)}
        </ul>
        {canManage && (
          <Select value="" onValueChange={(v) => app.addMember(crew.id, v, "member", app.user.id)}>
            <SelectTrigger className="mt-1 h-8 text-xs"><SelectValue placeholder="+ Add a person or pilot…" /></SelectTrigger>
            <SelectContent>
              <SelectItem value="me">🧑 {userLabel(app.user)}</SelectItem>
              {app.members.filter((m) => m.id !== app.user.id).map((m) => (
                <SelectItem key={`u${m.id}`} value={`user:${m.id}`}>🧑 {m.name || m.email}</SelectItem>
              ))}
              {app.pilots.map((p) => (
                <SelectItem key={p.kind} value={`pilot:${p.kind}`} disabled={p.rovers === 0}>
                  <span className="flex items-center gap-2"><PilotIcon kind={p.kind} /> {pilotLabel(p.kind)}{p.rovers === 0 && " (no rover)"}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      </CardContent>
    </Card>
  );
}

function memberValue(m: CrewMember, userId: string) {
  return m.member_type === "user" && m.member_id === userId ? "me" : `${m.member_type}:${m.member_id}`;
}

function memberTypeRank(m: CrewMember) {
  return m.member_type === "user" ? 0 : 1;
}

function memberName(m: CrewMember, app: ReturnType<typeof useApp>) {
  if (m.member_type === "pilot") return pilotLabel(m.member_id);
  if (m.member_id === app.user.id) return userLabel(app.user);
  const u = app.members.find((x) => x.id === m.member_id);
  return u?.name || u?.email || "member";
}

function MemberRow({ crewId, m, canManage }: { crewId: string; m: CrewMember; canManage: boolean }) {
  const app = useApp();
  const isPilot = m.member_type === "pilot";
  const name = memberName(m, app);
  return (
    <li className="flex items-center gap-2 rounded-md px-2 py-1 text-sm hover:bg-muted/50">
      {isPilot ? <PilotIcon kind={m.member_id} /> : <UserRound className="size-4 text-muted-foreground" />}
      {isPilot ? (
        <span className="flex-1 truncate">{name}</span>
      ) : (
        <button type="button" className="min-w-0 flex-1 truncate text-left hover:underline" onClick={() => app.openUser(m.member_id)}>
          {name}
        </button>
      )}
      {m.role === "captain" && <Badge variant="secondary" className="gap-1 text-[10px]"><Shield className="size-3.5" /> Captain</Badge>}
      <span className="text-[10px] uppercase text-muted-foreground">{isPilot ? "pilot" : "person"}</span>
      {canManage && <Button variant="ghost" size="icon-sm" onClick={() => app.removeMember(crewId, m.member_type, m.member_id)}><X /></Button>}
    </li>
  );
}
