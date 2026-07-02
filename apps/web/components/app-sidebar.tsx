"use client";

import { useEffect, useState } from "react";
import { type LucideIcon, PanelLeftClose, PanelLeftOpen, Plus } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { NewFleetDialog } from "@/components/new-fleet-dialog";
import { RenameFleetDialog } from "@/components/rename-fleet-dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { cn } from "@/lib/utils";
import { initials } from "@/lib/labels";
import type { Section } from "@/lib/routes";
import { SECTION_ICONS } from "@/lib/section-icons";

type NavItem = { key: Section; label: string; icon: LucideIcon };
const NAV: NavItem[] = [
  { key: "operations", label: "Operations", icon: SECTION_ICONS.operations },
  { key: "missions", label: "Missions", icon: SECTION_ICONS.missions },
  { key: "routines", label: "Routines", icon: SECTION_ICONS.routines },
  { key: "crews", label: "Crews", icon: SECTION_ICONS.crews },
  { key: "rovers", label: "Rovers", icon: SECTION_ICONS.rovers },
  { key: "members", label: "Members", icon: SECTION_ICONS.members },
  { key: "settings", label: "Settings", icon: SECTION_ICONS.settings },
];

export function AppSidebar({ section, setSection }: { section: Section; setSection: (s: Section) => void }) {
  const app = useApp();
  const [collapsed, setCollapsed] = useState(false);
  const currentFleet = app.fleets.find((f) => f.id === app.fleet);

  useEffect(() => {
    if (localStorage.getItem("ufo.sidebar") === "collapsed") setCollapsed(true);
  }, []);
  const toggle = () => setCollapsed((c) => { const n = !c; localStorage.setItem("ufo.sidebar", n ? "collapsed" : "open"); return n; });

  // Navigating closes the operation detail (it overlays the content column).
  const go = (s: Section) => { app.openUser(null); app.openOperation(null); setSection(s); };
  return (
    <aside className={cn("ufo-sidebar shrink-0 overflow-hidden border-r border-sidebar-border bg-sidebar text-sidebar-foreground transition-[width] duration-200 ease-out", collapsed ? "ufo-sidebar-collapsed w-14" : "w-56")}>
      <div className="flex h-full w-56 flex-col">
        <div className="grid h-12 grid-cols-[2rem_minmax(0,1fr)_1.75rem] items-center gap-2 px-3">
          <div className="flex size-8 items-center justify-center">
            <button onClick={toggle} title={collapsed ? "Expand sidebar" : "Collapse sidebar"} className="ufo-mark group relative flex size-7 items-center justify-center rounded-md bg-brand text-sm font-bold leading-none text-brand-foreground hover:opacity-90">
              U
              {collapsed && <PanelLeftOpen className="absolute -bottom-1 -right-1 size-3 rounded-sm bg-sidebar text-sidebar-foreground opacity-80 ring-1 ring-sidebar-border group-hover:opacity-100" />}
            </button>
          </div>
          <span className={cn("flex-1 text-sm font-semibold leading-none transition-opacity", collapsed && "pointer-events-none opacity-0")}>UFO</span>
          <Button variant="ghost" size="icon-sm" onClick={toggle} title="Collapse sidebar"><PanelLeftClose /></Button>
        </div>

        <div className="h-10 px-3 pb-2">
          <div className="grid grid-cols-[2rem_minmax(0,1fr)_1.75rem] items-center gap-1">
            <Select value={app.fleet} onValueChange={(v) => app.switchFleet(v)}>
              <SelectTrigger className={cn("h-8 bg-sidebar text-xs", collapsed ? "col-start-1 w-8 justify-center p-0 [&>svg]:hidden" : "col-span-3")}>
                {collapsed ? initials(currentFleet?.name ?? "UFO") : <SelectValue />}
              </SelectTrigger>
              <SelectContent>
                {app.fleets.map((f) => <SelectItem key={f.id} value={f.id}>{f.name}</SelectItem>)}
                <div className="my-1 border-t border-border" />
                {currentFleet && (
                  <RenameFleetDialog
                    fleetId={currentFleet.id}
                    name={currentFleet.name}
                    trigger={<button type="button" className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm outline-none hover:bg-accent hover:text-accent-foreground">Rename fleet</button>}
                  />
                )}
                <NewFleetDialog
                  trigger={
                    <button type="button" className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm outline-none hover:bg-accent hover:text-accent-foreground">
                      <Plus className="size-4" />
                      New fleet
                    </button>
                  }
                />
              </SelectContent>
            </Select>
          </div>
        </div>

        <nav className="flex-1 space-y-0.5 p-2">
          {NAV.map(({ key, label, icon: Icon }) => (
            <button
              key={key}
              onClick={() => go(key)}
              title={collapsed ? label : undefined}
              className={cn(
                "ufo-nav flex h-8 w-full items-center gap-2.5 rounded-md px-3 text-sm transition-colors",
                section === key ? "bg-sidebar-accent text-sidebar-accent-foreground font-medium" : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground",
              )}
            >
              <Icon className="size-4" />
              <span className={cn("flex-1 text-left transition-opacity", collapsed && "pointer-events-none opacity-0")}>{label}</span>
            </button>
          ))}
        </nav>

        <div className="flex items-center gap-2 border-t border-sidebar-border px-3 py-2">
          <div className="flex size-8 shrink-0 items-center justify-center">
            <Avatar className="size-7"><AvatarFallback>{initials(app.user.name || app.user.email)}</AvatarFallback></Avatar>
          </div>
          <p className={cn("min-w-0 flex-1 truncate text-xs font-medium transition-opacity", collapsed && "pointer-events-none opacity-0")}>{app.user.name || app.user.email}</p>
        </div>
      </div>
    </aside>
  );
}
