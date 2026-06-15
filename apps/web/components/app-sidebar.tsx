"use client";

import { useEffect, useState } from "react";
import { useTheme } from "next-themes";
import { Bot, FolderKanban, LayoutGrid, type LucideIcon, Moon, PanelLeftClose, PanelLeftOpen, Server, Settings, Sun, Users } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { NewFleetDialog } from "@/components/new-fleet-dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { cn } from "@/lib/utils";
import { initials } from "@/lib/labels";
import type { Section } from "@/lib/routes";

type NavItem = { key: Section; label: string; icon: LucideIcon };
const NAV: NavItem[] = [
  { key: "operations", label: "Operations", icon: LayoutGrid },
  { key: "missions", label: "Missions", icon: FolderKanban },
  { key: "crews", label: "Crews", icon: Bot },
  { key: "rovers", label: "Rovers", icon: Server },
  { key: "members", label: "Members", icon: Users },
  { key: "settings", label: "Settings", icon: Settings },
];

export function AppSidebar({ section, setSection }: { section: Section; setSection: (s: Section) => void }) {
  const app = useApp();
  const { resolvedTheme, setTheme } = useTheme();
  const [collapsed, setCollapsed] = useState(false);

  useEffect(() => {
    if (localStorage.getItem("ufo.sidebar") === "collapsed") setCollapsed(true);
  }, []);
  const toggle = () => setCollapsed((c) => { const n = !c; localStorage.setItem("ufo.sidebar", n ? "collapsed" : "open"); return n; });

  // Navigating closes the operation detail (it overlays the content column).
  const go = (s: Section) => { app.openOp(null); setSection(s); };

  return (
    <aside className={cn("flex shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground transition-[width]", collapsed ? "w-14" : "w-56")}>
      <div className={cn("flex items-center p-3", collapsed ? "justify-center" : "gap-2")}>
        <div className="flex size-7 shrink-0 items-center justify-center rounded-md bg-brand text-sm font-bold text-brand-foreground">U</div>
        {!collapsed && <span className="flex-1 text-sm font-semibold">UFO</span>}
        {!collapsed && (
          <Button variant="ghost" size="icon-sm" onClick={toggle} title="Collapse sidebar"><PanelLeftClose /></Button>
        )}
      </div>

      {!collapsed && (
        <div className="flex items-center gap-1 px-3 pb-2">
          <Select value={app.fleet} onValueChange={(v) => app.switchFleet(v)}>
            <SelectTrigger className="h-8 flex-1 bg-sidebar text-xs"><SelectValue /></SelectTrigger>
            <SelectContent>
              {app.fleets.map((f) => <SelectItem key={f.id} value={f.id}>{f.name}</SelectItem>)}
            </SelectContent>
          </Select>
          <NewFleetDialog />
        </div>
      )}

      <nav className="flex-1 space-y-0.5 p-2">
        {collapsed && (
          <button onClick={toggle} title="Expand sidebar" className="flex w-full items-center justify-center rounded-md py-1.5 text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground">
            <PanelLeftOpen className="size-4" />
          </button>
        )}
        {NAV.map(({ key, label, icon: Icon }) => (
          <button
            key={key}
            onClick={() => go(key)}
            title={collapsed ? label : undefined}
            className={cn(
              "flex w-full items-center rounded-md py-1.5 text-sm transition-colors",
              collapsed ? "justify-center" : "gap-2.5 px-2.5",
              section === key ? "bg-sidebar-accent text-sidebar-accent-foreground font-medium" : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground",
            )}
          >
            <Icon className="size-4" />
            {!collapsed && <span className="flex-1 text-left">{label}</span>}
          </button>
        ))}
      </nav>

      <div className={cn("flex items-center border-t border-sidebar-border p-2", collapsed ? "flex-col gap-1" : "gap-2")}>
        <Avatar className="size-7 shrink-0"><AvatarFallback>{initials(app.user.name || app.user.email)}</AvatarFallback></Avatar>
        {!collapsed && <p className="min-w-0 flex-1 truncate text-xs font-medium">{app.user.name || app.user.email}</p>}
        <Button variant="ghost" size="icon-sm" onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")} title="Toggle theme">
          {resolvedTheme === "dark" ? <Sun /> : <Moon />}
        </Button>
      </div>
    </aside>
  );
}
