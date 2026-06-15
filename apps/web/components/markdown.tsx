"use client";

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ExternalLink } from "lucide-react";
import { cn } from "@/lib/utils";

// GitHub-flavored Markdown for comments and pilot output. Raw HTML is rendered as text.
export function Markdown({ children, className }: { children: string; className?: string }) {
  return (
    <div
      className={cn(
        "prose prose-sm dark:prose-invert max-w-none break-words",
        "prose-pre:bg-muted/60 prose-pre:text-foreground prose-code:text-foreground",
        "prose-headings:font-semibold prose-a:text-brand prose-p:my-1.5 prose-pre:my-2",
        className,
      )}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ children, href }) => (
            <a href={href} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-0.5">
              {children}
              <ExternalLink className="size-3 shrink-0 opacity-70" />
            </a>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
