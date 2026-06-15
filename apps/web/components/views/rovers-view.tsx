"use client";

import { useState } from "react";
import { Server } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { TagEditor, TagList } from "@/components/tag-editor";
import { maxExpiryDate } from "@/lib/api";
import { cn } from "@/lib/utils";

const DOT: Record<string, string> = { online: "bg-success", busy: "bg-info", offline: "bg-muted-foreground" };

export function RoversView() {
  const app = useApp();
  const [enrollmentCodeExpiry, setEnrollmentCodeExpiry] = useState("");

  return (
    <div className="mx-auto max-w-3xl p-4">
      <Card>
        <CardHeader><CardTitle className="flex items-center gap-2 text-base"><Server className="size-4" /> Rovers</CardTitle></CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={() => app.addRover()}>Add rover</Button>
            <Input type="date" value={enrollmentCodeExpiry} max={maxExpiryDate()} onChange={(e) => setEnrollmentCodeExpiry(e.target.value)} className="h-8 w-40" />
            <Button size="sm" variant="outline" onClick={() => enrollmentCodeExpiry && app.createReusableEnrollmentCode(enrollmentCodeExpiry)}>Reusable enrollment code</Button>
          </div>
          {app.newEnrollmentCode && (
            <pre className="overflow-x-auto rounded-md bg-foreground/90 p-3 text-xs text-background">
              {`UFO_ENROLLMENT_CODE=${app.newEnrollmentCode} scripts/dev.sh rover`}
            </pre>
          )}
          <div className="divide-y divide-border">
            {app.rovers.map((r) => (
              <div key={r.id} className="space-y-2 py-3 text-sm">
                <div className="flex items-center justify-between">
                  <span className="flex items-center gap-2">
                    <span className={cn("size-2 rounded-full", DOT[r.status] ?? "bg-muted-foreground")} />
                    {r.name}
                    <span className="text-xs text-muted-foreground">{r.status}</span>
                  </span>
                  <Button variant="ghost" size="sm" className="text-destructive" onClick={() => app.revokeRover(r.id)}>Revoke</Button>
                </div>
                <div className="space-y-1 pl-4">
                  <div className="flex items-center gap-2">
                    <span className="w-12 shrink-0 text-[11px] uppercase text-muted-foreground">tags</span>
                    <TagEditor tags={r.tags ?? []} onChange={(t) => app.setRoverTags(r.id, t)} />
                  </div>
                  {(r.auto_tags ?? []).length > 0 && (
                    <div className="flex items-center gap-2">
                      <span className="w-12 shrink-0 text-[11px] uppercase text-muted-foreground">auto</span>
                      <TagList tags={r.auto_tags} />
                    </div>
                  )}
                </div>
              </div>
            ))}
            {app.rovers.length === 0 && <p className="py-2 text-sm text-muted-foreground">No rovers enrolled.</p>}
          </div>
          {app.enrollmentCodes.length > 0 && (
            <div className="space-y-1 border-t border-border pt-3">
              <p className="text-xs font-medium text-muted-foreground">Enrollment codes</p>
              {app.enrollmentCodes.map((t) => (
                <div key={t.id} className="flex items-center justify-between text-xs text-muted-foreground">
                  <span>{t.code.slice(0, 10)}… · {t.reusable ? "reusable" : "one-time"}</span>
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
