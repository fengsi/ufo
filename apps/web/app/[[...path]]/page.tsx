"use client";

import { useEffect, useState } from "react";
import { AppProvider } from "@/components/app-provider";
import { AppShell } from "@/components/app-shell";
import { apiFetch, getJSON } from "@/lib/api";
import { parseAppPath } from "@/lib/routes";
import type { Fleet, User } from "@/lib/types";
import { storeAuthNextPath } from "../auth-ui";

export default function Page() {
  const [boot, setBoot] = useState<{ user: User; fleets: Fleet[]; fleet: string } | null>(null);

  useEffect(() => {
    (async () => {
      const me = await apiFetch("/api/v1/users/me");
      if (me.status === 401) {
        const next = `${window.location.pathname}${window.location.search}${window.location.hash}`;
        storeAuthNextPath(next);
        window.location.href = "/login";
        return;
      }
      const user = (await me.json()) as User;
      const fleets = (await getJSON<Fleet[]>("/api/v1/fleets")) ?? [];
      const route = parseAppPath(window.location.pathname);
      const fromUrl = fleets.find((f) => f.id === route.fleetId)?.id;
      const saved = localStorage.getItem("ufo.fleet") ?? "";
      const fleet = fromUrl ?? fleets.find((f) => f.id === saved)?.id ?? fleets[0]?.id ?? "";
      if (fleet) localStorage.setItem("ufo.fleet", fleet);
      setBoot({ user, fleets, fleet });
    })();
  }, []);

  if (!boot || !boot.fleet) {
    return <div className="flex h-svh items-center justify-center text-sm text-muted-foreground">Loading…</div>;
  }

  return (
    <AppProvider user={boot.user} fleets={boot.fleets} initialFleet={boot.fleet}>
      <AppShell />
    </AppProvider>
  );
}
