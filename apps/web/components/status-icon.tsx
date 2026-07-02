import { cn } from "@/lib/utils";
import { STATUS_TEXT } from "@/lib/types";

// Status glyphs share a compact 14x14 viewBox.
export function StatusIcon({ status, className, subOperations = false }: { status: string; className?: string; subOperations?: boolean }) {
  const color = STATUS_TEXT[status] ?? "text-muted-foreground";
  return (
    <svg viewBox="0 0 14 14" className={cn("size-3.5 shrink-0", color, className)} aria-hidden>
      {status === "backlog" && (
        <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="1.6 1.8" />
      )}
      {status === "todo" && <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />}
      {status === "in_progress" && (
        <>
          <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M7 7 L7 2.5 A4.5 4.5 0 0 1 7 11.5 Z" fill="currentColor" />
        </>
      )}
      {status === "in_progress" && subOperations && (
        <>
          <circle cx="3.1" cy="3.1" r="1.2" fill="var(--background)" stroke="currentColor" strokeWidth="1" />
          <circle cx="10.9" cy="10.9" r="1.2" fill="var(--background)" stroke="currentColor" strokeWidth="1" />
        </>
      )}
      {status === "in_review" && (
        <>
          <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M7 7 L7 2.5 A4.5 4.5 0 1 1 2.5 7 Z" fill="currentColor" />
        </>
      )}
      {status === "done" && (
        <>
          <circle cx="7" cy="7" r="6" fill="currentColor" />
          <path d="M4.3 7.1 L6.2 9 L9.7 5" fill="none" stroke="var(--background)" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
        </>
      )}
      {status === "blocked" && (
        <>
          <circle cx="7" cy="7" r="6" fill="currentColor" />
          <path d="M4.8 4.8 L9.2 9.2 M9.2 4.8 L4.8 9.2" stroke="var(--background)" strokeWidth="1.4" strokeLinecap="round" />
        </>
      )}
      {status === "canceled" && (
        <>
          <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M4.8 7 H9.2" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" />
        </>
      )}
    </svg>
  );
}
