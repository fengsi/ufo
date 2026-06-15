"use client";

import type { ReactNode } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export { Input as AuthInput } from "@/components/ui/input";
export { Button as AuthButton } from "@/components/ui/button";

export function AuthCard({
  title,
  error,
  footer,
  children,
}: {
  title: string;
  error: string | null;
  footer: { text: string; href: string; label: string };
  children: ReactNode;
}) {
  return (
    <main className="flex min-h-svh items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <div className="mb-1 flex items-center gap-2">
            <div className="flex size-7 items-center justify-center rounded-md bg-brand text-sm font-bold text-brand-foreground">U</div>
            <span className="text-xs font-medium text-muted-foreground">UFO — Unified Fleet Orchestrator</span>
          </div>
          <CardTitle className="text-xl">{title}</CardTitle>
        </CardHeader>
        <CardContent>
          {error && (
            <div className="mb-3 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
              {error}
            </div>
          )}
          {children}
          <p className="mt-4 text-sm text-muted-foreground">
            {footer.text}{" "}
            <a href={footer.href} className="font-medium text-brand hover:underline">{footer.label}</a>
          </p>
        </CardContent>
      </Card>
    </main>
  );
}
