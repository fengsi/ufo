"use client";

import { useEffect, useRef, useState } from "react";
import { useTheme } from "next-themes";
import { Antenna, Archive, ArchiveRestore, ArrowLeft, ArrowUp, ArrowUpDown, Check, ChevronDown, ChevronRight, Clock, Download, GitBranch, GitPullRequest, Grid2x2, Grid3x3, Layers, Link2, List, Loader2, MessageCircleQuestion, Moon, Paperclip, Pencil, Plus, RefreshCw, Reply, RotateCcw, ScrollText, SmilePlus, Sun, Tags, Trash2, Users, X } from "lucide-react";
import { useApp } from "@/components/app-provider";
import { del, getJSON } from "@/lib/api";
import { AssetDeleteDialog } from "@/components/asset-delete-dialog";
import { AssetKindIcon, AssetSourceIcon, assetExtension, assetInlineContentURL, assetKindLabel, assetSource, canPreviewAsset, formatAssetDate, formatBytes, isImageAsset, type AssetSource } from "@/components/asset-display";
import { AssetPreview, AssetTextCopyButton } from "@/components/asset-preview";
import { StatusIcon } from "@/components/status-icon";
import { PriorityIcon } from "@/components/priority-icon";
import { PilotIcon } from "@/components/pilot-icon";
import { onFire, DetailFire } from "@/components/fire";
import { Markdown } from "@/components/markdown";
import { CrewOption, PilotOption } from "@/components/assignee-select";
import { TelemetryDialog } from "@/components/telemetry-dialog";
import { SignalsMenu } from "@/components/signals-menu";
import { SelectionActionsMenu, copyText, selectedTextWithin } from "@/components/selection-actions-menu";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { TagEditor } from "@/components/tag-editor";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn, hideFlowControlFlags } from "@/lib/utils";
import { appendAssetLink, assetFilePath } from "@/lib/assets";
import { assigneeHasPilot, commentAuthor, memberLabel, pilotLabel, operationAssigneeValue, operationCode, operationWaitingOnSubOperations, PRIORITY, LABEL_COLOR, userLabel } from "@/lib/labels";
import { elapsed } from "@/lib/timeline";
import { DRAFT_SAVE_DELAY_SECONDS, formatTimestamp, useAssetPanelOpen, useAssetViewMode, useCommsOrder, useTimeFormat, type AssetViewMode, type TimeFormat } from "@/lib/view";
import type { Asset, Comment, OperationReference, Operation, Reaction, Relation, Run, SourceAction } from "@/lib/types";

const ACTIVE = new Set(["queued", "accepted", "starting", "running"]);
const isActive = (r: Run) => ACTIVE.has(r.status);
const STATUSES = ["backlog", "todo", "in_progress", "in_review", "done", "blocked", "canceled"];
const STATUS_LABEL: Record<string, string> = {
  backlog: "Backlog", todo: "Todo", in_progress: "In Progress", in_review: "In Review",
  done: "Done", blocked: "Blocked", canceled: "Canceled",
};
const EMOJI = ["👍", "👎", "👀", "✅", "🙏", "🙌", "🎉", "💯", "❤️", "🔥", "🚀", "🙂", "☹️", "🙃", "😂", "🤣", "😅", "🤔", "🫠", "😢"];
const COMMENT_PREVIEW_LIMIT = 30;
const PILOT_STATUS_PREFIX = "Pilot set status: ";
const CAPTAIN_SPLIT_RE = /^Captain split into (\d+) sub-operations$/;
const RAIL_CONTROL_CLASS = "h-7 rounded-md border-0 bg-transparent px-1.5 text-xs hover:bg-accent hover:text-accent-foreground focus:ring-0 focus-visible:ring-0";
const OPERATION_EDIT_DRAFT_PREFIX = "ufo.operationEditDraft.";
const SUB_OPERATION_CREATE_DRAFT_PREFIX = "ufo.subOperationCreateDraft.";
const COMMENT_CREATE_DRAFT_PREFIX = "ufo.commentCreateDraft.";
const COMMENT_EDIT_DRAFT_PREFIX = "ufo.commentEditDraft.";
const DATE_MONTH_LABELS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const REPLY_TEXTAREA_MAX_HEIGHT = 160;
type AssetSourceFilter = "all" | AssetSource;
type ActivityCommentRow = { comment: Comment; pilotStatus?: string; captainSplit?: string };
type ActivityDisplayRow = ActivityCommentRow | { commentGroup: ActivityCommentRow[]; status: "queued" | "working" };

function worktreeValue(metadata: Record<string, unknown> | undefined): boolean | undefined {
  return typeof metadata?.worktree_enabled === "boolean" ? metadata.worktree_enabled : undefined;
}

type MissionMoveNotice = {
  from_key: string;
  from_name?: string;
  to_key?: string;
  to_name?: string;
  at?: string;
};

function missionMoveFromMetadata(metadata: Record<string, unknown> | undefined): MissionMoveNotice | null {
  const raw = metadata?.mission_move;
  if (!raw || typeof raw !== "object") return null;
  const move = raw as Record<string, unknown>;
  const fromKey = typeof move.from_key === "string" ? move.from_key : "";
  if (!fromKey) return null;
  return {
    from_key: fromKey,
    from_name: typeof move.from_name === "string" ? move.from_name : undefined,
    to_key: typeof move.to_key === "string" ? move.to_key : undefined,
    to_name: typeof move.to_name === "string" ? move.to_name : undefined,
    at: typeof move.at === "string" ? move.at : undefined,
  };
}

function operationEditDraftKey(operationId: string) {
  return `${OPERATION_EDIT_DRAFT_PREFIX}${operationId}`;
}

function subOperationCreateDraftKey(mainId: string) {
  return `${SUB_OPERATION_CREATE_DRAFT_PREFIX}${mainId}`;
}

function commentCreateDraftKey(operationId: string) {
  return `${COMMENT_CREATE_DRAFT_PREFIX}${operationId}`;
}

function commentEditDraftKey(commentId: string) {
  return `${COMMENT_EDIT_DRAFT_PREFIX}${commentId}`;
}

function readDraft(key: string) {
  return localStorage.getItem(key);
}

function writeChangedLocalDraft(key: string, value: string, base: string) {
  const saved = localStorage.getItem(key);
  if (value === base) {
    if (saved == null) return false;
    localStorage.removeItem(key);
    return true;
  }
  if (saved === value) return false;
  localStorage.setItem(key, value);
  return true;
}

function writeChangedSessionDraft(key: string, value: string, base: string) {
  const saved = sessionStorage.getItem(key);
  if (value === base) {
    if (saved == null) return false;
    sessionStorage.removeItem(key);
    return true;
  }
  if (saved === value) return false;
  sessionStorage.setItem(key, value);
  return true;
}

