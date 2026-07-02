"use client";

import React, { useEffect, useMemo, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Download, ExternalLink, X } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { AssetKindIcon, AssetSourceIcon, assetInlineContentURL, assetKindLabel, canPreviewAsset, formatBytes, isImageAsset } from "@/components/asset-display";
import { AssetPreview, AssetTextCopyButton } from "@/components/asset-preview";
import { cn, hideFlowControlFlags } from "@/lib/utils";
import { assetFilePath } from "@/lib/assets";
import { appPath } from "@/lib/routes";
import { linkUserMentions, userHrefID } from "@/lib/user-mentions";
import type { Asset } from "@/lib/types";

const OP_CODE_RE = /(^|[^\w`])#?([A-Za-z0-9]+-\d+)\b/g;
const EMOJI_RE = /(\p{Extended_Pictographic}(?:\uFE0F|\u200D\p{Extended_Pictographic}(?:\uFE0F)?)*)/gu;
const EMOJI_ONLY_RE = /^\p{Extended_Pictographic}(?:\uFE0F|\u200D\p{Extended_Pictographic}(?:\uFE0F)?)*$/u;
type OperationMention = { id: string; href: string };
const MENTION_CACHE_MAX = 500;
const mentionCache = new Map<string, Promise<OperationMention | null>>();

function operationCodes(text: string): string[] {
  return [...new Set([...text.matchAll(OP_CODE_RE)].map((m) => m[2].toUpperCase()))];
}

function linkOperationCodes(text: string, mentions: Record<string, OperationMention>): string {
  return text.replace(OP_CODE_RE, (full, prefix: string, code: string) => {
    const mention = mentions[code.toUpperCase()];
    if (!mention) return full;
    return `${prefix}[${full.slice(prefix.length)}](${mention.href})`;
  });
}

function renderEmojiText(text: string): React.ReactNode {
  return text.split(EMOJI_RE).map((part, i) => (
    EMOJI_ONLY_RE.test(part) ? <span key={i} className="align-[-0.1em] text-base leading-none">{part}</span> : part
  ));
}

function renderEmojiNode(node: React.ReactNode): React.ReactNode {
  if (typeof node === "string") return renderEmojiText(node);
  if (Array.isArray(node)) return node.map((child, i) => <React.Fragment key={i}>{renderEmojiNode(child)}</React.Fragment>);
  if (React.isValidElement<{ children?: React.ReactNode }>(node) && node.props.children) {
    return React.cloneElement(node, undefined, renderEmojiNode(node.props.children));
  }
  return node;
}

/** Allow only http(s) and same-origin relative paths — blocks javascript:/data: etc. */
function safeMarkdownHref(href?: string): string | null {
  if (!href) return null;
  const trimmed = href.trim();
  if (!trimmed || trimmed.startsWith("//")) return null;
  if (trimmed.startsWith("/") && !trimmed.startsWith("//")) return trimmed;
  try {
    const url = new URL(trimmed);
    if (url.protocol === "http:" || url.protocol === "https:") return trimmed;
  } catch {
    return null;
  }
  return null;
}

function operationHrefID(href?: string) {
  const parts = href?.split("?")[0].split("/").filter(Boolean) ?? [];
  return parts[0] === "fleets" && parts[2] === "operations" ? parts[3] : null;
}

function assetHrefID(href?: string) {
  const parts = href?.split("?")[0].split("/").filter(Boolean) ?? [];
  if (parts[0] === "v1" && parts[1] === "assets" && parts[3] === "file") return parts[2];
  if (parts[0] === "api" && parts[1] === "v1" && parts[2] === "assets" && parts[4] === "file") return parts[3];
  return null;
}

function AssetLinkPreview({ asset, onPreview }: { asset: Asset; onPreview: (asset: Asset) => void }) {
  const previewable = canPreviewAsset(asset);
  const fileURL = asset.url || assetFilePath(asset.id);
  const className = "not-prose my-1 inline-flex max-w-full items-center gap-2 rounded-md border border-border bg-muted/30 p-1.5 text-left hover:bg-accent hover:text-accent-foreground";
  const content = (
    <>
      <span className="flex size-12 shrink-0 items-center justify-center overflow-hidden rounded bg-background">
        {isImageAsset(asset) ? (
          <img src={assetInlineContentURL(asset)} alt="" className="h-full w-full object-cover" />
        ) : (
          <AssetKindIcon asset={asset} className="size-6 text-muted-foreground" />
        )}
      </span>
      <span className="min-w-0">
        <span className="block truncate text-xs font-medium text-foreground">{asset.filename}</span>
        <span className="block truncate text-[11px] text-muted-foreground">{assetKindLabel(asset)} · {formatBytes(asset.byte_size)}</span>
      </span>
    </>
  );
  if (!previewable) {
    return <a href={fileURL} target="_blank" rel="noopener noreferrer" className={className}>{content}</a>;
  }
  return (
    <button type="button" className={className} onClick={() => onPreview(asset)}>
      {content}
    </button>
  );
}

function AssetPreviewDialog({ asset, assets, onClose }: { asset: Asset | null; assets: Asset[]; onClose: () => void }) {
  useEffect(() => {
    if (!asset) return;
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [asset, onClose]);

  if (!asset) return null;
  const fileURL = asset.url || assetFilePath(asset.id);
  return (
    <div className="not-prose fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-4 backdrop-blur-[2px]" onPointerDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <div className="flex max-h-[90vh] w-full max-w-6xl flex-col overflow-hidden rounded-lg border border-border/80 bg-popover text-popover-foreground shadow-[0_24px_80px_rgba(0,0,0,0.45)] ring-1 ring-background/20" role="dialog" aria-modal="true" aria-label={asset.filename}>
        <div className="flex min-w-0 items-center gap-2 border-b border-border px-3 py-2 text-sm">
          <AssetKindIcon asset={asset} className="size-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1">
            <div className="truncate font-medium">{asset.filename}</div>
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <AssetSourceIcon asset={asset} />
              <span>{assetKindLabel(asset)} · {formatBytes(asset.byte_size)}</span>
            </div>
          </div>
          <a href={fileURL} className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground" title="Download file" aria-label={`Download ${asset.filename}`}>
            <Download className="size-4" />
          </a>
          <AssetTextCopyButton asset={asset} className="size-8" />
          <button type="button" className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground" title="Close preview" aria-label="Close preview" onClick={onClose}>
            <X className="size-4" />
          </button>
        </div>
        <AssetPreview asset={asset} size="dialog" renderMarkdown={(text) => <Markdown assets={assets}>{text}</Markdown>} />
      </div>
    </div>
  );
}

// GitHub-flavored Markdown for comments and pilot output. Raw HTML is rendered as text.
export function Markdown({ children, className, assets = [] }: { children: string; className?: string; assets?: Asset[] }) {
  const app = useApp();
  const text = hideFlowControlFlags(children);
  const codes = useMemo(() => operationCodes(text), [text]);
  const [previewAsset, setPreviewAsset] = useState<Asset | null>(null);
  const missionVersion = useMemo(() => app.missions.map((m) => `${m.id}:${m.key}`).join("|"), [app.missions]);
  const [mentions, setMentions] = useState<Record<string, OperationMention>>({});
  useEffect(() => {
    if (codes.length === 0 || app.missions.length === 0) return;
    let active = true;
    Promise.all(codes.map(async (code) => {
      const cacheKey = `${app.fleet}:${missionVersion}:${code}`;
      let mention = mentionCache.get(cacheKey);
      if (!mention) {
        mention = app.searchOperations(code).then((hits) => {
          const hit = hits.find((op) => {
            const mission = app.missions.find((m) => m.id === op.mission_id);
            return mission && `${mission.key}-${op.sequence}`.toUpperCase() === code;
          });
          return hit ? { id: hit.id, href: appPath(app.fleet, "operations", hit.id) } : null;
        });
        if (mentionCache.size >= MENTION_CACHE_MAX) {
          const oldest = mentionCache.keys().next().value;
          if (oldest !== undefined) mentionCache.delete(oldest);
        }
        mentionCache.set(cacheKey, mention);
      }
      const resolved = await mention;
      return resolved ? [code, resolved] as const : null;
    })).then((rows) => {
      if (!active) return;
      setMentions(Object.fromEntries(rows.filter((row): row is NonNullable<typeof row> => row != null)));
    });
    return () => { active = false; };
  }, [app.fleet, app.missions, app.searchOperations, codes, missionVersion]);
  useEffect(() => {
    if (previewAsset && !assets.some((asset) => asset.id === previewAsset.id)) setPreviewAsset(null);
  }, [assets, previewAsset]);
  const linkedText = useMemo(() => {
    const withOps = linkOperationCodes(text, mentions);
    return linkUserMentions(withOps, app.members, app.user, app.fleet);
  }, [text, mentions, app.members, app.user, app.fleet]);
  return (
    <div
      className={cn(
        "prose prose-sm max-w-none break-words",
        "[--tw-prose-body:var(--foreground)] [--tw-prose-headings:var(--foreground)] [--tw-prose-lead:var(--muted-foreground)] [--tw-prose-links:var(--brand)] [--tw-prose-bold:var(--foreground)] [--tw-prose-counters:var(--muted-foreground)] [--tw-prose-bullets:var(--muted-foreground)] [--tw-prose-hr:var(--border)] [--tw-prose-quotes:var(--foreground)] [--tw-prose-quote-borders:var(--border)] [--tw-prose-captions:var(--muted-foreground)] [--tw-prose-code:var(--foreground)] [--tw-prose-pre-code:var(--foreground)] [--tw-prose-pre-bg:color-mix(in_oklch,var(--muted)_65%,transparent)] [--tw-prose-th-borders:var(--border)] [--tw-prose-td-borders:var(--border)]",
        "prose-pre:bg-muted/60 prose-pre:text-foreground prose-code:text-foreground prose-code:before:content-none prose-code:after:content-none",
        "[&_:not(pre)>code]:rounded [&_:not(pre)>code]:bg-muted [&_:not(pre)>code]:px-1 [&_:not(pre)>code]:py-0.5 [&_:not(pre)>code]:font-mono [&_:not(pre)>code]:text-[0.875em] [&_:not(pre)>code]:font-medium",
        "prose-headings:font-semibold prose-a:text-brand prose-p:my-1.5 prose-pre:my-2",
        className,
      )}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => <p className="whitespace-pre-wrap">{renderEmojiNode(children)}</p>,
          li: ({ children }) => <li>{renderEmojiNode(children)}</li>,
          a: ({ children, href }) => {
            const assetID = assetHrefID(href);
            const asset = assetID ? assets.find((asset) => asset.id === assetID) : null;
            if (asset) return <AssetLinkPreview asset={asset} onPreview={setPreviewAsset} />;
            const safeHref = safeMarkdownHref(href);
            if (!safeHref) {
              return <span className="inline-flex items-center gap-0.5">{children}</span>;
            }
            const internal = safeHref.startsWith("/");
            return (
              <a
                href={safeHref}
                target={internal ? undefined : "_blank"}
                rel={internal ? undefined : "noopener noreferrer"}
                onClick={(e) => {
                  const userId = userHrefID(safeHref);
                  if (userId) {
                    e.preventDefault();
                    app.openUser(userId);
                    return;
                  }
                  const id = operationHrefID(safeHref);
                  if (!id) return;
                  e.preventDefault();
                  app.openOperation(id);
                }}
                className="inline-flex items-center gap-0.5"
              >
                {children}
                {!internal && <ExternalLink className="size-3 shrink-0 opacity-70" />}
              </a>
            );
          },
        }}
      >
        {linkedText}
      </ReactMarkdown>
      <AssetPreviewDialog asset={previewAsset} assets={assets} onClose={() => setPreviewAsset(null)} />
    </div>
  );
}
