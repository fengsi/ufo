"use client";

import type { ReactNode, MouseEvent } from "react";
import { useApp } from "@/components/app-provider";
import { cn } from "@/lib/utils";

export function UserLink({
  userId,
  className,
  children,
  stopPropagation = false,
}: {
  userId: string | null | undefined;
  className?: string;
  children: ReactNode;
  stopPropagation?: boolean;
}) {
  const app = useApp();
  if (!userId) return <span className={className}>{children}</span>;
  return (
    <button
      type="button"
      className={cn("text-left hover:underline", className)}
      onClick={(e: MouseEvent) => {
        if (stopPropagation) e.stopPropagation();
        app.openUser(userId);
      }}
    >
      {children}
    </button>
  );
}
