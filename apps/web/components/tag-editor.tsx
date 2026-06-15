"use client";

import { useState } from "react";
import { X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";

// Editable set of tags (lowercased, deduped). Add on Enter/comma; click × to remove.
export function TagEditor({ tags, onChange, placeholder = "add tag…" }: { tags: string[]; onChange: (t: string[]) => void; placeholder?: string }) {
  const [draft, setDraft] = useState("");

  function commit() {
    const t = draft.trim().toLowerCase();
    setDraft("");
    if (t && !tags.includes(t)) onChange([...tags, t]);
  }

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {tags.map((t) => (
        <Badge key={t} variant="secondary" className="gap-1 font-mono text-[11px]">
          {t}
          <button onClick={() => onChange(tags.filter((x) => x !== t))} className="text-muted-foreground hover:text-destructive"><X className="size-3" /></button>
        </Badge>
      ))}
      <Input
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" || e.key === ",") { e.preventDefault(); commit(); } }}
        onBlur={commit}
        placeholder={placeholder}
        className="h-6 w-28 border-dashed px-2 text-xs"
      />
    </div>
  );
}

// Read-only tag chips (e.g. a rover's auto-detected tags).
export function TagList({ tags, muted = true }: { tags: string[]; muted?: boolean }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {tags.map((t) => (
        <Badge key={t} variant="outline" className={muted ? "font-mono text-[11px] text-muted-foreground" : "font-mono text-[11px]"}>{t}</Badge>
      ))}
    </div>
  );
}
