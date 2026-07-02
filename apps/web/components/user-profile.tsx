"use client";

import { useEffect, useState } from "react";
import { ArrowLeft } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { StatusIcon } from "@/components/status-icon";
import { getJSON, withFleet } from "@/lib/api";
import { initials, operationCode } from "@/lib/labels";
import type { Operation } from "@/lib/types";
import { cn } from "@/lib/utils";

export function UserProfileView() {
  const app = useApp();
  const profile = app.userProfile;
  const member = profile ? app.members.find((m) => m.id === profile.id) : null;
  const self = profile?.id === app.user.id;
  const [ops, setOps] = useState<Operation[] | null>(null);

  useEffect(() => {
    if (!profile) {
      setOps(null);
      return;
    }
    let canceled = false;
    getJSON<Operation[]>(withFleet(`/api/v1/operations?assignee_kind=user&assignee=${encodeURIComponent(profile.id)}&limit=20`, app.fleet)).then((work) => {
      if (!canceled) setOps(work ?? []);
    });
    return () => { canceled = true; };
  }, [profile, app.fleet]);

  return (
    <div className="flex h-full flex-col">
      <header className="ufo-topbar flex h-12 shrink-0 items-center gap-2 border-b border-border px-3">
        <Button type="button" variant="ghost" size="icon-sm" title="Back" aria-label="Back" onClick={() => app.openUser(null)}>
          <ArrowLeft className="size-4" />
        </Button>
        <h1 className="text-sm font-semibold">Profile</h1>
      </header>
      <div className="mx-auto w-full max-w-lg flex-1 space-y-4 overflow-y-auto p-4">
        {!profile ? (
          <p className="text-sm text-muted-foreground">Loading profile…</p>
        ) : (
          <>
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-3 text-base">
                  <Avatar className="size-10">
                    <AvatarFallback className="text-sm">{initials(profile.name || member?.email || "?")}</AvatarFallback>
                  </Avatar>
                  <span className="min-w-0 truncate">{profile.name || "Unnamed"}{self ? " (you)" : ""}</span>
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-3 text-sm">
                <div>
                  <p className="text-xs font-medium uppercase text-muted-foreground">Name</p>
                  <p className="mt-0.5">{profile.name || "—"}</p>
                </div>
                {member && (
                  <div>
                    <p className="text-xs font-medium uppercase text-muted-foreground">Fleet role</p>
                    <p className="mt-0.5 capitalize">{member.role}</p>
                  </div>
                )}
                {self && (
                  <p className="text-xs text-muted-foreground">Email and account settings are under Settings.</p>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="text-sm">Mutual fleets</CardTitle>
              </CardHeader>
              <CardContent>
                {profile.fleets.length === 0 ? (
                  <p className="text-xs text-muted-foreground">None in common.</p>
                ) : (
                  <ul className="space-y-1">
                    {profile.fleets.map((f) => (
                      <li key={f.id} className="flex items-center justify-between gap-2 text-sm">
                        <span className="truncate font-medium">{f.name}</span>
                        <span className="shrink-0 text-[10px] uppercase text-muted-foreground">{f.kind}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle className="text-sm">Operations involved</CardTitle>
              </CardHeader>
              <CardContent>
                {ops == null ? (
                  <p className="text-xs text-muted-foreground">Loading…</p>
                ) : ops.length === 0 ? (
                  <p className="text-xs text-muted-foreground">None in this fleet.</p>
                ) : (
                  <ul className="space-y-1">
                    {ops.map((op) => (
                      <li key={op.id}>
                        <button
                          type="button"
                          className="flex w-full items-center gap-2 rounded-md px-1.5 py-1.5 text-left text-sm hover:bg-muted/60"
                          onClick={() => app.openOperation(op.id)}
                        >
                          <StatusIcon status={op.status} className="size-3.5 shrink-0" />
                          <span className="shrink-0 font-mono text-[10px] uppercase text-muted-foreground">{operationCode(op, app.missions)}</span>
                          <span className={cn("min-w-0 flex-1 truncate", op.status === "done" || op.status === "canceled" ? "text-muted-foreground" : "font-medium")}>{op.title}</span>
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </CardContent>
            </Card>
          </>
        )}
      </div>
    </div>
  );
}
