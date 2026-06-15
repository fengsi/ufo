"use client";

import { Mail } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";

// Pending invitations addressed to the signed-in user (matched by email).
export function InviteBanner() {
  const app = useApp();
  if (app.myInvites.length === 0) return null;
  return (
    <div className="space-y-1 border-b border-border bg-brand/5 px-4 py-2">
      {app.myInvites.map((inv) => (
        <div key={inv.id} className="flex items-center gap-2 text-sm">
          <Mail className="size-4 text-brand" />
          <span>You&apos;re invited to <span className="font-medium">{inv.fleet_name}</span> as {inv.role}.</span>
          <Button size="sm" className="ml-auto" onClick={() => app.acceptInvite(inv.id, inv.fleet_id)}>Accept</Button>
          <Button size="sm" variant="ghost" onClick={() => app.declineInvite(inv.id)}>Decline</Button>
        </div>
      ))}
    </div>
  );
}
