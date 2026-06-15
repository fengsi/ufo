import { cn } from "@/lib/utils";
import { PRIORITY } from "@/lib/labels";

// Priority glyphs use shape first, color second.
const BARS = [
  { x: 1.5, y: 9, h: 5 },
  { x: 6.5, y: 5.5, h: 8.5 },
  { x: 11.5, y: 2, h: 12 },
];
const COLOR = ["text-muted-foreground", "text-info", "text-warning", "text-destructive", "text-destructive"];

export function PriorityIcon({ level, className }: { level: number; className?: string }) {
  const label = PRIORITY[level]?.label ?? "Priority";
  if (level >= 4) {
    return (
      <svg viewBox="0 0 16 16" fill="currentColor" role="img" aria-label={label} className={cn("text-destructive", className)}>
        <rect x="1" y="1" width="14" height="14" rx="3.5" />
        <rect className="fill-background" x="7" y="3.5" width="2" height="6" rx="1" />
        <rect className="fill-background" x="7" y="11" width="2" height="2" rx="1" />
      </svg>
    );
  }
  if (level <= 0) {
    return (
      <svg viewBox="0 0 16 16" fill="currentColor" role="img" aria-label={label} className={cn("text-muted-foreground", className)}>
        <rect x="1" y="1" width="14" height="14" rx="3.5" />
        <text x="8" y="12" textAnchor="middle" fontSize="11" fontWeight="700" fontFamily="system-ui, sans-serif" className="fill-background">?</text>
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 16 16" fill="currentColor" role="img" aria-label={label} className={cn(COLOR[level] ?? COLOR[0], className)}>
      {BARS.map((b, i) => (
        <rect key={i} x={b.x} y={b.y} width="3" height={b.h} rx="1" opacity={i < level ? 1 : 0.28} />
      ))}
    </svg>
  );
}
