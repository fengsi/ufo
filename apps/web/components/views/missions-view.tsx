"use client";

import { useState } from "react";
import { FolderKanban, Pencil, Plus } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import type { Mission } from "@/lib/types";

// A mission groups operations within a fleet. Its key prefixes operation codes.
export function MissionsView() {
  const app = useApp();
  const [name, setName] = useState("");
  const [key, setKey] = useState("");

  const count = (missionId: string) => app.missionCounts[missionId] ?? 0;

  async function create(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !key.trim()) return;
    if (await app.addMission(name, key)) { setName(""); setKey(""); }
  }

  return (
    <div className="mx-auto max-w-3xl p-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base"><FolderKanban className="size-4" /> Missions</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <form className="flex gap-2" onSubmit={create}>
            <Input value={key} onChange={(e) => setKey(e.target.value.toUpperCase())} placeholder="KEY" className="w-24 font-mono uppercase" maxLength={8} />
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Mission name" className="flex-1" />
            <Button type="submit" size="icon"><Plus /></Button>
          </form>
          <div className="divide-y divide-border">
            {app.missions.map((m) => <MissionRow key={m.id} mission={m} count={count(m.id)} />)}
            {app.missions.length === 0 && <p className="py-2 text-sm text-muted-foreground">No missions yet.</p>}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function MissionRow({ mission, count }: { mission: Mission; count: number }) {
  const app = useApp();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(mission.name);
  const [key, setKey] = useState(mission.key);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !key.trim()) return;
    if (await app.updateMission(mission.id, name, key)) setEditing(false);
  }

  if (editing) {
    return (
      <form className="flex items-center gap-2 py-2" onSubmit={save}>
        <Input value={key} onChange={(e) => setKey(e.target.value.toUpperCase())} className="w-24 font-mono uppercase" maxLength={8} />
        <Input value={name} onChange={(e) => setName(e.target.value)} className="flex-1" />
        <Button type="submit" size="sm">Save</Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => setEditing(false)}>Cancel</Button>
      </form>
    );
  }

  return (
    <div className="flex items-center justify-between py-2 text-sm">
      <span className="flex items-center gap-2">
        <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs font-medium">{mission.key}</span>
        {mission.name}
      </span>
      <span className="flex items-center gap-3">
        <span className="text-xs text-muted-foreground">{count} operations</span>
        <Button variant="ghost" size="icon-sm" onClick={() => { setName(mission.name); setKey(mission.key); setEditing(true); }}><Pencil /></Button>
      </span>
    </div>
  );
}
