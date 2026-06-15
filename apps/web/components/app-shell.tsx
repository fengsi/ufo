"use client";

import { useEffect, useRef, useState } from "react";
import { useApp } from "@/components/app-provider";
import { AppSidebar } from "@/components/app-sidebar";
import { Board } from "@/components/board";
import { OperationDetail } from "@/components/operation-detail";
import { NewOperationDialog } from "@/components/new-operation-dialog";
import { SignalsMenu } from "@/components/signals-menu";
import { MissionsView } from "@/components/views/missions-view";
import { CrewsView } from "@/components/views/crews-view";
import { RoversView } from "@/components/views/rovers-view";
import { MembersView } from "@/components/views/members-view";
import { SettingsView } from "@/components/views/settings-view";
import { InviteBanner } from "@/components/invite-banner";
import { appPath, parseAppPath, type Section } from "@/lib/routes";

const TITLE: Record<Section, string> = {
  operations: "Operations",
  missions: "Missions",
  crews: "Crews",
  rovers: "Rovers",
  members: "Members",
  settings: "Settings",
};

function FireDefs() {
  return (
    <svg width="0" height="0" aria-hidden className="absolute">
      <defs>
        <filter id="ufo-fire" x="-20%" y="-60%" width="140%" height="200%" colorInterpolationFilters="sRGB">
          <feTurbulence type="fractalNoise" baseFrequency="0.03 0.04" numOctaves={2} seed={5} result="n">
            <animate attributeName="baseFrequency" dur="2.4s" values="0.03 0.04;0.05 0.07;0.03 0.04" repeatCount="indefinite" />
          </feTurbulence>
          <feDisplacementMap in="SourceGraphic" in2="n" scale={6} xChannelSelector="R" yChannelSelector="G" />
        </filter>
      </defs>
    </svg>
  );
}

export function AppShell() {
  const app = useApp();
  const [section, setSection] = useState<Section>(() =>
    typeof window === "undefined" ? "operations" : parseAppPath(window.location.pathname).section,
  );
  const fleetsRef = useRef(app.fleets);
  const fleetRef = useRef(app.fleet);
  fleetsRef.current = app.fleets;
  fleetRef.current = app.fleet;

  useEffect(() => {
    const initialRoute = parseAppPath(window.location.pathname);
    if (initialRoute.operationId) app.openOp(initialRoute.operationId);

    const onPop = () => {
      const route = parseAppPath(window.location.pathname);
      if (route.fleetId && fleetsRef.current.some((f) => f.id === route.fleetId) && route.fleetId !== fleetRef.current) {
        app.switchFleet(route.fleetId);
      }
      setSection(route.section);
      app.openOp(route.operationId);
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const path = appPath(app.fleet, section, app.selectedOp);
    if (window.location.pathname !== path) window.history.replaceState(null, "", path);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [section, app.selectedOp, app.fleet]);

  return (
    <div className="flex h-svh overflow-hidden">
      <FireDefs />
      <AppSidebar section={section} setSection={setSection} />
      <div className="relative flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center justify-between border-b border-border px-4">
          <h1 className="text-sm font-semibold">{TITLE[section]}</h1>
          <div className="flex items-center gap-2">
            {section === "operations" && <NewOperationDialog />}
            <SignalsMenu />
          </div>
        </header>
        <InviteBanner />
        <main className="min-h-0 flex-1 overflow-hidden">
          {section === "operations" && <Board />}
          {section === "missions" && <MissionsView />}
          {section === "crews" && <CrewsView />}
          {section === "rovers" && <RoversView />}
          {section === "members" && <MembersView />}
          {section === "settings" && <SettingsView />}
        </main>
        {/* Operation detail is a centered full-page view over this column (sidebar stays). */}
        <OperationDetail />
      </div>
    </div>
  );
}
