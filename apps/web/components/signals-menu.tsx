"use client";

import { useState } from "react";
import { Archive, Eye, type LucideIcon, MessageCircleQuestion, Radio, TriangleAlert } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

const SEVERITY_COLOR: Record<string, string> = {
  action_required: "text-destructive",
  attention: "text-warning",
  info: "text-muted-foreground",
};

const TYPE_ICON: Record<string, LucideIcon> = {
  input_requested: MessageCircleQuestion,
  review_requested: Eye,
  task_failed: TriangleAlert,
};

export function SignalsMenu() {
  const app = useApp();
  const [open, setOpen] = useState(false);
  const unread = app.signals.filter((s) => !s.read).length;

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="icon-sm" className="relative" title="Signals">
          <Radio />
          {unread > 0 && (
            <span className="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-brand px-1 text-[10px] font-medium text-brand-foreground">
              {unread}
            </span>
          )}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-96 p-0">
        <div className="flex items-center justify-between border-b border-border px-3 py-2">
          <p className="text-xs font-semibold">Signals</p>
          {unread > 0 && <span className="text-xs text-muted-foreground">{unread} unread</span>}
        </div>
        {app.signals.length === 0 ? (
          <div className="flex flex-col items-center justify-center gap-2 px-4 py-10 text-muted-foreground">
            <Radio className="size-6" />
            <p className="text-xs">All clear — nothing needs you.</p>
          </div>
        ) : (
          <div className="max-h-[70vh] divide-y divide-border overflow-y-auto">
            {app.signals.map((it) => {
              const Icon = TYPE_ICON[it.type] ?? Radio;
              return (
                <div
                  key={it.id}
                  onClick={() => { app.openSignal(it); setOpen(false); }}
                  className={cn(
                    "relative flex cursor-pointer items-start gap-2.5 py-2.5 pr-3 pl-4 transition-colors hover:bg-accent/50",
                    it.read && "opacity-65",
                  )}
                >
                  {!it.read && <span className="absolute inset-y-0 left-0 w-0.5 bg-brand" />}
                  <Icon className={cn("mt-0.5 size-4 shrink-0", SEVERITY_COLOR[it.severity] ?? "text-muted-foreground")} />
                  <div className="min-w-0 flex-1">
                    <p className={cn("text-xs leading-snug", !it.read ? "font-semibold text-foreground" : "text-muted-foreground")}>{it.title}</p>
                    {it.body && <p className="line-clamp-2 text-xs text-muted-foreground">{it.body}</p>}
                    <p className="mt-0.5 text-[11px] text-muted-foreground">{new Date(it.created_at).toLocaleString([], { hour12: false })}</p>
                  </div>
                  {!it.read && <span className="mt-1.5 size-2 shrink-0 rounded-full bg-brand" title="Unread" />}
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    onClick={(e) => { e.stopPropagation(); app.archiveSignal(it.id); }}
                    title="Archive"
                  >
                    <Archive />
                  </Button>
                </div>
              );
            })}
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}
