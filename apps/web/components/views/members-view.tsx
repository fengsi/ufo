"use client";

import { useState } from "react";
import { Trash2, UserPlus } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { initials } from "@/lib/labels";

export function MembersView() {
  const app = useApp();
  const personal = app.fleets.find((f) => f.id === app.fleet)?.kind === "personal";
  const manager = (app.myRole === "owner" || app.myRole === "admin") && !personal;
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("member");

  async function sendInvite(e: React.FormEvent) {
    e.preventDefault();
    if (!email.trim()) return;
    if (await app.invite(email, role)) setEmail("");
  }

  return (
    <div className="mx-auto max-w-3xl space-y-4 p-4">
      <Card>
        <CardHeader><CardTitle className="text-base">Members</CardTitle></CardHeader>
        <CardContent className="space-y-1">
          {app.members.map((m) => (
            <div key={m.id} className="flex items-center gap-3 py-2">
              <Avatar className="size-7"><AvatarFallback>{initials(m.name || m.email)}</AvatarFallback></Avatar>
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{m.name || m.email}{m.id === app.user.id && " (you)"}</p>
                <p className="truncate text-xs text-muted-foreground">{m.email}</p>
              </div>
              {app.myRole === "owner" && m.id !== app.user.id ? (
                <Select value={m.role} onValueChange={(v) => app.setMemberRole(m.id, v)}>
                  <SelectTrigger className="h-7 w-28 text-xs"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="owner">owner</SelectItem>
                    <SelectItem value="admin">admin</SelectItem>
                    <SelectItem value="member">member</SelectItem>
                  </SelectContent>
                </Select>
              ) : (
                <Badge variant="secondary">{m.role}</Badge>
              )}
              {manager && m.role !== "owner" && m.id !== app.user.id && (
                <Button variant="ghost" size="icon-sm" onClick={() => app.removeFleetMember(m.id)}><Trash2 /></Button>
              )}
            </div>
          ))}
        </CardContent>
      </Card>

      {personal && (
        <p className="text-sm text-muted-foreground">This is your personal fleet — it&apos;s just you. Create a group fleet to invite teammates.</p>
      )}

      {manager && (
        <Card>
          <CardHeader><CardTitle className="flex items-center gap-2 text-base"><UserPlus className="size-4" /> Invite</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <form className="flex gap-2" onSubmit={sendInvite}>
              <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="teammate@email.com" className="flex-1" />
              <Select value={role} onValueChange={setRole}>
                <SelectTrigger className="w-28"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="member">member</SelectItem>
                  <SelectItem value="admin">admin</SelectItem>
                </SelectContent>
              </Select>
              <Button type="submit">Invite</Button>
            </form>
            <p className="text-xs text-muted-foreground">They join when they sign in with this email and accept the invitation.</p>
            {app.fleetInvites.length > 0 && (
              <div className="divide-y divide-border border-t border-border pt-2">
                {app.fleetInvites.map((inv) => (
                  <div key={inv.id} className="flex items-center justify-between py-2 text-sm">
                    <span>{inv.invitee_email} <Badge variant="secondary">{inv.role}</Badge> <span className="text-xs text-muted-foreground">pending</span></span>
                    <Button variant="ghost" size="sm" className="text-destructive" onClick={() => app.revokeInvite(inv.id)}>Revoke</Button>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