export function OperationDetail() {
  const app = useApp();
  const open = app.selectedOperation != null;
  const d = app.operationDetail;
  const [comment, setComment] = useState(() => {
    if (typeof window === "undefined" || !app.selectedOperation) return "";
    return readDraft(commentCreateDraftKey(app.selectedOperation)) ?? "";
  });
  const skipDraftSaveRef = useRef(true);
  const draftSaveTimerRef = useRef<number | null>(null);
  const pendingDraftSaveRef = useRef<{ key: string; value: string } | null>(null);
  const selectedOperationRef = useRef(app.selectedOperation);
  const commentRef = useRef(comment);
  const [editingBody, setEditingBody] = useState(false);
  const [titleEditDraft, setTitleEditDraft] = useState("");
  const [bodyEditDraft, setBodyEditDraft] = useState("");
  const editingBodyRef = useRef(editingBody);
  const bodyEditDraftRef = useRef(bodyEditDraft);
  const operationBodyRef = useRef<{ id: string; body: string } | null>(null);
  const [assetUploading, setAssetUploading] = useState(false);
  const assetUploadRef = useRef<HTMLInputElement>(null);
  const replyTextareaRef = useRef<HTMLTextAreaElement>(null);
  const titleInputRef = useRef<HTMLInputElement>(null);
  const bodyTextareaRef = useRef<HTMLTextAreaElement>(null);
  const autoInsertUploadTargetRef = useRef<"comment" | "body" | null>(null);
  const restoreReplyFocusRef = useRef(false);
  const [insertTarget, setInsertTarget] = useState<"comment" | "body">("comment");
  const [assetsOpen, setAssetsOpenState] = useState(false);
  const { assetView, setAssetView } = useAssetViewMode();
  const { assetPanelOpen, setAssetPanelOpen } = useAssetPanelOpen();
  const assetPanelOpenRef = useRef<boolean | null>(null);
  const [assetSource, setAssetSource] = useState<AssetSourceFilter>("all");
  const [operationAssets, setOperationAssets] = useState<Asset[]>([]);
  const [assetsLoading, setAssetsLoading] = useState(false);
  const [assetDeleteTarget, setAssetDeleteTarget] = useState<Asset | null>(null);
  const [assetDeletingId, setAssetDeletingId] = useState<string | null>(null);
  const [assetDeleteError, setAssetDeleteError] = useState<string | null>(null);
  const [previewAsset, setPreviewAsset] = useState<Asset | null>(null);
  const [telemetryRun, setTelemetryRun] = useState<Run | null>(null);
  const [olderComments, setOlderComments] = useState<Comment[]>([]);
  const [commentsMore, setCommentsMore] = useState(false);
  const [loadingOlderComments, setLoadingOlderComments] = useState(false);
  const [addingSubOperation, setAddingSubOperation] = useState(false);
  const [pendingMissionId, setPendingMissionId] = useState<string | null>(null);
  const [missionMoveConfirm, setMissionMoveConfirm] = useState("");
  const [missionMoving, setMissionMoving] = useState(false);
  const [missionMoveError, setMissionMoveError] = useState<string | null>(null);
  const { theme, resolvedTheme, setTheme } = useTheme();
  const { timeFormat } = useTimeFormat();
  const { commsOrder, setCommsOrder } = useCommsOrder();

  const runs = d?.runs ?? [];
  const activeRun = runs.find(isActive);
  const sortedCrews = [...app.crews].sort((a, b) => a.name.localeCompare(b.name));
  const sortedPilots = [...app.pilots].sort((a, b) => pilotLabel(a.kind).localeCompare(pilotLabel(b.kind)));
  const sortedMembers = app.members.filter((m) => m.id !== app.user.id).sort((a, b) => (a.name || a.email).localeCompare(b.name || b.email));
  selectedOperationRef.current = app.selectedOperation;
  commentRef.current = comment;
  editingBodyRef.current = editingBody;
  bodyEditDraftRef.current = bodyEditDraft;
  operationBodyRef.current = d?.operation ? { id: d.operation.id, body: d.operation.body ?? "" } : null;
  function writeCommentCreateDraft(key: string, value: string) {
    const saved = localStorage.getItem(key);
    if (!value.trim()) {
      if (saved == null) return false;
      localStorage.removeItem(key);
      return true;
    }
    if (saved === value) return false;
    localStorage.setItem(key, value);
    return true;
  }
  function clearCommentCreateDraftSaveTimer() {
    if (draftSaveTimerRef.current == null) return;
    window.clearTimeout(draftSaveTimerRef.current);
    draftSaveTimerRef.current = null;
  }
  function flushCommentCreateDraftSave() {
    const pending = pendingDraftSaveRef.current;
    clearCommentCreateDraftSaveTimer();
    if (!pending) return;
    pendingDraftSaveRef.current = null;
    writeCommentCreateDraft(pending.key, pending.value);
  }
  function saveCurrentCommentCreateDraft() {
    clearCommentCreateDraftSaveTimer();
    pendingDraftSaveRef.current = null;
    const operationId = selectedOperationRef.current;
    if (!operationId) return;
    writeCommentCreateDraft(commentCreateDraftKey(operationId), commentRef.current);
  }
  function saveCurrentOperationBodyEditDraft() {
    const operation = operationBodyRef.current;
    if (!editingBodyRef.current || !operation) return;
    writeChangedLocalDraft(operationEditDraftKey(operation.id), bodyEditDraftRef.current, operation.body);
  }
  function saveCurrentDrafts() {
    saveCurrentCommentCreateDraft();
    saveCurrentOperationBodyEditDraft();
  }
  function resizeReplyTextarea() {
    const el = replyTextareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, REPLY_TEXTAREA_MAX_HEIGHT)}px`;
    el.style.overflowY = el.scrollHeight > REPLY_TEXTAREA_MAX_HEIGHT ? "auto" : "hidden";
  }
  useEffect(resizeReplyTextarea, [comment]);
  useEffect(() => {
    setOlderComments([]);
    setCommentsMore(Boolean(d?.comments_more));
  }, [d?.operation.id, d?.comments_more]);
  useEffect(() => {
    const operation = d?.operation;
    if (!operation) {
      setTitleEditDraft("");
      setBodyEditDraft("");
      setEditingBody(false);
      return;
    }
    const key = operationEditDraftKey(operation.id);
    const body = operation.body ?? "";
    setTitleEditDraft(operation.title);
    const saved = readDraft(key);
    if (saved != null && saved !== body) {
      setBodyEditDraft(saved);
      setEditingBody(true);
      return;
    }
    if (saved === body) localStorage.removeItem(key);
    setBodyEditDraft(body);
    setEditingBody(false);
  }, [d?.operation.id, d?.operation.title, d?.operation.body]);
  useEffect(() => {
    const operation = d?.operation;
    if (!editingBody || !operation) return;
    const id = window.setTimeout(() => {
      writeChangedLocalDraft(operationEditDraftKey(operation.id), bodyEditDraft, operation.body ?? "");
    }, DRAFT_SAVE_DELAY_SECONDS * 1000);
    return () => window.clearTimeout(id);
  }, [editingBody, bodyEditDraft, d?.operation.id, d?.operation.body]);
  useEffect(() => {
    setAddingSubOperation(false);
  }, [d?.operation.id]);
  useEffect(() => {
    flushCommentCreateDraftSave();
    skipDraftSaveRef.current = true;
    if (!app.selectedOperation) {
      setComment("");
      return;
    }
    setComment(readDraft(commentCreateDraftKey(app.selectedOperation)) ?? "");
  }, [app.selectedOperation]);
  useEffect(() => {
    if (!app.selectedOperation) return;
    if (skipDraftSaveRef.current) {
      skipDraftSaveRef.current = false;
      return;
    }
    const key = commentCreateDraftKey(app.selectedOperation);
    pendingDraftSaveRef.current = { key, value: comment };
    if (!comment.trim()) {
      flushCommentCreateDraftSave();
      return;
    }
    clearCommentCreateDraftSaveTimer();
    draftSaveTimerRef.current = window.setTimeout(() => flushCommentCreateDraftSave(), DRAFT_SAVE_DELAY_SECONDS * 1000);
    return clearCommentCreateDraftSaveTimer;
  }, [app.selectedOperation, comment]);
  useEffect(() => {
    window.addEventListener("pagehide", saveCurrentDrafts);
    window.addEventListener("beforeunload", saveCurrentDrafts);
    return () => {
      window.removeEventListener("pagehide", saveCurrentDrafts);
      window.removeEventListener("beforeunload", saveCurrentDrafts);
      saveCurrentDrafts();
    };
  }, []);
  const setAssetsOpen = (next: boolean | ((prev: boolean) => boolean)) => {
    setAssetsOpenState((prev) => {
      const value = typeof next === "function" ? next(prev) : next;
      assetPanelOpenRef.current = value;
      setAssetPanelOpen(value);
      return value;
    });
  };
  useEffect(() => {
    assetPanelOpenRef.current = assetPanelOpen;
    if (assetPanelOpen != null) setAssetsOpenState(assetPanelOpen);
  }, [assetPanelOpen]);
  useEffect(() => {
    const id = d?.operation.id;
    if (!id) {
      setOperationAssets([]);
      setPreviewAsset(null);
      setAssetsLoading(false);
      return;
    }
    let active = true;
    setAssetsLoading(true);
    getJSON<Asset[]>(`/api/v1/assets?operation_id=${id}`).then((assets) => {
      if (!active) return;
      const next = assets ?? [];
      setOperationAssets(next);
      setAssetsOpenState(assetPanelOpenRef.current ?? next.length > 0);
      setPreviewAsset((prev) => {
        if (!prev) return null;
        return next.find((asset) => asset.id === prev.id) ?? null;
      });
    }).finally(() => {
      if (active) setAssetsLoading(false);
    });
    return () => { active = false; };
  }, [d]);

  function openTelemetry(run: Run) {
    restoreReplyFocusRef.current = document.activeElement === replyTextareaRef.current;
    app.setSelectedRun(run.id);
    setTelemetryRun(run);
  }
  function telemetryOpenChange(next: boolean) {
    if (next) return;
    setTelemetryRun(null);
    app.setSelectedRun(null);
    if (restoreReplyFocusRef.current) {
      restoreReplyFocusRef.current = false;
      requestAnimationFrame(() => replyTextareaRef.current?.focus());
    }
  }
  function assigneeChange(v: string) {
    if (!d) return;
    if (v === "me") app.reassign(d.operation.id, "user", app.user.id);
    else { const [k, id] = v.split(":"); app.reassign(d.operation.id, k, id); }
  }
  const darkNow = resolvedTheme === "dark" || theme === "console-dark";
  function toggleTheme() {
    setTheme(theme?.startsWith("console-") ? (darkNow ? "console-light" : "console-dark") : (darkNow ? "light" : "dark"));
  }
  if (!open) {
    return <TelemetryDialog run={telemetryRun} open={telemetryRun != null} onOpenChange={telemetryOpenChange} />;
  }
  if (!d) {
    return (
      <div className="flex min-h-0 flex-1 flex-col bg-background">
        <header className="flex h-12 shrink-0 items-center gap-3 border-b border-border px-4">
          <Button variant="ghost" size="icon-sm" onClick={app.backOperation} title="Back"><ArrowLeft /></Button>
          <Loader2 className="size-4 animate-spin text-muted-foreground" />
          <span className="flex-1 text-sm text-muted-foreground">Loading operation...</span>
          <div className="flex items-center gap-2">
            <SignalsMenu />
            <Button variant="ghost" size="icon-sm" onClick={toggleTheme} title={darkNow ? "Theme: dark" : "Theme: light"} aria-label="Toggle theme">
              {darkNow ? <Moon /> : <Sun />}
            </Button>
          </div>
        </header>
      </div>
    );
  }

  const fire = onFire(d.operation);
  const seenComments = new Set<string>();
  const comments = [...olderComments, ...(d.comments ?? [])].filter((c) => {
    if (seenComments.has(c.id)) return false;
    seenComments.add(c.id);
    return true;
  });
  const activity = compactActivityComments(comments);
  const orderedActivity = commsOrder === "oldest_bottom" ? [...activity].reverse() : activity;
  const visibleActivity = orderedActivity;
  const latestRun = latestOperationRun(runs);
  const latestRunHasComment = latestRun
    ? comments.some((c) => runForPilotComment(c, runs)?.id === latestRun.id)
    : false;
  const settledRun = !activeRun && latestRun && isSettledProblemRun(latestRun) && !latestRunHasComment
    ? latestRun
    : null;
  const timestamp = (value: string) => formatTimestamp(value, timeFormat);
  const operationId = d.operation.id;
  const operationTitle = d.operation.title;
  const operationMission = app.missions.find((m) => m.id === d.operation.mission_id);
  const currentOperationCode = operationCode(d.operation, app.missions);
  const pendingMission = pendingMissionId
    ? app.missions.find((m) => m.id === pendingMissionId) ?? null
    : null;
  const missionMoveConfirmOk =
    missionMoveConfirm.trim().toUpperCase() === currentOperationCode.toUpperCase();
  const missionMoveNotice = missionMoveFromMetadata(d.operation.metadata);
  async function confirmMissionMove() {
    if (!pendingMissionId || !missionMoveConfirmOk || missionMoving) return;
    setMissionMoving(true);
    setMissionMoveError(null);
    const ok = await app.setOperationMission(operationId, pendingMissionId);
    setMissionMoving(false);
    if (!ok) {
      setMissionMoveError("Could not move this operation.");
      return;
    }
    setPendingMissionId(null);
    setMissionMoveConfirm("");
  }
  const operationWorktree = worktreeValue(d.operation.metadata);
  const missionWorktree = worktreeValue(operationMission?.metadata);
  const fleetWorktree = worktreeValue(app.fleets.find((f) => f.id === app.fleet)?.metadata) ?? true;
  const effectiveWorktree = operationWorktree ?? missionWorktree ?? fleetWorktree;
  const worktreeSelectValue = operationWorktree === undefined ? "inherit" : operationWorktree ? "on" : "off";
  const effectiveWorktreeLabel = effectiveWorktree ? "On" : "Off";
  const sourceActions = d.source_actions ?? [];
  const sourceRover = app.rovers.find((rover) => rover.id === d.source_rover_id);
  const sourceRepo = sourceRepoInfo(sourceActions, sourceRover?.metadata);
  const operationWorktreeName = d?.operation ? metadataStringValue(d.operation.metadata, "worktree_name") : "";
  const operationWorktreePath = operationWorktreePathInfo(runs, sourceActions);
  const pullRequests = d.pull_requests ?? [];
  const showSource = d.source_action_available || sourceActions.length > 0;
  const showPullRequests = showSource || pullRequests.length > 0;
  function setWorktreeOverride(v: string) {
    app.setOperationWorktree(operationId, v === "inherit" ? null : v === "on");
  }
  function focusedDraftTarget() {
    const active = document.activeElement;
    if (active === replyTextareaRef.current) return "comment";
    if (active === bodyTextareaRef.current) return "body";
    return null;
  }
  function rememberUploadInsertTarget() {
    autoInsertUploadTargetRef.current = focusedDraftTarget();
  }
  function openAssetPicker() {
    assetUploadRef.current?.click();
  }
  async function uploadOperationFiles(files: FileList | File[] | null | undefined) {
    const selected = Array.from(files ?? []);
    if (selected.length === 0 || assetUploading) return;
    const autoInsertTarget = autoInsertUploadTargetRef.current;
    autoInsertUploadTargetRef.current = null;
    setAssetUploading(true);
    try {
      for (const file of selected) {
        const asset = await app.uploadAsset(file, { operationId });
        if (asset) {
          setOperationAssets((prev) => mergeAssets(prev, [asset]));
          if (autoInsertTarget) insertAssetLink(asset, autoInsertTarget);
          else setAssetsOpen(true);
        }
      }
    } finally {
      setAssetUploading(false);
      if (assetUploadRef.current) assetUploadRef.current.value = "";
    }
  }
  async function deleteOperationAsset(asset: Asset) {
    if (assetDeletingId) return;
    setAssetDeletingId(asset.id);
    setAssetDeleteError(null);
    try {
      const res = await del(`/api/v1/assets/${asset.id}`);
      if (!res.ok) {
        setAssetDeleteError("Could not delete this attachment.");
        return;
      }
      setOperationAssets((prev) => prev.filter((item) => item.id !== asset.id));
      setPreviewAsset((prev) => prev?.id === asset.id ? null : prev);
      setAssetDeleteTarget(null);
    } finally {
      setAssetDeletingId(null);
    }
  }
  function handlePaste(e: React.ClipboardEvent<HTMLDivElement>) {
    const files = Array.from(e.clipboardData.files ?? []);
    if (files.length === 0) return;
    e.preventDefault();
    autoInsertUploadTargetRef.current = focusedDraftTarget();
    void uploadOperationFiles(files);
  }
  function handleDragOver(e: React.DragEvent<HTMLDivElement>) {
    if (!Array.from(e.dataTransfer.types).includes("Files")) return;
    e.preventDefault();
  }
  function handleDrop(e: React.DragEvent<HTMLDivElement>) {
    const files = Array.from(e.dataTransfer.files ?? []);
    if (files.length === 0) return;
    e.preventDefault();
    autoInsertUploadTargetRef.current = focusedDraftTarget();
    void uploadOperationFiles(files);
  }
  function insertAssetLink(asset: Asset, target = insertTarget) {
    if (target === "body") {
      setBodyEditDraft((prev) => {
        const next = appendAssetLink(prev, asset);
        bodyEditDraftRef.current = next;
        return next;
      });
      setEditingBody(true);
    } else {
      setComment((prev) => {
        const next = appendAssetLink(prev, asset);
        commentRef.current = next;
        return next;
      });
    }
  }
  async function saveOperationBody() {
    const title = titleEditDraft.trim();
    if (!title) return;
    const ok = await app.updateOperation(operationId, { title, body: bodyEditDraft });
    if (!ok) return;
    localStorage.removeItem(operationEditDraftKey(operationId));
    editingBodyRef.current = false;
    operationBodyRef.current = { id: operationId, body: bodyEditDraft };
    setTitleEditDraft(title);
    setEditingBody(false);
  }
  function resetOperationBodyDraft() {
    const body = operationBodyRef.current?.body ?? "";
    localStorage.removeItem(operationEditDraftKey(operationId));
    bodyEditDraftRef.current = body;
    setTitleEditDraft(operationTitle);
    setBodyEditDraft(body);
    requestAnimationFrame(() => titleInputRef.current?.focus());
  }
  function cancelOperationBodyEdit() {
    const body = operationBodyRef.current?.body ?? "";
    localStorage.removeItem(operationEditDraftKey(operationId));
    editingBodyRef.current = false;
    bodyEditDraftRef.current = body;
    setTitleEditDraft(operationTitle);
    setBodyEditDraft(body);
    setEditingBody(false);
  }
  async function loadOlderComments() {
    const before = comments[0]?.id;
    if (!before || loadingOlderComments) return;
    setLoadingOlderComments(true);
    const page = await getJSON<{ comments: Comment[]; comments_more: boolean }>(
      `/api/v1/comments?operation_id=${operationId}&before=${before}&limit=${COMMENT_PREVIEW_LIMIT}`,
    );
    setLoadingOlderComments(false);
    if (!page) return;
    setOlderComments((prev) => [...page.comments, ...prev]);
    setCommentsMore(page.comments_more);
  }
  const loadOlderButton = commentsMore && (
    <Button variant="ghost" size="sm" className="w-full text-xs text-muted-foreground" onClick={loadOlderComments} disabled={loadingOlderComments}>
      {loadingOlderComments ? "Loading earlier communications..." : "Load earlier communications"}
    </Button>
  );
  const assignedPilot = assigneeHasPilot(d.operation, app.crews);
  const canAddSubOperation = !d.operation.main_operation_id;
  const waitingOnSubOperations = operationWaitingOnSubOperations(d.operation);
  const activeRunStartedAt = activeRun ? new Date(activeRun.created_at).getTime() : 0;
  const activeRunInput = activeRun ? activeRunInputPreview(comments, runs, activeRun, d.operation) : "";
  const activeRunInputCommentIds = activeRun ? activeRunInputComments(comments, runs, activeRun).reduce((ids, c) => ids.add(c.id), new Set<string>()) : new Set<string>();
  const queuedCommentIds = activeRun ? comments.reduce((ids, c) => {
    if (c.author_type === "user" && new Date(c.created_at).getTime() > activeRunStartedAt) ids.add(c.id);
    return ids;
  }, new Set<string>()) : new Set<string>();
  const activityRows = groupActivityRows(visibleActivity, activeRunInputCommentIds, queuedCommentIds);
  const queuedReplies = queuedCommentIds.size;
  function sendReply() {
    const body = comment.trim();
    if (!body) return;
    app.addComment(operationId, body);
    setComment("");
  }
  function clearReply() {
    clearCommentCreateDraftSaveTimer();
    pendingDraftSaveRef.current = null;
    commentRef.current = "";
    setComment("");
    localStorage.removeItem(commentCreateDraftKey(operationId));
    requestAnimationFrame(() => replyTextareaRef.current?.focus());
  }
  function quoteComment(c: Comment, selectedText: string) {
    const quote = quotedReplyBody(commentAuthor(c, app.user, app.members, app.pilots), selectedText || c.body);
    if (!quote) return;
    setInsertTarget("comment");
    setComment((prev) => prev.trim() ? `${prev.trimEnd()}\n\n${quote}` : quote);
    requestAnimationFrame(() => replyTextareaRef.current?.focus());
  }
  const replyComposer = (
    <div className="shrink-0 px-4 pb-3 pt-1.5 lg:px-10">
      <div className="mx-auto flex w-full max-w-[78rem]">
        <div className="min-w-0 flex-1 pr-6">
          <div className="mx-auto w-full max-w-[52rem]">
            <div className={cn("ufo-reply-composer rounded-lg border border-foreground/20 bg-background ring-1 ring-border/80 transition-colors focus-within:border-brand/70 focus-within:ring-2 focus-within:ring-brand/35", activeRun && "ufo-active-composer")}>
              {activeRun && (
                <div className="px-3 py-1.5">
                  <ActiveRunBanner run={activeRun} operationId={d.operation.id} inputPreview={activeRunInput} onTelemetry={openTelemetry} />
                </div>
              )}
              <form
                className="min-w-0"
                onSubmit={(e) => { e.preventDefault(); sendReply(); }}
                onKeyDown={(e) => {
                  if (e.nativeEvent.isComposing) return;
                  if (e.key === "Escape") {
                    e.preventDefault();
                    if (e.target instanceof HTMLElement) e.target.blur();
                  }
                  if (e.key === "Enter" && !e.shiftKey && e.target instanceof HTMLTextAreaElement) {
                    e.preventDefault();
                    sendReply();
                  }
                }}
              >
                <div className="px-3 py-1">
                  <div className="flex items-start gap-2">
                    <Textarea ref={replyTextareaRef} value={comment} onFocus={() => setInsertTarget("comment")} onChange={(e) => { commentRef.current = e.target.value; setComment(e.target.value); }} placeholder="Reply…" className="min-h-20 max-h-[160px] flex-1 resize-none overflow-hidden border-0 bg-transparent px-2 py-1.5 shadow-none focus-visible:ring-0" />
                    <div className="flex w-[5.75rem] shrink-0 items-center justify-end gap-1 pt-0.5">
                      <Button type="button" variant="ghost" size="icon-sm" className="text-muted-foreground" title="Clear reply" aria-label="Clear reply" disabled={!comment} onMouseDown={(e) => e.preventDefault()} onClick={clearReply}>
                        <RotateCcw className="size-3.5" />
                      </Button>
                      <Button type="button" variant="ghost" size="icon-sm" className="text-muted-foreground" title="Add attachment" aria-label="Add attachment" disabled={assetUploading} onMouseDown={rememberUploadInsertTarget} onClick={openAssetPicker}>
                        {assetUploading ? <Loader2 className="size-3.5 animate-spin" /> : <Paperclip className="size-3.5" />}
                      </Button>
                      <Button type="button" size="icon-sm" className="size-8 rounded-full bg-brand/85 text-brand-foreground hover:bg-brand/95" title="Send" aria-label="Send reply" disabled={!comment.trim()} onClick={sendReply}><ArrowUp className="size-5" /></Button>
                    </div>
                  </div>
                  <div className="flex items-center justify-between gap-2 px-2 pt-1 text-[11px] text-muted-foreground">
                    <span>{assignedPilot ? (activeRun ? "Pilot is already working. Your reply will queue for the next turn." : "Replying resumes the pilot's session.") : ""}</span>
                  </div>
                </div>
              </form>
            </div>
          </div>
        </div>
        <div className="hidden w-72 shrink-0 lg:block" />
      </div>
    </div>
  );
  const assetsPanel = (
    <OperationAssetsPanel
      assets={operationAssets}
      loading={assetsLoading}
      uploading={assetUploading}
      open={assetsOpen}
      view={assetView}
      source={assetSource}
      deletingAssetId={assetDeletingId}
      previewAsset={previewAsset}
      onToggle={() => setAssetsOpen((v) => !v)}
      onView={setAssetView}
      onSource={setAssetSource}
      onPreview={(asset) => setPreviewAsset((prev) => prev?.id === asset.id ? null : asset)}
      onClearPreview={() => setPreviewAsset(null)}
      onUpload={openAssetPicker}
      onInsert={insertAssetLink}
      onDelete={(asset) => { setAssetDeleteError(null); setAssetDeleteTarget(asset); }}
      insertTarget={insertTarget}
    />
  );

  return (
    <>
      <div className="flex min-h-0 flex-1 flex-col bg-background" onPaste={handlePaste} onDragOver={handleDragOver} onDrop={handleDrop}>
        <header className="flex h-12 shrink-0 items-center gap-3 border-b border-border px-4">
          <Button variant="ghost" size="icon-sm" onClick={app.backOperation} title="Back"><ArrowLeft /></Button>
          <div className="flex min-w-0 flex-1 items-center gap-2">
            <StatusIcon status={d.operation.status} subOperations={waitingOnSubOperations} className="size-3.5" />
            <span className="flex h-4 items-center font-mono text-[11px] font-medium uppercase text-muted-foreground">{operationCode(d.operation, app.missions)}</span>
            <span className="flex h-4 min-w-0 items-center truncate text-sm font-medium">{d.operation.title}</span>
          </div>
          <div className="flex items-center gap-2">
            {(d.operation.status === "done" || d.operation.status === "canceled") && (
              <Button size="sm" variant="ghost" onClick={() => app.setArchived(d.operation.id, !d.operation.archived)} title={d.operation.archived ? "Unarchive" : "Archive"}>
                {d.operation.archived ? <ArchiveRestore /> : <Archive />} {d.operation.archived ? "Unarchive" : "Archive"}
              </Button>
            )}
            <SignalsMenu />
            <Button variant="ghost" size="icon-sm" onClick={toggleTheme} title={darkNow ? "Theme: dark" : "Theme: light"} aria-label="Toggle theme">
              {darkNow ? <Moon /> : <Sun />}
            </Button>
          </div>
        </header>

        <div className="min-h-0 flex-1 overflow-hidden px-4 pt-4 lg:px-10">
          <div className="mx-auto flex h-full min-h-0 w-full max-w-[78rem]">
            {/* main */}
            <div className="min-w-0 flex-1 overflow-y-auto pb-2 pr-6">
              <div className="mx-auto w-full max-w-[52rem] space-y-4">
                {editingBody ? (
                  <Input
                    ref={titleInputRef}
                    value={titleEditDraft}
                    onChange={(e) => setTitleEditDraft(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== "Escape") return;
                      e.preventDefault();
                      cancelOperationBodyEdit();
                    }}
                    placeholder="Title"
                    className="h-auto px-2 py-1 text-xl font-semibold leading-snug"
                  />
                ) : (
                  <h1 className="text-xl font-semibold leading-snug">{d.operation.title}</h1>
                )}
                {d.operation.orchestrating && (
                  <Badge variant="secondary" className="gap-1 text-[10px]"><Users className="size-2.5" /> Captain Orchestrating Sub-Operations</Badge>
                )}
                <div className="space-y-1.5">
                  {editingBody ? (
                    <Textarea
                      ref={bodyTextareaRef}
                      value={bodyEditDraft}
                      onFocus={() => setInsertTarget("body")}
                      onChange={(e) => { bodyEditDraftRef.current = e.target.value; setBodyEditDraft(e.target.value); }}
                      onKeyDown={(e) => {
                        if (e.key !== "Escape") return;
                        e.preventDefault();
                        cancelOperationBodyEdit();
                      }}
                      rows={6}
                      className="resize-y"
                    />
                  ) : (
                    d.operation.body && <Markdown assets={operationAssets}>{d.operation.body}</Markdown>
                  )}
                  <div className="flex flex-wrap items-center justify-between gap-2 pt-1">
                    <ReactionBar reactions={d.operation.reactions ?? []} onToggle={(e, on) => app.react("operations", d.operation.id, e, d.operation.id, on)} />
                    <EditActions
                      editing={editingBody}
                      onEdit={() => { setInsertTarget("body"); setTitleEditDraft(d.operation.title); setEditingBody(true); requestAnimationFrame(() => titleInputRef.current?.focus()); }}
                      onReset={resetOperationBodyDraft}
                      resetDisabled={titleEditDraft === d.operation.title && bodyEditDraft === (d.operation.body ?? "")}
                      onCancel={cancelOperationBodyEdit}
                      onSave={saveOperationBody}
                      saveDisabled={!titleEditDraft.trim()}
                    />
                  </div>
                </div>
                {d.operation.status === "in_review" && d.runs.some((r) => r.needs_input) && (
                  <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 p-3 text-sm">
                    <MessageCircleQuestion className="size-4 shrink-0 text-warning" />
                    <span>A pilot is waiting for your input — reply below to continue.</span>
                  </div>
                )}
                {settledRun && <SettledRunBanner run={settledRun} onTelemetry={openTelemetry} />}

                {canAddSubOperation && (
                  <SubOperationEntry
                    adding={addingSubOperation}
                    mainId={d.operation.id}
                    missionId={d.operation.mission_id}
                    subOperations={d.sub_operations}
                    onAdd={() => setAddingSubOperation(true)}
                    onDone={() => setAddingSubOperation(false)}
                    onCancel={() => setAddingSubOperation(false)}
                  />
                )}
                <input ref={assetUploadRef} type="file" multiple className="sr-only" onChange={(e) => uploadOperationFiles(e.target.files)} />
                {assetsPanel}

                <div className="space-y-3">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-2">
                      <h2 className="text-sm font-semibold">Communications</h2>
                      {queuedReplies > 0 && (
                        <span
                          title="New replies are queued for the pilot's next turn."
                          className="inline-flex items-center gap-1 rounded-full bg-warning/10 px-1.5 py-0.5 text-[11px] text-warning ring-1 ring-warning/25"
                        >
                          <Layers className="size-3" />
                          {queuedReplies}
                        </span>
                      )}
                    </div>
                    <div className="flex items-center gap-1">
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        className="size-7 text-muted-foreground"
                        title={commsOrder === "oldest_top" ? "Oldest top" : "Oldest bottom"}
                        aria-label="Toggle communications order"
                        onClick={() => setCommsOrder(commsOrder === "oldest_top" ? "oldest_bottom" : "oldest_top")}
                      >
                        <ArrowUpDown className="size-3.5" />
                      </Button>
                    </div>
                  </div>
                  {d.comments.length === 0 && <p className="text-sm text-muted-foreground">No communications yet.</p>}
                  {commsOrder === "oldest_top" && loadOlderButton}
                  {activityRows.map((row) => "commentGroup" in row ? (
                    <div
                      key={`${row.status}-${row.commentGroup.map((r) => r.comment.id).join("-")}`}
                      className={cn(
                        "rounded-lg border p-3",
                        row.status === "working" ? "ufo-active-composer border-warning/40 bg-brand/5" : "border-warning/25 bg-warning/5",
                      )}
                    >
                      <div className="mb-1.5 flex items-center gap-1.5 text-[10px] font-medium">
                        {row.status === "working" ? (
                          <span className="text-brand">{row.commentGroup.length} Comments</span>
                        ) : (
                          <span className="inline-flex items-center gap-1 text-warning">
                            <Clock className="size-3" />
                            {row.commentGroup.length} Queued Comments
                          </span>
                        )}
                      </div>
                      <div className="space-y-1.5">
                        {row.commentGroup.map((item) => (
                          <CommentRow
                            key={item.comment.id}
                            c={item.comment}
                            operationId={d.operation.id}
                            pilotStatus={item.pilotStatus}
                            captainSplit={item.captainSplit}
                            run={runForPilotComment(item.comment, runs)}
                            queued={row.status === "queued"}
                            processing={row.status === "working"}
                            processingFrame={false}
                            showStatusBadge={row.status === "working"}
                            onTelemetry={openTelemetry}
                            timeFormat={timeFormat}
                            assets={operationAssets}
                            onQuote={quoteComment}
                          />
                        ))}
                      </div>
                    </div>
                  ) : (
                    <div key={row.comment.id} className={cn(!activeRunInputCommentIds.has(row.comment.id) && "px-3")}>
                      <CommentRow
                        c={row.comment}
                        operationId={d.operation.id}
                        pilotStatus={row.pilotStatus}
                        captainSplit={row.captainSplit}
                        run={runForPilotComment(row.comment, runs)}
                        queued={queuedCommentIds.has(row.comment.id)}
                        processing={activeRunInputCommentIds.has(row.comment.id)}
                        onTelemetry={openTelemetry}
                        timeFormat={timeFormat}
                        assets={operationAssets}
                        onQuote={quoteComment}
                      />
                    </div>
                  ))}
                  {commsOrder === "oldest_bottom" && loadOlderButton}
                </div>
              </div>
            </div>

            {/* properties rail */}
            <div className="min-h-0 w-72 shrink-0 overflow-y-auto border-l border-border bg-muted/20">
              <div className="divide-y divide-border/60 text-sm">
                <div className="space-y-0.5 p-4">
                  <PropRow label="Status">
                    <RailSelect value={d.operation.status} onValueChange={(v) => app.moveOperation(d.operation.id, v)}>
                      {STATUSES.map((s) => <SelectItem key={s} value={s}><span className="flex items-center gap-2"><StatusIcon status={s} className="size-3.5" /> {STATUS_LABEL[s]}</span></SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Assignee">
                    <RailSelect value={operationAssigneeValue(d.operation, app.user)} onValueChange={assigneeChange} placeholder="Unassigned">
                      <SelectItem value="me">{userLabel(app.user)}</SelectItem>
                      {sortedCrews.map((c) => <SelectItem key={`c${c.id}`} value={`crew:${c.id}`}><CrewOption crew={c} crewIcon="emoji" /></SelectItem>)}
                      {sortedPilots.map((p) => <SelectItem key={`p${p.kind}`} value={`pilot:${p.kind}`} disabled={p.rovers === 0}><PilotOption kind={p.kind} unavailable={p.rovers === 0} /></SelectItem>)}
                      {sortedMembers.map((m) => <SelectItem key={`u${m.id}`} value={`user:${m.id}`}>🧑 {m.name || m.email}</SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Priority">
                    <RailSelect value={String(d.operation.priority)} onValueChange={(v) => app.setPriority(d.operation.id, Number(v))}>
                      {PRIORITY.map((p, i) => <SelectItem key={i} value={String(i)}><span className="flex items-center gap-2"><PriorityIcon level={i} className="size-3.5" /> {p.label}</span></SelectItem>)}
                    </RailSelect>
                  </PropRow>
                  <PropRow label="Mission">
                    {d.operation.main_operation_id ? (
                      <span className="truncate font-mono text-xs" title={operationMission?.name}>{operationMission?.key ?? "—"}</span>
                    ) : (
                      <RailSelect
                        value={d.operation.mission_id}
                        onValueChange={(missionId) => {
                          if (missionId === d.operation.mission_id) return;
                          setPendingMissionId(missionId);
                          setMissionMoveConfirm("");
                          setMissionMoveError(null);
                        }}
                      >
                        {app.missions.map((m) => (
                          <SelectItem key={m.id} value={m.id}>
                            <span className="font-mono text-xs" title={m.name}>{m.key}</span>
                          </SelectItem>
                        ))}
                      </RailSelect>
                    )}
                  </PropRow>
                  {missionMoveNotice && (
                    <div
                      className="mx-0 mt-1 rounded-md border border-warning/35 bg-warning/10 px-2 py-1.5 text-[11px] leading-snug text-warning"
                      title={missionMoveNotice.at ? `Moved at ${missionMoveNotice.at}` : undefined}
                    >
                      Moved from{" "}
                      <span className="font-mono font-medium">
                        {missionMoveNotice.from_key}
                      </span>
                      {missionMoveNotice.from_name ? ` (${missionMoveNotice.from_name})` : ""}
                      . Prior mission context no longer applies to new runs.
                    </div>
                  )}
                </div>

                <div className="p-4">
                  <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><Tags className="size-3.5" /> Labels</p>
                  <Labels op={d.operation} />
                </div>

                <div className="p-4">
                  <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><Antenna className="size-3.5" /> Dispatch · rover tags</p>
                  <div className="space-y-1.5">
                    <div className="flex items-start gap-2 text-xs">
                      <span className="w-12 shrink-0 pt-1 text-muted-foreground">Need</span>
                      <TagEditor tags={d.operation.required_tags ?? []} onChange={(t) => app.setOperationTags(d.operation.id, t, d.operation.excluded_tags ?? [])} placeholder="any" />
                    </div>
                    <div className="flex items-start gap-2 text-xs">
                      <span className="w-12 shrink-0 pt-1 text-muted-foreground">Avoid</span>
                      <TagEditor tags={d.operation.excluded_tags ?? []} onChange={(t) => app.setOperationTags(d.operation.id, d.operation.required_tags ?? [], t)} placeholder="none" />
                    </div>
                  </div>
                </div>

                <div className="p-4">
                  <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><Link2 className="size-3.5" /> Relationships</p>
                  <Relationships op={d.operation} relations={d.relations ?? []} />
                </div>

                {showSource && (
                  <div className="p-4">
                    <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><GitBranch className="size-3.5" /> Source</p>
                    <div className="mb-2 space-y-1">
                      <PropRow label="Repo">
                        <span className="min-w-0 truncate font-mono text-xs" title={sourceRepo?.address}>{sourceRepo?.address ?? "Unknown until rover reports"}</span>
                      </PropRow>
                      {sourceRepo?.path && sourceRepo.path !== sourceRepo.address && (
                        <PropRow label="Checkout">
                          <span className="min-w-0 truncate font-mono text-xs" title={sourceRepo.path}>{sourceRepo.path}</span>
                        </PropRow>
                      )}
                      <PropRow label="Worktree">
                        <RailSelect value={worktreeSelectValue} onValueChange={setWorktreeOverride}>
                          <SelectItem value="inherit"><span className="flex items-center gap-2"><GitBranch className="size-3.5" /> Inherit ({effectiveWorktreeLabel})</span></SelectItem>
                          <SelectItem value="on"><span className="flex items-center gap-2"><GitBranch className="size-3.5" /> On</span></SelectItem>
                          <SelectItem value="off"><span className="flex items-center gap-2"><GitBranch className="size-3.5" /> Off</span></SelectItem>
                        </RailSelect>
                      </PropRow>
                      <PropRow label="Worktree path">
                        <span className="min-w-0 truncate font-mono text-xs" title={operationWorktreePath || operationWorktreeName || undefined}>{operationWorktreePath || operationWorktreeName || "Pending first dispatch"}</span>
                      </PropRow>
                    </div>
                    <SourceActions operationId={d.operation.id} worktreeEnabled={effectiveWorktree} actionAvailable={d.source_action_available} actions={sourceActions} timeFormat={timeFormat} />
                  </div>
                )}

                {showPullRequests && (
                  <div className="p-4">
                    <p className="mb-1.5 flex items-center gap-1.5 text-[11px] font-medium uppercase text-muted-foreground"><GitPullRequest className="size-3.5" /> Pull requests</p>
                    <PullRequests operationId={d.operation.id} />
                  </div>
                )}

                <div className="space-y-1.5 p-4 text-xs text-muted-foreground">
                  <div className="flex items-center justify-between gap-2">
                    <span>Created by</span>
                    {d.operation.created_by ? (
                      <button type="button" className="truncate text-foreground hover:underline" onClick={() => app.openUser(d.operation.created_by!)}>
                        {memberLabel(d.operation.created_by, app.user, app.members, "—")}
                      </button>
                    ) : (
                      <span className="text-foreground">—</span>
                    )}
                  </div>
                  <div className="flex items-center justify-between"><span>Created</span><span>{timestamp(d.operation.created_at)}</span></div>
                  <div className="flex items-center justify-between"><span>Updated</span><span>{timestamp(d.operation.updated_at)}</span></div>
                  {d.operation.started_at && <div className="flex items-center justify-between"><span>Started</span><span>{timestamp(d.operation.started_at)}</span></div>}
                  {d.operation.finished_at && <div className="flex items-center justify-between"><span>Finished</span><span>{timestamp(d.operation.finished_at)}</span></div>}
                  <div className="flex items-center justify-between gap-2"><span>Start</span><DateField value={d.operation.start_date} onChange={(v) => app.setDates(d.operation.id, v, d.operation.due_date)} title="Planned start date" /></div>
                  <div className="flex items-center justify-between gap-2"><span>Due</span><DateField value={d.operation.due_date} onChange={(v) => app.setDates(d.operation.id, d.operation.start_date, v)} /></div>
                </div>
              </div>
            </div>
          </div>
        </div>
        {replyComposer}
        {fire && <DetailFire />}
      </div>
      <TelemetryDialog run={telemetryRun} open={telemetryRun != null} onOpenChange={telemetryOpenChange} />
      <AssetDeleteDialog
        asset={assetDeleteTarget}
        open={assetDeleteTarget != null}
        deleting={assetDeletingId != null}
        error={assetDeleteError}
        onOpenChange={(next) => { if (!next) { setAssetDeleteTarget(null); setAssetDeleteError(null); } }}
        onConfirm={deleteOperationAsset}
      />
      <Dialog
        open={pendingMissionId != null}
        onOpenChange={(next) => {
          if (missionMoving) return;
          if (!next) {
            setPendingMissionId(null);
            setMissionMoveConfirm("");
            setMissionMoveError(null);
          }
        }}
      >
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>Move operation to another mission?</DialogTitle>
            <DialogDescription>
              This changes the display code, clears the pilot session, and switches
              stacked context to the target mission. Sub-operations move with it.
              Fleet stays the same.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3 text-sm">
            <div className="rounded-md border border-border bg-muted/30 px-3 py-2 font-mono text-xs">
              {operationMission?.key ?? "—"}
              {operationMission?.name ? ` (${operationMission.name})` : ""}
              {" → "}
              {pendingMission?.key ?? "—"}
              {pendingMission?.name ? ` (${pendingMission.name})` : ""}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="mission-move-confirm" className="block text-xs leading-snug text-muted-foreground">
                Type <span className="font-mono text-foreground">{currentOperationCode}</span> to confirm
              </Label>
              <Input
                id="mission-move-confirm"
                value={missionMoveConfirm}
                onChange={(e) => setMissionMoveConfirm(e.target.value)}
                placeholder={currentOperationCode}
                autoComplete="off"
                spellCheck={false}
                className="font-mono text-sm"
                disabled={missionMoving}
                onKeyDown={(e) => {
                  if (e.key !== "Enter") return;
                  e.preventDefault();
                  void confirmMissionMove();
                }}
              />
            </div>
            {missionMoveError && (
              <div className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                {missionMoveError}
              </div>
            )}
          </div>
          <div className="mt-4 flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              disabled={missionMoving}
              onClick={() => {
                setPendingMissionId(null);
                setMissionMoveConfirm("");
                setMissionMoveError(null);
              }}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              disabled={!missionMoveConfirmOk || !pendingMissionId || missionMoving}
              onClick={() => void confirmMissionMove()}
            >
              {missionMoving ? <Loader2 className="size-4 animate-spin" /> : null}
              Move operation
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}

function OperationAssetsPanel({
  assets,
  loading,
  uploading,
  open,
  view,
  source,
  deletingAssetId,
  previewAsset,
  insertTarget,
  onToggle,
  onView,
  onSource,
  onPreview,
  onClearPreview,
  onUpload,
  onInsert,
  onDelete,
}: {
  assets: Asset[];
  loading: boolean;
  uploading: boolean;
  open: boolean;
  view: AssetViewMode;
  source: AssetSourceFilter;
  deletingAssetId: string | null;
  previewAsset: Asset | null;
  insertTarget: "comment" | "body";
  onToggle: () => void;
  onView: (view: AssetViewMode) => void;
  onSource: (source: AssetSourceFilter) => void;
  onPreview: (asset: Asset) => void;
  onClearPreview: () => void;
  onUpload: () => void;
  onInsert: (asset: Asset) => void;
  onDelete: (asset: Asset) => void;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const sourceCounts = assetSourceCounts(assets);
  const filteredAssets = source === "all" ? assets : assets.filter((asset) => assetSource(asset) === source);
  const currentPreview = previewAsset ? filteredAssets.find((asset) => asset.id === previewAsset.id) : null;
  const previewableAssets = filteredAssets.filter(canPreviewAsset);
  useEffect(() => {
    if (!currentPreview) return;
    panelRef.current?.querySelector<HTMLElement>("[data-asset-preview]")
      ?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [currentPreview]);
  useEffect(() => {
    if (!currentPreview) return;
    const currentPreviewID = currentPreview.id;
    function onKeyDown(event: KeyboardEvent) {
      const target = event.target as HTMLElement | null;
      if (target?.closest("input, textarea, [contenteditable='true']")) return;
      if (event.key === "Escape") {
        event.preventDefault();
        onClearPreview();
        return;
      }
      if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key) || previewableAssets.length < 2) return;
      const index = previewableAssets.findIndex((asset) => asset.id === currentPreviewID);
      if (index < 0) return;
      event.preventDefault();
      const next = view !== "list"
        ? gridPreviewTarget(panelRef.current, filteredAssets, currentPreviewID, event.key)
        : previewableAssets[(index + (event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : -1) + previewableAssets.length) % previewableAssets.length];
      if (next) onPreview(next);
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [currentPreview, filteredAssets, onClearPreview, onPreview, previewableAssets, view]);
  useEffect(() => {
    if (!currentPreview) return;
    function onPointerUp(event: PointerEvent) {
      if (event.button !== 0) return;
      const panel = panelRef.current;
      const target = event.target;
      if (!panel || !(target instanceof Node) || panel.contains(target)) return;
      if (target instanceof Element && target.closest("button, a, input, textarea, select, [role='button'], [role='menuitem'], [role='option'], [contenteditable='true']")) return;
      const selection = window.getSelection();
      if (selection && !selection.isCollapsed && selection.toString().trim()) return;
      onClearPreview();
    }
    document.addEventListener("pointerup", onPointerUp);
    return () => document.removeEventListener("pointerup", onPointerUp);
  }, [currentPreview, onClearPreview]);
  function onPanelPointerUp(event: React.PointerEvent<HTMLDivElement>) {
    if (!currentPreview || event.button !== 0) return;
    const target = event.target as HTMLElement | null;
    if (!target || target.closest("button, a, input, textarea, select, [contenteditable='true'], [data-asset-id], [data-asset-preview]")) return;
    const selection = window.getSelection();
    if (selection && !selection.isCollapsed && selection.toString().trim()) return;
    onClearPreview();
  }
  if (!loading && !uploading && assets.length === 0) return null;
  return (
    <div ref={panelRef} className="space-y-1 rounded-md border border-border bg-muted/20 p-2" onPointerUp={onPanelPointerUp}>
      <div className="flex min-w-0 items-center gap-2 px-2">
        <button onClick={onToggle} className="inline-flex min-w-0 flex-1 cursor-pointer items-center gap-1.5 rounded py-0.5 text-left text-[11px] font-medium uppercase text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground active:bg-brand/15 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
          {open ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
          <span className="truncate">Attachments</span>
          {!open && <AttachmentCount count={assets.length} />}
          {loading && <Loader2 className="size-3 animate-spin" />}
        </button>
        {open && assets.length > 0 && (
          <div className="flex shrink-0 items-center rounded-md bg-muted/60 p-0.5">
            <Button type="button" variant="ghost" size="icon-sm" className={cn("size-6 text-muted-foreground", view === "grid" && "bg-background text-foreground shadow-sm")} title="Grid view" aria-label="Grid view" aria-pressed={view === "grid"} onClick={() => onView("grid")}>
              <Grid2x2 className="size-3.5" />
            </Button>
            <Button type="button" variant="ghost" size="icon-sm" className={cn("size-6 text-muted-foreground", view === "compact_grid" && "bg-background text-foreground shadow-sm")} title="Compact grid view" aria-label="Compact grid view" aria-pressed={view === "compact_grid"} onClick={() => onView("compact_grid")}>
              <Grid3x3 className="size-3.5" />
            </Button>
            <Button type="button" variant="ghost" size="icon-sm" className={cn("size-6 text-muted-foreground", view === "list" && "bg-background text-foreground shadow-sm")} title="List view" aria-label="List view" aria-pressed={view === "list"} onClick={() => onView("list")}>
              <List className="size-3.5" />
            </Button>
          </div>
        )}
        <Button type="button" variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title="Add attachment" aria-label="Add attachment" disabled={uploading} onClick={onUpload}>
          {uploading ? <Loader2 className="size-3.5 animate-spin" /> : <Plus className="size-3.5" />}
        </Button>
      </div>
      {open && (
        <div className="space-y-1">
          {assets.length === 0 ? (
            <div className="px-2 py-3 text-xs text-muted-foreground">No attachments.</div>
          ) : (
            <>
              <AssetSourceFilterBar source={source} counts={sourceCounts} onSource={onSource} />
              {filteredAssets.length === 0 ? (
                <div className="px-2 py-3 text-xs text-muted-foreground">No matching attachments.</div>
              ) : view === "list" ? (
                <AssetList assets={filteredAssets} currentPreview={currentPreview} deletingAssetId={deletingAssetId} insertTarget={insertTarget} onPreview={onPreview} onInsert={onInsert} onDelete={onDelete} />
              ) : (
                <AssetGrid assets={filteredAssets} compact={view === "compact_grid"} currentPreview={currentPreview} deletingAssetId={deletingAssetId} insertTarget={insertTarget} onPreview={onPreview} onInsert={onInsert} onDelete={onDelete} />
              )}
            </>
          )}
          {currentPreview && canPreviewAsset(currentPreview) && (
            <div data-asset-preview className="mx-2 overflow-hidden rounded-md border border-border bg-background">
              <div className="flex min-w-0 items-center gap-2 border-b border-border px-2 py-1.5 text-xs">
                <AssetKindIcon asset={currentPreview} />
                <span className="min-w-0 flex-1 truncate font-medium">{currentPreview.filename}</span>
                <AssetTextCopyButton asset={currentPreview} />
                <Button variant="ghost" size="icon-sm" className="size-6 shrink-0 text-muted-foreground" title="Close preview" aria-label="Close preview" onClick={onClearPreview}>
                  <X className="size-3.5" />
                </Button>
              </div>
              <AssetPreview asset={currentPreview} renderMarkdown={(text) => <Markdown>{text}</Markdown>} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function assetSourceCounts(assets: Asset[]) {
  return assets.reduce<Record<AssetSource, number>>((counts, asset) => {
    counts[assetSource(asset)] += 1;
    return counts;
  }, { upload: 0, output: 0 });
}

function gridPreviewTarget(root: HTMLElement | null, assets: Asset[], currentID: string, key: string) {
  const nodes = Array.from(root?.querySelectorAll<HTMLElement>("[data-asset-id]") ?? []);
  const ids = nodes.map((node) => node.dataset.assetId).filter(Boolean) as string[];
  const current = ids.indexOf(currentID);
  if (current < 0) return null;
  const firstTop = nodes[0]?.offsetTop ?? 0;
  const columns = Math.max(1, nodes.filter((node) => Math.abs(node.offsetTop - firstTop) < 2).length);
  const byID = new Map(assets.map((asset) => [asset.id, asset]));
  const pick = (start: number, step: number, stop: (index: number) => boolean) => {
    for (let i = start; stop(i); i += step) {
      const asset = byID.get(ids[i]);
      if (asset && canPreviewAsset(asset)) return asset;
    }
    return null;
  };
  if (key === "ArrowUp") return pick(current - columns, -columns, (i) => i >= 0);
  if (key === "ArrowDown") return pick(current + columns, columns, (i) => i < ids.length);
  const rowStart = Math.floor(current / columns) * columns;
  const rowEnd = Math.min(rowStart + columns, ids.length);
  if (key === "ArrowLeft") return pick(current - 1, -1, (i) => i >= rowStart);
  if (key === "ArrowRight") return pick(current + 1, 1, (i) => i < rowEnd);
  return null;
}

function AssetSourceFilterBar({ source, counts, onSource }: { source: AssetSourceFilter; counts: Record<AssetSource, number>; onSource: (source: AssetSourceFilter) => void }) {
  const options: { value: AssetSourceFilter; label: string; count: number }[] = [
    { value: "all", label: "All", count: counts.upload + counts.output },
    { value: "upload", label: "Uploads", count: counts.upload },
    { value: "output", label: "Outputs", count: counts.output },
  ];
  return (
    <div className="flex px-2 pb-1">
      <div className="inline-flex rounded-md bg-muted/60 p-0.5">
        {options.map((option) => (
          <button
            key={option.value}
            type="button"
            className={cn("inline-flex h-6 items-center rounded px-2 text-[11px] text-muted-foreground", source === option.value && "bg-background text-foreground shadow-sm")}
            aria-pressed={source === option.value}
            onClick={() => onSource(option.value)}
          >
            {option.label} <AttachmentCount count={option.count} className="ml-1" />
          </button>
        ))}
      </div>
    </div>
  );
}

function AttachmentCount({ count, className }: { count: number; className?: string }) {
  return (
    <span className={cn("inline-flex h-4 min-w-4 shrink-0 items-center justify-center rounded-full border border-border bg-background px-1 font-mono text-[10px] tabular-nums text-muted-foreground shadow-sm", className)}>
      {count}
    </span>
  );
}

function AssetGrid({ assets, compact = false, currentPreview, deletingAssetId, insertTarget, onPreview, onInsert, onDelete }: { assets: Asset[]; compact?: boolean; currentPreview?: Asset | null; deletingAssetId: string | null; insertTarget: "comment" | "body"; onPreview: (asset: Asset) => void; onInsert: (asset: Asset) => void; onDelete: (asset: Asset) => void }) {
  return (
    <div className={cn("grid px-2 pb-1", compact ? "grid-cols-[repeat(auto-fill,minmax(5.75rem,1fr))] gap-1.5" : "grid-cols-[repeat(auto-fill,minmax(8.5rem,1fr))] gap-2")}>
      {assets.map((asset) => {
        const previewable = canPreviewAsset(asset);
        const selected = currentPreview?.id === asset.id;
        const contentURL = asset.url || assetFilePath(asset.id);
        const tileClass = cn(
          "group relative min-w-0 overflow-hidden rounded-md border border-border bg-background text-left text-sm transition-colors",
          selected ? "border-brand/50 bg-brand/5 shadow-sm ring-1 ring-brand/30" : "hover:bg-accent/60",
        );
        const body = (
          <>
            <AssetTileMedia asset={asset} compact={compact} />
            <div className={cn("min-w-0 border-t border-border/60", compact ? "px-1.5 py-1" : "px-2 py-1.5")}>
              <div className={cn("flex min-w-0 items-center", compact ? "gap-1" : "gap-1.5")}>
                <div className={cn("min-w-0 flex-1 truncate font-medium", compact ? "text-[10px]" : "text-xs")}>{asset.filename}</div>
              </div>
              {compact ? (
                <div className="mt-0.5 flex min-w-0 items-center gap-1 text-[9px] text-muted-foreground">
                  <AssetSourceIcon asset={asset} />
                  <span className="truncate">{assetKindLabel(asset)}</span>
                </div>
              ) : (
                <>
                  <div className="mt-0.5 flex min-w-0 items-center gap-1.5 text-[10px] text-muted-foreground">
                    <AssetSourceIcon asset={asset} />
                    <span className="truncate">{assetKindLabel(asset)}</span>
                    <span className="shrink-0 tabular-nums">{formatBytes(asset.byte_size)}</span>
                  </div>
                  <div className="text-[10px] text-muted-foreground">{formatAssetDate(asset.created_at)}</div>
                </>
              )}
            </div>
          </>
        );
        return (
          <div key={asset.id} data-asset-id={asset.id} className={tileClass}>
            {previewable ? (
              <button type="button" className="block w-full rounded-md text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset" title={`Preview ${asset.filename}`} aria-label={`Preview ${asset.filename}`} aria-pressed={selected} onClick={(e) => { e.currentTarget.blur(); onPreview(asset); }}>
                {body}
              </button>
            ) : body}
            <AssetActions asset={asset} contentURL={contentURL} deleting={deletingAssetId === asset.id} insertTarget={insertTarget} onInsert={onInsert} onDelete={onDelete} className="absolute right-1 top-1 z-10 rounded-md bg-background/80 opacity-0 shadow-sm backdrop-blur transition-opacity group-hover:opacity-100 group-focus-within:opacity-100" />
          </div>
        );
      })}
    </div>
  );
}

function AssetList({ assets, currentPreview, deletingAssetId, insertTarget, onPreview, onInsert, onDelete }: { assets: Asset[]; currentPreview?: Asset | null; deletingAssetId: string | null; insertTarget: "comment" | "body"; onPreview: (asset: Asset) => void; onInsert: (asset: Asset) => void; onDelete: (asset: Asset) => void }) {
  return (
    <div className="divide-y divide-border/70 px-2 pb-1">
      {assets.map((asset) => {
        const previewable = canPreviewAsset(asset);
        const selected = currentPreview?.id === asset.id;
        const contentURL = asset.url || assetFilePath(asset.id);
        const rowClass = cn("group flex min-w-0 items-center gap-2 rounded-md border border-transparent py-2", previewable && "cursor-pointer text-left hover:bg-accent/60", selected && "border-brand/50 bg-brand/5 shadow-sm ring-1 ring-brand/30");
        const body = (
          <>
            <AssetKindIcon asset={asset} className="size-4 shrink-0 text-muted-foreground" />
            <div className="min-w-0 flex-1">
              <div className="flex min-w-0 items-center gap-1.5">
                <div className="min-w-0 truncate text-sm font-medium">{asset.filename}</div>
              </div>
              <div className="flex min-w-0 gap-2 text-[11px] text-muted-foreground">
                <span className="inline-flex min-w-0 items-center gap-1.5">
                  <AssetSourceIcon asset={asset} />
                  <span className="truncate">{assetKindLabel(asset)}</span>
                </span>
                <span className="shrink-0 tabular-nums">{formatBytes(asset.byte_size)}</span>
                <span className="shrink-0">{formatAssetDate(asset.created_at)}</span>
              </div>
            </div>
          </>
        );
        return (
          <div key={asset.id} data-asset-id={asset.id} className={rowClass}>
            {previewable ? (
              <button type="button" className="flex min-w-0 flex-1 items-center gap-2 rounded-md focus:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset" title={`Preview ${asset.filename}`} aria-label={`Preview ${asset.filename}`} aria-pressed={selected} onClick={(e) => { e.currentTarget.blur(); onPreview(asset); }}>
                {body}
              </button>
            ) : (
              <div className="flex min-w-0 flex-1 items-center gap-2">{body}</div>
            )}
            <AssetActions asset={asset} contentURL={contentURL} deleting={deletingAssetId === asset.id} insertTarget={insertTarget} onInsert={onInsert} onDelete={onDelete} className="shrink-0 opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100" />
          </div>
        );
      })}
    </div>
  );
}

function AssetTileMedia({ asset, compact = false }: { asset: Asset; compact?: boolean }) {
  return (
    <div className="flex aspect-[4/3] items-center justify-center bg-muted/40">
      {isImageAsset(asset) ? (
        <img src={assetInlineContentURL(asset)} alt="" className="h-full w-full object-cover" />
      ) : (
        <div className={cn("flex flex-col items-center text-muted-foreground", compact ? "gap-0.5" : "gap-1")}>
          <AssetKindIcon asset={asset} className={cn("text-muted-foreground", compact ? "size-6" : "size-9")} />
          <span className={cn("truncate font-medium uppercase", compact ? "max-w-16 text-[8px]" : "max-w-24 text-[10px]")}>{assetExtension(asset).replace(".", "") || assetKindLabel(asset)}</span>
        </div>
      )}
    </div>
  );
}

function AssetActions({
  asset,
  contentURL,
  deleting,
  insertTarget,
  onInsert,
  onDelete,
  className,
}: {
  asset: Asset;
  contentURL: string;
  deleting: boolean;
  insertTarget: "comment" | "body";
  onInsert: (asset: Asset) => void;
  onDelete: (asset: Asset) => void;
  className?: string;
}) {
  return (
    <div className={cn("flex gap-1 p-0.5", className)}>
      <Button variant="ghost" size="icon-sm" className="size-6 text-muted-foreground" title={`Insert into ${insertTarget === "body" ? "body" : "reply"}`} aria-label={`Insert ${asset.filename} into ${insertTarget === "body" ? "body" : "reply"}`} onClick={() => onInsert(asset)}>
        <Link2 className="size-3.5" />
      </Button>
      <Button asChild variant="ghost" size="icon-sm" className="size-6 text-muted-foreground" title="Download file" aria-label={`Download ${asset.filename}`}>
        <a href={contentURL} target="_blank" rel="noreferrer"><Download className="size-3.5" /></a>
      </Button>
      <Button variant="ghost" size="icon-sm" className="size-6 text-muted-foreground hover:text-destructive" title="Delete attachment" aria-label={`Delete ${asset.filename}`} disabled={deleting} onClick={() => onDelete(asset)}>
        {deleting ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
      </Button>
    </div>
  );
}

function mergeAssets(...groups: Asset[][]) {
  const seen = new Set<string>();
  const merged: Asset[] = [];
  for (const group of groups) {
    for (const asset of group) {
      if (seen.has(asset.id)) continue;
      seen.add(asset.id);
      merged.push(asset);
    }
  }
  return merged;
}

// A compact label-left / control-right property row for the detail rail.
function PropRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-8 items-center justify-between gap-2">
      <span className="text-xs text-muted-foreground">{label}</span>
      <div className="flex min-w-0 justify-end">{children}</div>
    </div>
  );
}

// Borderless compact Select for the rail (value reads as inline text until hovered).
function RailSelect({ value, onValueChange, placeholder, children }: { value: string; onValueChange: (v: string) => void; placeholder?: string; children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <Select value={value} onValueChange={onValueChange} open={open} onOpenChange={setOpen}>
      <SelectTrigger className={cn(
        RAIL_CONTROL_CLASS,
        "w-auto cursor-pointer gap-1",
        open && "text-foreground",
      )} style={open ? { backgroundColor: "color-mix(in oklch, var(--foreground) 12%, var(--background))", boxShadow: "inset 0 0 0 2px var(--foreground)" } : undefined}><SelectValue placeholder={placeholder} /></SelectTrigger>
      <SelectContent>{children}</SelectContent>
    </Select>
  );
}

// Low-key date control that still opens the native picker explicitly.
function DateField({ value, onChange, title }: { value: string | null; onChange: (v: string | null) => void; title?: string }) {
  const inputRef = useRef<HTMLInputElement>(null);
  function openPicker() {
    const input = inputRef.current;
    if (!input) return;
    try {
      input.showPicker();
    } catch {
      input.focus();
      input.click();
    }
  }
  return (
    <>
      <button type="button" onClick={openPicker} title={title} className={cn(RAIL_CONTROL_CLASS, "group inline-flex h-4 min-h-0 items-center px-1 py-0 leading-4")}>
        <span className={cn(!value && "text-muted-foreground group-hover:text-accent-foreground")}>
          {value ? literalDateLabel(value) : "Set date"}
        </span>
      </button>
      <input ref={inputRef} type="date" value={value ?? ""} onChange={(e) => onChange(e.target.value || null)} className="sr-only" tabIndex={-1} />
    </>
  );
}

function literalDateLabel(value: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!match) return value;
  const month = Number(match[2]);
  const day = Number(match[3]);
  const monthLabel = DATE_MONTH_LABELS[month - 1];
  return monthLabel && day >= 1 && day <= 31 ? `${monthLabel} ${day}` : value;
}

function pilotStatusFromComment(c: Comment) {
  if (c.author_type !== "system" || !c.body.startsWith(PILOT_STATUS_PREFIX)) return null;
  const status = c.body.slice(PILOT_STATUS_PREFIX.length).trim();
  return STATUS_LABEL[status] ? status : null;
}

function captainSplitFromComment(c: Comment) {
  if (c.author_type !== "system") return null;
  return c.body.match(CAPTAIN_SPLIT_RE)?.[1] ?? null;
}

function compactActivityComments(comments: Comment[]) {
  const rows: ActivityCommentRow[] = [];
  for (const c of comments) {
    const status = pilotStatusFromComment(c);
    const split = captainSplitFromComment(c);
    const prev = rows[rows.length - 1];
    if (status && prev?.comment.author_type === "pilot") {
      prev.pilotStatus = status;
      continue;
    }
    if (status) {
      rows.push({ comment: c, pilotStatus: status });
      continue;
    }
    if (split && prev?.comment.author_type === "pilot") {
      prev.captainSplit = split;
      continue;
    }
    rows.push({ comment: c });
  }
  return rows;
}

function groupActivityRows(rows: ActivityCommentRow[], processingIds: Set<string>, queuedIds: Set<string>) {
  const out: ActivityDisplayRow[] = [];
  for (let i = 0; i < rows.length;) {
    const status = processingIds.has(rows[i].comment.id) ? "working" : queuedIds.has(rows[i].comment.id) ? "queued" : null;
    if (!status) {
      out.push(rows[i]);
      i += 1;
      continue;
    }
    const group: ActivityCommentRow[] = [];
    while (i < rows.length) {
      const nextStatus = processingIds.has(rows[i].comment.id) ? "working" : queuedIds.has(rows[i].comment.id) ? "queued" : null;
      if (nextStatus !== status) break;
      group.push(rows[i]);
      i += 1;
    }
    out.push(group.length > 1 ? { commentGroup: group, status } : group[0]);
  }
  return out;
}

function runForPilotComment(c: Comment, runs: Run[]) {
  if (c.author_type !== "pilot" || !c.author_pilot_kind) return null;
  const at = new Date(c.created_at).getTime();
  // Nearest preceding run by the same pilot.
  return runs
    .filter((r) => r.pilot === c.author_pilot_kind && new Date(r.created_at).getTime() <= at)
    .sort((a, b) => b.created_at.localeCompare(a.created_at))[0] ?? null;
}

function latestOperationRun(runs: Run[]) {
  return [...runs].sort((a, b) => b.created_at.localeCompare(a.created_at))[0] ?? null;
}

function isSettledProblemRun(run: Run) {
  return run.status === "failed" || run.status === "blocked" || run.status === "canceled";
}

function oneLinePreview(text: string, max = 120) {
  const value = hideFlowControlFlags(text).replace(/\s+/g, " ").trim();
  if (value.length <= max) return value;
  return `${value.slice(0, max - 1).trimEnd()}…`;
}

function activeRunInputPreview(comments: Comment[], runs: Run[], run: Run, operation: Operation) {
  const latest = latestActiveRunInputComment(comments, runs, run);
  return oneLinePreview(latest?.body || operation.body || operation.title);
}

function activeRunInputComments(comments: Comment[], runs: Run[], run: Run) {
  const startedAt = new Date(run.created_at).getTime();
  if (!Number.isFinite(startedAt)) return [];
  const previousStartedAt = [...runs]
    .filter((r) => r.id !== run.id && new Date(r.created_at).getTime() < startedAt)
    .sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
  const after = previousStartedAt ? new Date(previousStartedAt.created_at).getTime() : -Infinity;
  return comments.filter((c) => {
    const createdAt = new Date(c.created_at).getTime();
    return c.author_type === "user" && Number.isFinite(createdAt) && createdAt > after && createdAt <= startedAt;
  });
}

function latestActiveRunInputComment(comments: Comment[], runs: Run[], run: Run) {
  return activeRunInputComments(comments, runs, run).sort((a, b) => b.created_at.localeCompare(a.created_at))[0] ?? null;
}

function marqueeDuration(text: string) {
  return `${Math.min(80, Math.max(16, Math.ceil(text.length / 3)))}s`;
}

function runStatusLabel(run: Run) {
  if (run.status === "canceled") return "Canceled";
  return run.status.slice(0, 1).toUpperCase() + run.status.slice(1);
}

function SplitIcon() {
  return (
    <span className="inline-flex size-4 items-center justify-center rounded-full bg-info/15 text-info">
      <svg viewBox="0 0 14 14" className="size-3" aria-hidden>
        <path d="M4 7 H7 L10 4 M7 7 L10 10" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
        <circle cx="4" cy="7" r="1.2" fill="currentColor" />
        <circle cx="10" cy="4" r="1.2" fill="currentColor" />
        <circle cx="10" cy="10" r="1.2" fill="currentColor" />
      </svg>
    </span>
  );
}

function quotedReplyBody(author: string, source: string) {
  const text = hideFlowControlFlags(source).trim();
  if (!text) return "";
  const clipped = text.length > 1200 ? `${text.slice(0, 1200).trimEnd()}…` : text;
  return `> ${author}:\n${clipped.split(/\r?\n/).map((line) => `> ${line}`).join("\n")}\n\n`;
}

function EditActions({
  editing,
  onEdit,
  onReset,
  resetDisabled,
  onCancel,
  onSave,
  saveDisabled,
  className,
  buttonClassName,
}: {
  editing: boolean;
  onEdit?: () => void;
  onReset?: () => void;
  resetDisabled?: boolean;
  onCancel?: () => void;
  onSave?: () => void;
  saveDisabled?: boolean;
  className?: string;
  buttonClassName?: string;
}) {
  const buttonClass = cn("text-muted-foreground", buttonClassName);
  if (!editing) {
    return (
      <div className={cn("flex items-center gap-1", className)}>
        <Button variant="ghost" size="icon-sm" className={buttonClass} title="Edit" aria-label="Edit" onClick={onEdit}>
          <Pencil className="size-3.5" />
        </Button>
      </div>
    );
  }
  return (
    <div className={cn("flex items-center gap-1", className)}>
      <Button variant="ghost" size="icon-sm" className={buttonClass} title="Reset draft" aria-label="Reset draft" disabled={resetDisabled} onClick={onReset}>
        <RotateCcw className="size-3.5" />
      </Button>
      <Button variant="ghost" size="icon-sm" className={buttonClass} title="Cancel edit" aria-label="Cancel edit" onClick={onCancel}>
        <X className="size-3.5" />
      </Button>
      <Button variant="ghost" size="icon-sm" className={buttonClass} title="Save edit" aria-label="Save edit" disabled={saveDisabled} onClick={onSave}>
        <Check className="size-3.5" />
      </Button>
    </div>
  );
}

function CommentRow({ c, operationId, pilotStatus, captainSplit, run, queued = false, processing = false, processingFrame = true, showStatusBadge = true, onTelemetry, timeFormat, assets, onQuote }: { c: Comment; operationId: string; pilotStatus?: string; captainSplit?: string; run: Run | null; queued?: boolean; processing?: boolean; processingFrame?: boolean; showStatusBadge?: boolean; onTelemetry: (run: Run) => void; timeFormat: TimeFormat; assets: Asset[]; onQuote: (c: Comment, selectedText: string) => void }) {
  const app = useApp();
  const rowRef = useRef<HTMLDivElement>(null);
  const quoteMenuRef = useRef<HTMLDivElement>(null);
  const quoteMenuTextRef = useRef("");
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(c.body);
  const editingRef = useRef(editing);
  const draftRef = useRef(draft);
  const canEditQueuedRef = useRef(false);
  const commentBodyRef = useRef(c.body);
  const isPilot = c.author_type === "pilot";
  const isSystem = c.author_type === "system";
  const active = run ? isActive(run) : false;
  const body = hideFlowControlFlags(c.body);
  const canEditQueued = queued && c.author_type === "user" && c.author_id === app.user.id;
  const systemPilotAction = isSystem && pilotStatus && c.body.startsWith(PILOT_STATUS_PREFIX);
  editingRef.current = editing;
  draftRef.current = draft;
  canEditQueuedRef.current = canEditQueued;
  commentBodyRef.current = c.body;
  useEffect(() => {
    if (!canEditQueued) {
      setDraft(c.body);
      return;
    }
    const key = commentEditDraftKey(c.id);
    const saved = localStorage.getItem(key);
    if (saved != null && saved !== c.body) {
      setDraft(saved);
      setEditing(true);
      return;
    }
    if (saved === c.body) localStorage.removeItem(key);
    setDraft(c.body);
  }, [c.id, c.body, canEditQueued]);
  function saveCurrentCommentEditDraft() {
    if (!editingRef.current || !canEditQueuedRef.current) return;
    writeChangedLocalDraft(commentEditDraftKey(c.id), draftRef.current, commentBodyRef.current);
  }
  useEffect(() => {
    if (!editing || !canEditQueued) return;
    const id = window.setTimeout(saveCurrentCommentEditDraft, DRAFT_SAVE_DELAY_SECONDS * 1000);
    return () => window.clearTimeout(id);
  }, [editing, canEditQueued, draft, c.id, c.body]);
  useEffect(() => {
    window.addEventListener("pagehide", saveCurrentCommentEditDraft);
    window.addEventListener("beforeunload", saveCurrentCommentEditDraft);
    return () => {
      window.removeEventListener("pagehide", saveCurrentCommentEditDraft);
      window.removeEventListener("beforeunload", saveCurrentCommentEditDraft);
      saveCurrentCommentEditDraft();
    };
  }, [c.id]);
  async function saveComment() {
    if (!draft.trim()) return;
    const ok = await app.updateComment(operationId, c.id, draft);
    if (!ok) return;
    localStorage.removeItem(commentEditDraftKey(c.id));
    editingRef.current = false;
    commentBodyRef.current = draft;
    setEditing(false);
  }
  function resetCommentDraft() {
    localStorage.removeItem(commentEditDraftKey(c.id));
    draftRef.current = c.body;
    setDraft(c.body);
  }
  async function removeComment() {
    if (window.confirm("Delete queued comment?")) await app.deleteComment(operationId, c.id);
  }
  function hideQuoteMenu() {
    quoteMenuTextRef.current = "";
    if (quoteMenuRef.current) quoteMenuRef.current.hidden = true;
  }
  function clearQuoteSelection() {
    hideQuoteMenu();
    window.getSelection()?.removeAllRanges();
  }
  function updateQuoteMenu() {
    if (editing || !body.trim() || systemPilotAction) return hideQuoteMenu();
    const row = rowRef.current;
    const menu = quoteMenuRef.current;
    const selection = window.getSelection();
    const text = selectedTextWithin(row);
    if (!row || !menu || !selection || !text || selection.rangeCount === 0) return hideQuoteMenu();
    const range = selection.getRangeAt(0);
    const rects = range.getClientRects();
    const rect = rects.length ? rects[rects.length - 1] : range.getBoundingClientRect();
    const rowRect = row.getBoundingClientRect();
    quoteMenuTextRef.current = text;
    menu.style.left = `${Math.max(0, Math.min(rect.right - rowRect.left - 56, rowRect.width - 56))}px`;
    menu.style.top = `${Math.max(0, rect.top - rowRect.top - 34)}px`;
    menu.hidden = false;
  }
  function quoteSelection() {
    const text = quoteMenuTextRef.current;
    if (!text) return;
    onQuote(c, text);
    clearQuoteSelection();
  }
  function copySelection() {
    const text = quoteMenuTextRef.current;
    if (!text) return;
    void copyText(text);
    clearQuoteSelection();
  }
  function quoteComment() {
    clearQuoteSelection();
    onQuote(c, "");
  }
  return (
    <div ref={rowRef} className={cn("ufo-comment-row relative flex gap-2.5", processing && processingFrame && "ufo-processing-comment")} onMouseUp={updateQuoteMenu} onKeyUp={updateQuoteMenu}>
      <SelectionActionsMenu menuRef={quoteMenuRef} onCopy={copySelection} onQuote={quoteSelection} />
      <Avatar className="size-6">
        <AvatarFallback className={cn(isPilot && "bg-brand/15 text-brand", isSystem && "bg-muted text-muted-foreground")}>
          {isPilot ? <PilotIcon kind={c.author_pilot_kind ?? ""} size={12} /> : isSystem ? "·" : "U"}
        </AvatarFallback>
      </Avatar>
      <div className="flex-1">
        <div className="flex items-center justify-between gap-2">
          <div className="flex min-w-0 items-center gap-2">
            {c.author_type === "user" && c.author_id ? (
              <button
                type="button"
                className={cn("min-w-0 truncate text-sm font-medium hover:underline", isPilot && "text-brand", isSystem && "text-muted-foreground")}
                onClick={() => app.openUser(c.author_id!)}
              >
                {commentAuthor(c, app.user, app.members, app.pilots)}
              </button>
            ) : (
              <span className={cn("min-w-0 truncate text-sm font-medium", isPilot && "text-brand", isSystem && "text-muted-foreground")}>
                {commentAuthor(c, app.user, app.members, app.pilots)}
              </span>
            )}
            <span className="shrink-0 text-[11px] text-muted-foreground">{formatTimestamp(c.created_at, timeFormat)}</span>
            {queued && showStatusBadge && <CommentStatusBadge />}
            {pilotStatus && (
              <span className="inline-flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
                <StatusIcon status={pilotStatus} className="size-3" /> {STATUS_LABEL[pilotStatus]}
              </span>
            )}
            {captainSplit && (
              <span className="inline-flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
                <SplitIcon /> {captainSplit} sub-operations
              </span>
            )}
          </div>
          <div className="ml-auto grid shrink-0 grid-cols-[4.75rem_3.75rem] items-center gap-2">
            <span className="justify-self-end">
              {isPilot && run && (active ? (
                <ActiveRunElapsed run={run} />
              ) : (
                <span className="inline-flex h-6 w-[4.75rem] items-center justify-end text-[11px] tabular-nums text-muted-foreground">
                  {elapsed(run.created_at, new Date(run.updated_at).getTime())}
                </span>
              ))}
            </span>
            <div className={cn(canEditQueued ? "flex w-20 justify-end justify-self-end gap-1" : "grid w-[3.75rem] grid-cols-[1.5rem_2rem] items-center justify-end justify-self-end gap-1")}>
              {isPilot && run && (
                <Button variant="ghost" size="icon-sm" className="size-6 justify-self-center text-muted-foreground" title="Open run log" aria-label="Open run log" onMouseDown={(e) => e.preventDefault()} onClick={() => onTelemetry(run)}>
                  <ScrollText className="size-3.5" />
                </Button>
              )}
              {canEditQueued && !editing && (
                <>
                  <Button variant="ghost" size="icon-sm" className="size-6 text-muted-foreground" title="Delete queued comment" aria-label="Delete queued comment" onClick={removeComment}>
                    <Trash2 className="size-3.5" />
                  </Button>
                  <EditActions editing={false} onEdit={() => setEditing(true)} buttonClassName="size-6" />
                </>
              )}
              {body.trim() && !systemPilotAction && (
                <Button variant="ghost" size="icon-sm" className={cn("size-6 justify-self-center text-muted-foreground", !canEditQueued && "col-start-2")} title="Reply to comment" aria-label="Reply to comment" onMouseDown={(e) => { e.preventDefault(); clearQuoteSelection(); }} onClick={quoteComment}>
                  <Reply className="size-3.5" />
                </Button>
              )}
            </div>
          </div>
        </div>
        {editing ? (
          <div className="mt-1 space-y-1.5">
            <Textarea
              value={draft}
              onChange={(e) => { draftRef.current = e.target.value; setDraft(e.target.value); }}
              onKeyDown={(e) => {
                if (e.key !== "Escape") return;
                e.preventDefault();
                localStorage.removeItem(commentEditDraftKey(c.id));
                editingRef.current = false;
                setDraft(c.body);
                setEditing(false);
              }}
              className="min-h-20 resize-y"
            />
            <EditActions
              editing
              className="justify-end"
              onReset={resetCommentDraft}
              resetDisabled={draft === c.body}
              onCancel={() => { localStorage.removeItem(commentEditDraftKey(c.id)); editingRef.current = false; setDraft(c.body); setEditing(false); }}
              onSave={saveComment}
              saveDisabled={!draft.trim()}
            />
          </div>
        ) : body.trim() && !systemPilotAction && (isSystem ? <p className="text-sm text-muted-foreground">{body}</p> : <Markdown assets={assets}>{body}</Markdown>)}
        {!editing && (
          <div className="mt-1 min-w-0 overflow-hidden">
            <ReactionBar reactions={c.reactions} onToggle={(e, on) => app.react("comments", c.id, e, operationId, on)} />
          </div>
        )}
      </div>
    </div>
  );
}

function CommentStatusBadge() {
  return (
    <span
      title="Queued for the pilot's next turn"
      className="inline-flex items-center gap-1 rounded-full bg-warning/10 px-1.5 py-0.5 text-[10px] font-medium normal-case tracking-normal text-warning"
    >
      <Clock className="size-3" />
      Queued
    </span>
  );
}

// Shared reaction strip: existing reactions (hover → reactors) + an add-emoji menu.
function ReactionBar({ reactions, onToggle }: { reactions: Reaction[]; onToggle: (emoji: string, on?: boolean) => void }) {
  return (
    <TooltipProvider delayDuration={150}>
      <div className="flex flex-wrap items-center gap-1">
        {reactions.map((r) => (
          <Tooltip key={r.emoji}>
            <TooltipTrigger asChild>
              <button
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => onToggle(r.emoji, !r.mine)}
                className={cn(
                  "inline-flex cursor-pointer items-center gap-1 rounded-full border px-1.5 py-0.5 text-base transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  r.mine
                    ? "border-brand bg-brand/10 text-foreground hover:bg-brand/15 active:bg-brand/20"
                    : "border-border bg-background text-muted-foreground hover:border-brand/40 hover:bg-brand/10 hover:text-foreground active:bg-brand/15",
                )}
              >
                <span className="leading-none">{r.emoji}</span>
                <span className="text-xs leading-none">{r.count}</span>
              </button>
            </TooltipTrigger>
            <TooltipContent>{(r.users ?? []).join(", ") || r.emoji}</TooltipContent>
          </Tooltip>
        ))}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button type="button" className="inline-flex size-6 cursor-pointer items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground active:bg-brand/15 data-[state=open]:bg-accent data-[state=open]:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
              <SmilePlus className="size-3.5" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent side="top" align="start" avoidCollisions={false} onCloseAutoFocus={(e) => e.preventDefault()} className="grid w-[18rem] min-w-0 grid-cols-10 gap-0.5 p-1" style={{ zIndex: 1000 }}>
            {EMOJI.map((e) => (
              <DropdownMenuItem key={e} onSelect={() => onToggle(e)} onMouseDown={(event) => event.preventDefault()} className="size-7 justify-center p-0 text-base leading-none">
                {e}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </TooltipProvider>
  );
}

function Labels({ op }: { op: Operation }) {
  const app = useApp();
  const [name, setName] = useState("");
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState<{ id: string; name: string; color: string } | null>(null);
  const [editName, setEditName] = useState("");
  const onOperation = new Set(op.labels.map((l) => l.id));
  const q = query.trim().toLowerCase();
  const available = app.labels.filter((l) => !onOperation.has(l.id) && (!q || l.name.toLowerCase().includes(q)));
  const startRename = (label: { id: string; name: string; color: string }) => {
    setEditing(label);
    setEditName(label.name);
  };
  const cancelRename = () => {
    setEditing(null);
    setEditName("");
  };
  const saveRename = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!editing) return;
    const next = editName.trim();
    if (!next) return;
    await app.updateLabel(editing.id, next, editing.color);
    cancelRename();
  };
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap gap-1">
        {op.labels.map((l) => (
          editing?.id === l.id ? (
            <form key={l.id} onSubmit={saveRename} className={cn("inline-flex items-center gap-1 rounded-full px-1 py-0.5 text-[11px]", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)}>
              <Input value={editName} onChange={(e) => setEditName(e.target.value)} className="h-5 w-24 border-0 bg-transparent px-1 text-[11px] shadow-none" autoFocus />
              <button type="button" onClick={cancelRename} className="opacity-70 hover:opacity-100" title="Cancel"><X className="size-2.5" /></button>
              <button type="submit" className="opacity-70 hover:opacity-100" title="Save label"><Check className="size-2.5" /></button>
            </form>
          ) : (
            <span key={l.id} className={cn("inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px]", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)}>
              {l.name}
              <button onClick={() => startRename(l)} className="opacity-70 hover:opacity-100" title="Rename label"><Pencil className="size-2.5" /></button>
              <button onClick={() => app.detachLabel(op.id, l.id)} className="opacity-70 hover:opacity-100"><X className="size-2.5" /></button>
            </span>
          )
        ))}
      </div>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="sm" className="h-6 px-1.5 text-xs text-muted-foreground"><Plus className="size-3" /> Add label</Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent className="w-56">
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search labels..." className="mb-1 h-7 text-xs" />
          <div className="max-h-44 overflow-auto">
            {available.map((l) => (
              editing?.id === l.id ? (
                <form key={l.id} onSubmit={saveRename} className="flex items-center gap-1 p-1">
                  <Input value={editName} onChange={(e) => setEditName(e.target.value)} className="h-7 flex-1 text-xs" autoFocus />
                  <Button type="button" variant="ghost" size="icon-sm" title="Cancel" onClick={cancelRename}><X className="size-3" /></Button>
                  <Button type="submit" variant="ghost" size="icon-sm" title="Save label"><Check className="size-3" /></Button>
                </form>
              ) : (
                <div key={l.id} className="flex items-center rounded-sm hover:bg-accent hover:text-accent-foreground">
                  <button type="button" onClick={() => app.attachLabel(op.id, l.id)} className="flex min-w-0 flex-1 items-center gap-2 px-2 py-1.5 text-left text-sm">
                    <span className={cn("size-2 shrink-0 rounded-full", LABEL_COLOR[l.color] ?? LABEL_COLOR.gray)} />
                    <span className="truncate">{l.name}</span>
                  </button>
                  <button type="button" onClick={() => startRename(l)} className="shrink-0 px-2 py-1.5 text-muted-foreground hover:text-foreground" title="Rename label" aria-label={`Rename ${l.name}`}>
                    <Pencil className="size-3" />
                  </button>
                </div>
              )
            ))}
          </div>
          <form
            className="flex gap-1 p-1"
            onSubmit={async (e) => {
              e.preventDefault();
              if (!name.trim()) return;
              const l = await app.createLabel(name.trim(), "blue");
              if (l) app.attachLabel(op.id, l.id);
              setName("");
              setQuery("");
            }}
          >
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="new label" className="h-7 text-xs" />
          </form>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

const REL_LABEL: Record<string, string> = {
  blocks: "Blocks", blocked_by: "Blocked by", relates: "Relates to",
  duplicate: "Duplicate of", duplicated_by: "Duplicated by",
};
const REL_ORDER = ["blocks", "blocked_by", "relates", "duplicate", "duplicated_by"];

function Relationships({ op, relations }: { op: Operation; relations: Relation[] }) {
  const app = useApp();
  const [addKind, setAddKind] = useState<string | null>(null);
  const [q, setQ] = useState("");
  const [results, setResults] = useState<OperationReference[]>([]);
  const [selfMatch, setSelfMatch] = useState<OperationReference | null>(null);
  useEffect(() => {
    if (addKind === null) {
      setResults([]);
      setSelfMatch(null);
      return;
    }
    let active = true;
    app.searchOperations(q).then((r) => {
      if (!active) return;
      setResults(r.filter((o) => o.id !== op.id));
      setSelfMatch(q.trim().length > 0 ? (r.find((o) => o.id === op.id) ?? null) : null);
    });
    return () => { active = false; };
  }, [q, addKind, op.id, app.searchOperations]);
  const groups = REL_ORDER.map((k) => ({ k, items: relations.filter((r) => r.kind === k) })).filter((g) => g.items.length > 0);
  return (
    <div className="space-y-2">
      {groups.map((g) => (
        <div key={g.k} className="space-y-0.5">
          <p className="text-[10px] uppercase tracking-wide text-muted-foreground/70">{REL_LABEL[g.k]}</p>
          {g.items.map((r) => (
            <div key={r.id} className="group flex items-center gap-1 text-xs">
              <button onClick={() => app.openOperation(r.operation.id)} className="flex min-w-0 flex-1 cursor-pointer items-center gap-1.5 rounded-md px-1.5 py-1 text-left ring-inset hover:bg-brand/10 hover:text-foreground hover:ring-1 hover:ring-brand/40">
                <StatusIcon status={r.operation.status} className="size-3.5 shrink-0" />
                <span className="font-mono text-[10px] text-muted-foreground">{operationCode(r.operation as Operation, app.missions)}</span>
                <span className="truncate">{r.operation.title}</span>
              </button>
              <button onClick={() => app.removeRelation(r.id, op.id)} className="shrink-0 text-muted-foreground opacity-0 hover:text-destructive group-hover:opacity-100"><X className="size-3" /></button>
            </div>
          ))}
        </div>
      ))}
      {addKind === null ? (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="sm" className="text-xs text-muted-foreground"><Plus className="size-3" /> Add relationship</Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            {REL_ORDER.map((k) => <DropdownMenuItem key={k} onClick={() => { setAddKind(k); setQ(""); setResults([]); }}>{REL_LABEL[k]}…</DropdownMenuItem>)}
          </DropdownMenuContent>
        </DropdownMenu>
      ) : (
        <div className="space-y-1">
          <div className="flex items-center gap-1 text-[10px] uppercase tracking-wide text-muted-foreground/70">
            {REL_LABEL[addKind]}
            <button onClick={() => setAddKind(null)} className="ml-auto hover:text-foreground"><X className="size-3" /></button>
          </div>
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search operations…" className="h-7 text-xs" autoFocus />
          <div className="max-h-40 space-y-0.5 overflow-auto">
            {selfMatch && (
              <div className="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left text-xs text-muted-foreground opacity-70">
                <StatusIcon status={selfMatch.status} className="size-3.5 shrink-0" />
                <span className="font-mono text-[10px]">{operationCode(selfMatch as Operation, app.missions)}</span>
                <span className="min-w-0 flex-1 truncate">{selfMatch.title}</span>
                <span className="text-[10px] uppercase">Current</span>
              </div>
            )}
            {results.map((o) => (
              <button key={o.id} onClick={() => { app.addRelation(op.id, addKind, o.id); setAddKind(null); }} className="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left text-xs hover:bg-accent">
                <StatusIcon status={o.status} className="size-3.5 shrink-0" />
                <span className="font-mono text-[10px] text-muted-foreground">{operationCode(o as Operation, app.missions)}</span>
                <span className="truncate">{o.title}</span>
              </button>
            ))}
            {q && results.length === 0 && !selfMatch && <p className="px-1.5 py-1 text-xs text-muted-foreground">No matches.</p>}
          </div>
        </div>
      )}
    </div>
  );
}

function metadataStringValue(metadata: Record<string, unknown> | undefined, key: string) {
  const value = metadata?.[key];
  return typeof value === "string" ? value.trim() : "";
}

function sourceRepoInfo(actions: SourceAction[], roverMetadata?: Record<string, unknown>) {
  const roverRemote = metadataStringValue(roverMetadata, "source_remote_url");
  const roverPath = metadataStringValue(roverMetadata, "source_path");
  if (roverRemote || roverPath) return { address: roverRemote || roverPath, path: roverPath };
  for (const action of actions) {
    const remote = metadataStringValue(action.metadata, "source_remote_url");
    const path = metadataStringValue(action.metadata, "source_path");
    if (remote || path) return { address: remote || path, path };
  }
  return null;
}

function operationWorktreePathInfo(runs: Run[], actions: SourceAction[]) {
  for (const run of runs) {
    const path = metadataStringValue(run.metadata, "operation_worktree_path");
    if (path) return path;
  }
  for (const action of actions) {
    const path = metadataStringValue(action.metadata, "operation_worktree_path");
    if (path) return path;
  }
  return "";
}

const SOURCE_ACTION_LABEL: Record<SourceAction["kind"], string> = {
  apply_to_source: "Worktree -> source",
  create_source_branch: "Worktree -> source branch",
  refresh_from_source: "Source -> worktree",
};
const SOURCE_ACTION_TITLE: Record<SourceAction["kind"], string> = {
  apply_to_source: "Apply this operation worktree's diff back to the source checkout.",
  create_source_branch: "Commit this operation worktree's changes to a local branch in the source repo.",
  refresh_from_source: "Update this operation worktree from the source checkout, preserving its current diff.",
};
const SOURCE_ACTION_STATUS: Record<SourceAction["status"], string> = {
  queued: "Queued",
  accepted: "Running",
  succeeded: "Done",
  failed: "Failed",
  conflicted: "Conflict",
};
const SOURCE_ACTION_VISIBLE_LIMIT = 3;

function SourceActions({ operationId, worktreeEnabled, actionAvailable, actions, timeFormat }: { operationId: string; worktreeEnabled: boolean; actionAvailable: boolean; actions: SourceAction[]; timeFormat: TimeFormat }) {
  const app = useApp();
  const [busy, setBusy] = useState<SourceAction["kind"] | null>(null);
  const active = actions.some((a) => a.status === "queued" || a.status === "accepted");
  const visible = actions.slice(0, SOURCE_ACTION_VISIBLE_LIMIT);
  const hidden = actions.length - visible.length;
  const canCreate = worktreeEnabled && actionAvailable && !active && busy == null;
  async function create(kind: SourceAction["kind"]) {
    setBusy(kind);
    try {
      await app.createSourceAction(operationId, kind);
    } finally {
      setBusy(null);
    }
  }
  return (
    <div className="space-y-2">
      <div className="grid grid-cols-3 gap-1.5">
        <span title={SOURCE_ACTION_TITLE.apply_to_source}>
          <Button type="button" variant="secondary" size="sm" className="h-7 w-full justify-center gap-1 text-[11px]" disabled={!canCreate} onClick={() => create("apply_to_source")}>
            {busy === "apply_to_source" ? <Loader2 className="size-3 animate-spin" /> : <ArrowUp className="size-3" />} To source
          </Button>
        </span>
        <span title={SOURCE_ACTION_TITLE.create_source_branch}>
          <Button type="button" variant="secondary" size="sm" className="h-7 w-full justify-center gap-1 text-[11px]" disabled={!canCreate} onClick={() => create("create_source_branch")}>
            {busy === "create_source_branch" ? <Loader2 className="size-3 animate-spin" /> : <GitBranch className="size-3" />} Branch
          </Button>
        </span>
        <span title={SOURCE_ACTION_TITLE.refresh_from_source}>
          <Button type="button" variant="secondary" size="sm" className="h-7 w-full justify-center gap-1 text-[11px]" disabled={!canCreate} onClick={() => create("refresh_from_source")}>
            {busy === "refresh_from_source" ? <Loader2 className="size-3 animate-spin" /> : <RefreshCw className="size-3" />} From source
          </Button>
        </span>
      </div>
      {!worktreeEnabled ? <div className="text-xs text-muted-foreground">Worktree is off.</div> : !actionAvailable && visible.length === 0 && <div className="text-xs text-muted-foreground">No source action available.</div>}
      {visible.length > 0 && (
        <div className="space-y-1">
          {visible.map((action) => (
            <div key={action.id} className="space-y-0.5 rounded border border-border/70 bg-background/60 px-2 py-1.5 text-xs">
              <div className="flex min-w-0 items-center gap-1.5">
                {action.kind === "create_source_branch" ? <GitBranch className="size-3 shrink-0 text-muted-foreground" /> : action.kind === "refresh_from_source" ? <RefreshCw className="size-3 shrink-0 text-muted-foreground" /> : <ArrowUp className="size-3 shrink-0 text-muted-foreground" />}
                <span className="min-w-0 flex-1 truncate font-medium">{SOURCE_ACTION_LABEL[action.kind]}</span>
                <span className={cn("shrink-0 text-[11px]", action.status === "succeeded" ? "text-success" : action.status === "failed" || action.status === "conflicted" ? "text-destructive" : "text-warning")}>{SOURCE_ACTION_STATUS[action.status]}</span>
              </div>
              {action.branch_name && <div className="truncate font-mono text-[11px] text-muted-foreground">{action.branch_name}{action.commit_sha ? ` @ ${action.commit_sha.slice(0, 8)}` : ""}</div>}
              {action.message && <div className="line-clamp-2 text-[11px] text-muted-foreground">{action.message}</div>}
              <div className="text-[10px] text-muted-foreground">{formatTimestamp(action.finished_at ?? action.updated_at, timeFormat)}</div>
            </div>
          ))}
          {hidden > 0 && <div className="text-[10px] text-muted-foreground">{hidden} older source {hidden === 1 ? "action" : "actions"} hidden</div>}
        </div>
      )}
    </div>
  );
}

function PullRequests({ operationId }: { operationId: string }) {
  const app = useApp();
  const pullRequests = app.operationDetail?.pull_requests ?? [];
  const [adding, setAdding] = useState(false);
  const [url, setUrl] = useState("");
  const close = () => { setUrl(""); setAdding(false); };
  return (
    <div className="space-y-1.5">
      {pullRequests.map((p) => (
        <div key={p.id} className="flex items-center gap-1.5 text-xs">
          <a href={p.url} target="_blank" rel="noreferrer" className="min-w-0 flex-1 truncate text-info hover:underline">{p.title || p.url}</a>
          <button onClick={() => app.deletePullRequest(p.id, operationId)} className="text-muted-foreground hover:text-destructive"><X className="size-3" /></button>
        </div>
      ))}
      {adding ? (
        <form
          className="flex items-center gap-1"
          onBlur={(e) => {
            const next = e.relatedTarget;
            if (next instanceof Node && e.currentTarget.contains(next)) return;
            close();
          }}
          onSubmit={async (e) => {
            e.preventDefault();
            const next = url.trim();
            if (!next) return;
            await app.addPullRequest(operationId, next, "");
            close();
          }}
        >
          <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="Pull request URL..." className="h-7 text-xs" autoFocus />
          <Button type="submit" variant="ghost" size="icon-sm" className="shrink-0 text-muted-foreground" title="Link pull request" disabled={!url.trim()}>
            <Check className="size-3" />
          </Button>
          <Button type="button" variant="ghost" size="icon-sm" className="shrink-0 text-muted-foreground" title="Cancel" onClick={close}>
            <X className="size-3" />
          </Button>
        </form>
      ) : (
        <Button variant="ghost" size="sm" className="text-xs text-muted-foreground" onClick={() => setAdding(true)}>
          <Plus className="size-3" /> Add pull request
        </Button>
      )}
    </div>
  );
}

function ActiveRunBanner({ run, operationId, inputPreview, onTelemetry }: { run: Run; operationId: string; inputPreview: string; onTelemetry: (run: Run) => void }) {
  const app = useApp();
  return (
    <div className="flex h-8 items-center gap-2.5">
      <Avatar className="size-6 shrink-0">
        <AvatarFallback className="bg-brand/15 text-brand">
          <PilotIcon kind={run.pilot ?? ""} size={12} />
        </AvatarFallback>
      </Avatar>
      <div className="flex h-8 min-w-0 flex-1 items-center gap-2">
        <span className="inline-flex h-6 shrink-0 items-center text-sm font-medium leading-none text-brand">{pilotLabel(run.pilot ?? "pilot")}</span>
        {inputPreview && (
          <span className="ufo-run-input-marquee h-6 min-w-0 flex-1 text-xs text-muted-foreground" title={inputPreview}>
            <span className="ufo-run-input-marquee-track" style={{ animationDuration: marqueeDuration(inputPreview) }}>
              <span>· {inputPreview}</span>
              <span aria-hidden>· {inputPreview}</span>
            </span>
          </span>
        )}
      </div>
      <div className="flex h-8 shrink-0 items-center justify-end gap-2">
        <div className="grid shrink-0 grid-cols-[4.75rem_3.75rem] items-center gap-2">
          <span className="flex h-6 w-[4.75rem] items-center justify-end gap-1 justify-self-end">
            <ActiveRunPill run={run} />
            <ActiveRunElapsed run={run} showIcon={false} />
          </span>
          <div className="grid h-8 w-[3.75rem] shrink-0 grid-cols-[1.5rem_2rem] items-center justify-end justify-self-end gap-1">
            <Button variant="ghost" size="icon-sm" className="size-6 justify-self-center text-muted-foreground" title="Open run log" aria-label="Open run log" onMouseDown={(e) => e.preventDefault()} onClick={() => onTelemetry(run)}>
              <ScrollText className="size-3.5" />
            </Button>
            <Button size="icon-sm" variant="ghost" className="size-8 justify-self-center rounded-full bg-destructive/10 text-destructive hover:bg-destructive/15" title="Stop" aria-label="Stop run" onMouseDown={(e) => e.preventDefault()} onClick={() => app.cancelRun(run.id, operationId)}>
              <span className="block size-3 rounded-[2px] bg-destructive" aria-hidden />
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}

function SettledRunBanner({ run, onTelemetry }: { run: Run; onTelemetry: (run: Run) => void }) {
  const tone = run.status === "canceled"
    ? "border-muted bg-muted/30 text-muted-foreground"
    : "border-destructive/25 bg-destructive/5 text-destructive";
  return (
    <div className={cn("flex items-center gap-2 rounded-md border p-2", tone)}>
      <Avatar className="size-6">
        <AvatarFallback className="bg-background text-muted-foreground">
          <PilotIcon kind={run.pilot ?? ""} size={12} />
        </AvatarFallback>
      </Avatar>
      <div className="flex min-w-0 flex-1 items-center gap-2">
        <span className="truncate text-sm font-medium">{pilotLabel(run.pilot ?? "pilot")}</span>
        <span className="rounded-full bg-background/70 px-2 py-0.5 text-xs font-medium">{runStatusLabel(run)}</span>
        <span className="text-[11px] tabular-nums opacity-80">{elapsed(run.created_at, new Date(run.updated_at).getTime())}</span>
      </div>
      <Button variant="ghost" size="icon-sm" className="size-7 text-current opacity-75 hover:opacity-100" title="Open run log" aria-label="Open run log" onMouseDown={(e) => e.preventDefault()} onClick={() => onTelemetry(run)}>
        <ScrollText className="size-3.5" />
      </Button>
    </div>
  );
}

function SubOperationEntry({ adding, mainId, missionId, subOperations, onAdd, onDone, onCancel }: { adding: boolean; mainId: string; missionId: string | null; subOperations: Operation[]; onAdd: () => void; onDone: () => void; onCancel: () => void }) {
  if (subOperations.length > 0) return <SubOperations mainId={mainId} missionId={missionId} subOperations={subOperations} />;
  if (adding) return <SubOperationForm mainId={mainId} missionId={missionId} onDone={onDone} onCancel={onCancel} />;
  return (
    <Button variant="ghost" size="sm" className="h-7 w-fit gap-1 px-2 text-xs text-muted-foreground" onClick={onAdd}>
      <Plus className="size-3.5" /> Add sub-operation
    </Button>
  );
}

function SubOperations({ mainId, missionId, subOperations }: { mainId: string; missionId: string | null; subOperations: Operation[] }) {
  const [adding, setAdding] = useState(false);
  const [collapsed, setCollapsed] = useState(false);
  const app = useApp();
  const done = subOperations.filter((o) => o.status === "done").length;
  const pilots = subOperations.map((o) => o.assignee_type === "pilot" ? o.assignee_pilot_kind : null).filter(Boolean) as string[];
  return (
    <div className="space-y-1 rounded-md border border-border bg-muted/20 p-2">
      <div className="flex min-w-0 items-center gap-2 px-2">
        <button onClick={() => setCollapsed((v) => !v)} className="inline-flex min-w-0 flex-1 cursor-pointer items-center gap-1.5 rounded py-0.5 text-left text-[11px] font-medium uppercase text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground active:bg-brand/15 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
          {collapsed ? <ChevronRight className="size-3.5 shrink-0" /> : <ChevronDown className="size-3.5 shrink-0" />}
          <span className="truncate">Sub-operations</span>
          {subOperations.length > 0 && <span className="font-mono">{done}/{subOperations.length}</span>}
          <SubOperationPilotStack pilots={pilots} />
        </button>
        <Button variant="ghost" size="icon-sm" className="size-5 text-muted-foreground" title="Add sub-operation" aria-label="Add sub-operation" onClick={() => { setAdding((v) => !v); setCollapsed(false); }}>
          <Plus className="size-3.5" />
        </Button>
      </div>
      {!collapsed && (
        <>
          {adding && <SubOperationForm mainId={mainId} missionId={missionId} onDone={() => setAdding(false)} onCancel={() => setAdding(false)} />}
          {subOperations.map((subOperation) => (
            <button key={subOperation.id} onClick={() => app.openOperation(subOperation.id)} className="flex w-full cursor-pointer items-center gap-2 rounded-md px-2 py-0.5 text-left text-sm transition-colors hover:bg-accent hover:text-accent-foreground active:bg-brand/15 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
              <StatusIcon status={subOperation.status} className="size-3.5 shrink-0" />
              <span className="font-mono text-[10px] text-muted-foreground">{operationCode(subOperation, app.missions)}</span>
              <span className="min-w-0 flex-1 truncate">{subOperation.title}</span>
              <SubOperationAssigneeIcon operation={subOperation} />
            </button>
          ))}
        </>
      )}
    </div>
  );
}

function SubOperationForm({ mainId, missionId, onDone, onCancel }: { mainId: string; missionId: string | null; onDone: () => void; onCancel: () => void }) {
  const app = useApp();
  const [title, setTitle] = useState(() => typeof window === "undefined" ? "" : sessionStorage.getItem(subOperationCreateDraftKey(mainId)) ?? "");
  const titleRef = useRef(title);
  titleRef.current = title;
  function saveCurrentDraft() {
    writeChangedSessionDraft(subOperationCreateDraftKey(mainId), titleRef.current, "");
  }
  function clearDraft() {
    sessionStorage.removeItem(subOperationCreateDraftKey(mainId));
  }
  function clearTitleDraft() {
    clearDraft();
    titleRef.current = "";
    setTitle("");
  }
  useEffect(() => {
    setTitle(sessionStorage.getItem(subOperationCreateDraftKey(mainId)) ?? "");
  }, [mainId]);
  useEffect(() => {
    const id = window.setTimeout(saveCurrentDraft, DRAFT_SAVE_DELAY_SECONDS * 1000);
    return () => window.clearTimeout(id);
  }, [title, mainId]);
  useEffect(() => {
    window.addEventListener("pagehide", saveCurrentDraft);
    window.addEventListener("beforeunload", saveCurrentDraft);
    return () => {
      window.removeEventListener("pagehide", saveCurrentDraft);
      window.removeEventListener("beforeunload", saveCurrentDraft);
      saveCurrentDraft();
    };
  }, [mainId]);
  return (
    <form
      className="flex items-center gap-1 px-2"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!title.trim() || !missionId) return;
        const op = await app.createOperation({ title: title.trim(), body: "", mission_id: missionId, assignee_type: "user", assignee_id: app.user.id, main_operation_id: mainId });
        if (op) { clearDraft(); onDone(); }
      }}
    >
      <Input value={title} onChange={(e) => { titleRef.current = e.target.value; setTitle(e.target.value); }} placeholder="Sub-operation title…" className="h-7 text-xs" autoFocus />
      <Button type="button" variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title="Clear title" aria-label="Clear title" disabled={!title} onClick={clearTitleDraft}><RotateCcw className="size-3.5" /></Button>
      <Button type="button" variant="ghost" size="icon-sm" className="size-7 text-muted-foreground" title="Cancel" aria-label="Cancel" onClick={() => { clearDraft(); onCancel(); }}><X className="size-3.5" /></Button>
      <Button size="sm" className="h-7 px-2 text-xs" disabled={!title.trim() || !missionId}>Add</Button>
    </form>
  );
}

function SubOperationPilotStack({ pilots }: { pilots: string[] }) {
  const unique = [...new Set(pilots)];
  if (unique.length === 0) return null;
  const visible = unique.slice(0, 3);
  const title = unique.map(pilotLabel).join(", ");
  return (
    <span title={title} className="ml-1 inline-flex shrink-0 items-center gap-0.5 normal-case">
      {visible.map((kind) => (
        <span key={kind} className="inline-flex size-4 items-center justify-center rounded-full border border-background bg-card text-muted-foreground">
          <PilotIcon kind={kind} size={11} />
        </span>
      ))}
      {unique.length > visible.length && <span className="inline-flex h-4 min-w-4 items-center justify-center rounded-full border border-background bg-card px-1 text-[9px]">+{unique.length - visible.length}</span>}
    </span>
  );
}

function SubOperationAssigneeIcon({ operation }: { operation: Operation }) {
  const app = useApp();
  if (operation.assignee_type !== "pilot" || !operation.assignee_pilot_kind) return null;
  const pilot = app.pilots.find((p) => p.kind === operation.assignee_pilot_kind);
  const unavailable = !pilot || pilot.rovers === 0;
  return (
    <span
      title={`${pilotLabel(operation.assignee_pilot_kind)}${unavailable ? " — no rover" : ""}`}
      className={cn("inline-flex size-5 shrink-0 items-center justify-center rounded-full border border-border bg-background text-muted-foreground", unavailable && "opacity-40 grayscale")}
    >
      <PilotIcon kind={operation.assignee_pilot_kind} size={12} />
    </span>
  );
}

function ActiveRunElapsed({ run, showIcon = true }: { run: Run; showIcon?: boolean }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  const queued = run.status === "queued";
  const Icon = queued ? Clock : Loader2;
  return (
    <span className={cn(
      "inline-flex h-6 items-center gap-1 text-[11px] leading-none tabular-nums",
      queued ? "text-warning" : "text-info",
    )}>
      {showIcon && <Icon className={cn("size-3", !queued && "animate-spin")} />}
      {queued && "Queued "}
      {elapsed(run.created_at, now)}
    </span>
  );
}

function ActiveRunPill({ run }: { run: Run }) {
  const queued = run.status === "queued";
  return (
    <span
      title={queued ? "Queued" : "Working"}
      aria-label={queued ? "Queued" : "Working"}
      className={cn(
      "flex size-6 shrink-0 items-center justify-center rounded-full",
      queued ? "bg-warning/10 text-warning" : "bg-info/10 text-info",
    )}>
      {queued ? <Clock className="size-3" /> : <Loader2 className="size-3 animate-spin" />}
    </span>
  );
}
