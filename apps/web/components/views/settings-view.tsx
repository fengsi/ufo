"use client";

import { useTheme } from "next-themes";
import { Monitor, Moon, Sun } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export function SettingsView() {
  const app = useApp();
  const { theme, setTheme } = useTheme();
  const fleet = app.fleets.find((f) => f.id === app.fleet);
  const opts = [
    { v: "light", icon: Sun, label: "Light" },
    { v: "dark", icon: Moon, label: "Dark" },
    { v: "system", icon: Monitor, label: "System" },
  ];

  return (
    <div className="mx-auto max-w-2xl space-y-4 p-4">
      <Card>
        <CardHeader><CardTitle className="text-base">Appearance</CardTitle></CardHeader>
        <CardContent>
          <div className="flex gap-2">
            {opts.map(({ v, icon: Icon, label }) => (
              <Button key={v} variant={theme === v ? "default" : "outline"} size="sm" onClick={() => setTheme(v)}>
                <Icon /> {label}
              </Button>
            ))}
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader><CardTitle className="text-base">Account</CardTitle></CardHeader>
        <CardContent className="space-y-2 text-sm">
          <div className="flex justify-between"><span className="text-muted-foreground">Name</span><span>{app.user.name || "—"}</span></div>
          <div className="flex justify-between"><span className="text-muted-foreground">Email</span><span>{app.user.email}</span></div>
          <div className="flex justify-between"><span className="text-muted-foreground">Fleet</span><span>{fleet?.name}</span></div>
          <div className="pt-2">
            <Button variant="outline" size="sm" onClick={() => app.signOut()}>Sign out</Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
