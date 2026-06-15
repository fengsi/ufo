"use client";

import { useState } from "react";
import { Bot, Plus, Star, Trash2, UserRound, X } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { PilotIcon } from "@/components/pilot-icon";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { pilotLabel } from "@/lib/labels";
import type { Crew, CrewMember } from "@/lib/types";

export function CrewsView() {
  const app = useApp();
  const [crewName, setCrewName] = useState("");
  const [pilotName, setPilotName] = useState("");
  const [kind, setKind] = useState("claude");

  return (
    <div className="mx-auto max-w-3xl space-y-8 p-4">
      {/* Primary: crews are teams of people + pilots that run operations. */}
      <section className="space-y-3">
        <div>
          <h2 className="text-sm font-semibold">Crews</h2>
          <p className="text-xs text-muted-foreground">Teams that run operations — each staffed with people and pilots. An operation assigned to a crew runs on whichever member is free.</p>
        </div>
        <form
          className="flex gap-2"
          onSubmit={(e) => { e.preventDefault(); if (crewName.trim()) { app.addCrew(crewName); setCrewName(""); } }}
        >
          <Input value={crewName} onChange={(e) => setCrewName(e.target.value)} placeholder="New crew name" className="flex-1" />
          <Button type="submit"><Plus /> Crew</Button>
        </form>
        {app.crews.length === 0 && <p className="text-sm text-muted-foreground">No crews yet. Create one, then add people and pilots to it.</p>}
        {app.crews.map((c) => <CrewCard key={c.id} crew={c} />)}
      </section>

      {/* Secondary: the pilot pool crews are staffed from. */}
      <section className="space-y-3 border-t border-border pt-6">
        <div>
          <h2 className="flex items-center gap-1.5 text-sm font-semibold text-muted-foreground"><Bot className="size-4" /> Pilot pool</h2>
          <p className="text-xs text-muted-foreground">Pilots available to staff crews — or assign to an operation directly.</p>
        </div>
        <form
          className="flex gap-2"
          onSubmit={(e) => { e.preventDefault(); if (pilotName.trim()) { app.addPilot(pilotName, kind); setPilotName(""); } }}
        >
          <Input value={pilotName} onChange={(e) => setPilotName(e.target.value)} placeholder="Pilot name" className="flex-1" />
          <Select value={kind} onValueChange={setKind}>
            <SelectTrigger className="w-32"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="claude"><span className="flex items-center gap-2"><PilotIcon kind="claude" /> Claude</span></SelectItem>
              <SelectItem value="codex"><span className="flex items-center gap-2"><PilotIcon kind="codex" /> Codex</span></SelectItem>
            </SelectContent>
          </Select>
          <Button type="submit" size="icon"><Plus /></Button>
        </form>
        <div className="flex flex-wrap gap-2">
          {app.pilots.map((a) => (
            <span key={a.id} className="group inline-flex items-center gap-1.5 rounded-full border border-border bg-card py-1 pl-2.5 pr-1 text-xs">
              <PilotIcon kind={a.kind} /> {a.name} <span className="text-muted-foreground">{pilotLabel(a.kind)}</span>
              <button className="rounded-full p-0.5 text-muted-foreground hover:text-destructive" onClick={() => app.delPilot(a.id)}><X className="size-3" /></button>
            </span>
          ))}
          {app.pilots.length === 0 && <p className="text-sm text-muted-foreground">No pilots yet.</p>}
        </div>
      </section>
    </div>
  );
}

function CrewCard({ crew }: { crew: Crew }) {
  const app = useApp();
  const members = crew.members ?? [];

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 space-y-0 py-3">
        <CardTitle className="text-sm">{crew.name}</CardTitle>
        <Button variant="ghost" size="icon-sm" onClick={() => app.delCrew(crew.id)}><Trash2 /></Button>
      </CardHeader>
      <CardContent className="space-y-2">
        {members.length === 0 && <p className="text-xs text-muted-foreground">No members yet — add people and pilots below.</p>}
        <ul className="space-y-1">
          {members.map((m) => <MemberRow key={`${m.member_type}${m.member_id}`} crewId={crew.id} m={m} />)}
        </ul>
        <Select value="" onValueChange={(v) => app.addMember(crew.id, v, "member", app.user.id)}>
          <SelectTrigger className="mt-1 h-8 text-xs"><SelectValue placeholder="+ Add a person or pilot…" /></SelectTrigger>
          <SelectContent>
            <SelectItem value="me">🧑 You</SelectItem>
            {app.members.filter((m) => m.id !== app.user.id).map((m) => (
              <SelectItem key={`u${m.id}`} value={`user:${m.id}`}>🧑 {m.name || m.email}</SelectItem>
            ))}
            {app.pilots.map((a) => <SelectItem key={a.id} value={`pilot:${a.id}`}><span className="flex items-center gap-2"><PilotIcon kind={a.kind} /> {a.name}</span></SelectItem>)}
          </SelectContent>
        </Select>
      </CardContent>
    </Card>
  );
}

function MemberRow({ crewId, m }: { crewId: string; m: CrewMember }) {
  const app = useApp();
  const isPilot = m.member_type === "pilot";
  const pilot = isPilot ? app.pilots.find((a) => a.id === m.member_id) : null;
  let name: string;
  if (isPilot) {
    name = pilot?.name ?? "pilot";
  } else if (m.member_id === app.user.id) {
    name = "You";
  } else {
    const u = app.members.find((x) => x.id === m.member_id);
    name = u?.name || u?.email || "member";
  }
  return (
    <li className="flex items-center gap-2 rounded-md px-2 py-1 text-sm hover:bg-muted/50">
      {pilot ? <PilotIcon kind={pilot.kind} /> : isPilot ? <Bot className="size-4 text-brand" /> : <UserRound className="size-4 text-muted-foreground" />}
      <span className="flex-1 truncate">{name}</span>
      {m.role === "leader" && <Badge variant="secondary" className="gap-1 text-[10px]"><Star className="size-2.5" /> lead</Badge>}
      <span className="text-[10px] uppercase text-muted-foreground">{isPilot ? "pilot" : "person"}</span>
      <Button variant="ghost" size="icon-sm" onClick={() => app.removeMember(crewId, m.member_type, m.member_id)}><X /></Button>
    </li>
  );
}
