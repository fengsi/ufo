"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { del, fleetPath, getJSON, patchJSON, postJSON, putJSON, putRaw, withFleet } from "@/lib/api";
import { WebSocketClient } from "@/lib/websocket";
import { parseAppPath } from "@/lib/routes";
import type {
  EnrollmentCode, Pilot, Comment, Crew, Fleet, Invitation, Label, Member, MyInvite, OperationReference, Signal, Mission, Operation,
  Asset, AssetUploadIntent, OperationDetail, Routine, RoutineMetadata, RoutineOperationMetadata, Rover, Run, RunDetail, User, UserProfile,
  Pulse,
} from "@/lib/types";

const OP_CODE_SEARCH_RE = /#?([A-Za-z0-9]+)-(\d+)/;
type RoutineCreateInput = {
  mission_id: string;
  title: string;
  body: string;
  metadata: RoutineMetadata;
  operation_metadata?: RoutineOperationMetadata;
};

function operationCodeSearch(q: string): { key: string; sequence: number } | null {
  const m = q.match(OP_CODE_SEARCH_RE);
  return m ? { key: m[1].toUpperCase(), sequence: Number(m[2]) } : null;
}

function worktreeMetadata(metadata: Record<string, unknown> | undefined, enabled: boolean | null): Record<string, unknown> {
  const next = { ...(metadata ?? {}) };
  if (enabled == null) delete next.worktree_enabled;
  else next.worktree_enabled = enabled;
  return next;
}

function contextMetadata(metadata: Record<string, unknown> | undefined, context: string): Record<string, unknown> {
  const next = { ...(metadata ?? {}) };
  const text = context.trim();
  if (text) next.context = text;
  else delete next.context;
  return next;
}

type Ctx = {
  user: User;
  updateUserName: (name: string) => Promise<boolean>;
  fleets: Fleet[];
  fleet: string;
  switchFleet: (id: string) => void;
  createFleet: (name: string, context?: string) => Promise<void>;
  updateFleet: (id: string, name: string) => Promise<boolean>;
  setFleetContext: (context: string) => Promise<boolean>;
  setFleetWorktree: (enabled: boolean) => Promise<void>;
  signOut: () => void;
  // data (board fetches its own pages; provider holds the small lists)
  missions: Mission[];
  missionCounts: Record<string, number>;
  pilots: Pilot[];
  crews: Crew[];
  labels: Label[];
  routines: Routine[];
  rovers: Rover[];
  enrollmentCodes: EnrollmentCode[];
  signals: Signal[];
  newEnrollmentCode: string | null;
  members: Member[];
  myRole: string;
  fleetInvites: Invitation[];
  myInvites: MyInvite[];
  // boardTick increments whenever operations change → the board refetches.
  boardTick: number;
  // selection
  selectedOperation: string | null;
  openOperation: (id: string | null) => void;
  backOperation: () => void;
  operationDetail: OperationDetail | null;
  selectedRun: string | null;
  setSelectedRun: (id: string | null) => void;
  runDetail: RunDetail | null;
  selectedUserId: string | null;
  userProfile: UserProfile | null;
  openUser: (id: string | null) => void;
  // actions
  createOperation: (i: { title: string; body: string; mission_id: string | null; assignee_type: string | null; assignee_id: string | null; start_immediately?: boolean; sub_operations_enabled?: boolean; required_tags?: string[]; excluded_tags?: string[]; asset_ids?: string[]; priority?: number; main_operation_id?: string | null; start_date?: string | null; due_date?: string | null }) => Promise<Operation | null>;
  setOperationTags: (operationId: string, required_tags: string[], excluded_tags: string[]) => Promise<void>;
  setOperationWorktree: (operationId: string, enabled: boolean | null) => Promise<void>;
  updateOperation: (operationId: string, input: { title?: string; body?: string; mission_id?: string }) => Promise<boolean>;
  setOperationMission: (operationId: string, missionId: string) => Promise<boolean>;
  setPriority: (operationId: string, priority: number) => Promise<void>;
  setDates: (operationId: string, start_date: string | null, due_date: string | null) => Promise<void>;
  setMainOperation: (operationId: string, main_operation_id: string | null) => Promise<void>;
  setArchived: (operationId: string, archived: boolean) => Promise<void>;
  createLabel: (name: string, color: string) => Promise<Label | null>;
  updateLabel: (id: string, name: string, color?: string) => Promise<Label | null>;
  deleteLabel: (id: string) => Promise<void>;
  createRoutine: (i: RoutineCreateInput) => Promise<Routine | null>;
  updateRoutine: (id: string, i: RoutineCreateInput) => Promise<Routine | null>;
  deleteRoutine: (id: string) => Promise<void>;
  pulseRoutine: (id: string) => Promise<Pulse | null>;
  attachLabel: (operationId: string, labelId: string) => Promise<void>;
  detachLabel: (operationId: string, labelId: string) => Promise<void>;
  addRelation: (operationId: string, kind: string, target: string) => Promise<void>;
  removeRelation: (relationId: string, operationId: string) => Promise<void>;
  createSourceAction: (operationId: string, kind: "apply_to_source" | "create_source_branch" | "refresh_from_source") => Promise<void>;
  addPullRequest: (operationId: string, url: string, title: string) => Promise<void>;
  deletePullRequest: (pullRequestId: string, operationId: string) => Promise<void>;
  uploadAsset: (file: File, options?: { operationId?: string }) => Promise<Asset | null>;
  searchOperations: (q: string) => Promise<OperationReference[]>;
  react: (kind: "comments" | "operations", id: string, emoji: string, operationId: string, on?: boolean) => Promise<void>;
  reassign: (operationId: string, assignee_type: string | null, assignee_id: string | null) => Promise<void>;
  cancelRun: (runId: string, operationId: string) => Promise<void>;
  moveOperation: (operationId: string, status: string) => Promise<void>;
  addComment: (operationId: string, body: string) => Promise<void>;
  updateComment: (operationId: string, commentId: string, body: string) => Promise<boolean>;
  deleteComment: (operationId: string, commentId: string) => Promise<boolean>;
  addCrew: (name: string) => Promise<void>;
  renameCrew: (id: string, name: string) => Promise<void>;
  delCrew: (id: string) => Promise<void>;
  addMission: (name: string, key: string, context?: string) => Promise<boolean>;
  updateMission: (id: string, name: string, key: string, context?: string) => Promise<boolean>;
  setMissionWorktree: (id: string, enabled: boolean | null) => Promise<void>;
  addMember: (crewId: string, value: string, role: string, userId: string) => Promise<void>;
  removeMember: (crewId: string, member_type: string, member_id: string) => Promise<void>;
  createEnrollmentCode: (i: { name?: string; expiresAt?: string; uses?: number }) => Promise<void>;
  revokeRover: (id: string) => Promise<void>;
  renameRover: (id: string, name: string) => Promise<void>;
  setRoverTags: (id: string, tags: string[]) => Promise<void>;
  setRoverUnits: (id: string, units: number) => Promise<void>;
  revokeEnrollmentCode: (id: string) => Promise<void>;
  savePendingRover: (code: string, input: { name?: string; units?: number; tags?: string[] }) => Promise<EnrollmentCode | null>;
  approvePendingRover: (id: string, input: { fleetId?: string; name?: string; units?: number; tags?: string[] }) => Promise<boolean>;
  denyPendingRover: (id: string) => Promise<boolean>;
  openSignal: (it: Signal) => Promise<void>;
  archiveSignal: (id: string) => Promise<void>;
  invite: (email: string, role: string) => Promise<boolean>;
  revokeInvite: (id: string) => Promise<void>;
  acceptInvite: (id: string, fleetId: string) => Promise<void>;
  declineInvite: (id: string) => Promise<void>;
  setMemberRole: (userId: string, role: string) => Promise<void>;
  removeFleetMember: (userId: string) => Promise<void>;
};

const AppCtx = createContext<Ctx | null>(null);
export const useApp = () => {
  const c = useContext(AppCtx);
  if (!c) throw new Error("useApp outside provider");
  return c;
};

export function AppProvider({ user: initialUser, fleets: initialFleets, initialFleet, children }: { user: User; fleets: Fleet[]; initialFleet: string; children: React.ReactNode }) {
  const [user, setUser] = useState(initialUser);
  const [fleets, setFleets] = useState<Fleet[]>(initialFleets);
  const [fleet, setFleet] = useState(initialFleet);
  const [missions, setMissions] = useState<Mission[]>([]);
  const [missionCounts, setMissionCounts] = useState<Record<string, number>>({});
  const [pilots, setPilots] = useState<Pilot[]>([]);
  const [crews, setCrews] = useState<Crew[]>([]);
  const [labels, setLabels] = useState<Label[]>([]);
  const [routines, setRoutines] = useState<Routine[]>([]);
  const [rovers, setRovers] = useState<Rover[]>([]);
  const [enrollmentCodes, setEnrollmentCodes] = useState<EnrollmentCode[]>([]);
  const [signals, setSignals] = useState<Signal[]>([]);
  const [newEnrollmentCode, setNewEnrollmentCode] = useState<string | null>(null);
  const [newEnrollmentCodeId, setNewEnrollmentCodeId] = useState<string | null>(null);
  const [members, setMembers] = useState<Member[]>([]);
  const [fleetInvites, setFleetInvites] = useState<Invitation[]>([]);
  const [myInvites, setMyInvites] = useState<MyInvite[]>([]);
  const [boardTick, setBoardTick] = useState(0);
  const myRole = members.find((m) => m.id === user.id)?.role ?? "member";

  const [selectedOperation, setSelectedOperation] = useState<string | null>(() =>
    typeof window === "undefined" ? null : parseAppPath(window.location.pathname).operationId,
  );
  const [operationDetail, setOperationDetail] = useState<OperationDetail | null>(null);
  const [selectedRun, setSelectedRun] = useState<string | null>(null);
  const [runDetail, setRunDetail] = useState<RunDetail | null>(null);
  const [selectedUserId, setSelectedUserId] = useState<string | null>(() =>
    typeof window === "undefined" ? null : parseAppPath(window.location.pathname).userId,
  );
  const [userProfile, setUserProfile] = useState<UserProfile | null>(null);

  const operationRef = useRef<string | null>(null); operationRef.current = selectedOperation;
  const operationBackStackRef = useRef<string[]>([]);
  const runRef = useRef<string | null>(null); runRef.current = selectedRun;
  const fleetRef = useRef(fleet); fleetRef.current = fleet;
  const bumpBoard = useCallback(() => setBoardTick((t) => t + 1), []);

  const loadSignals = useCallback(async (f: string) => { const d = await getJSON<Signal[]>(withFleet("/api/v1/signals", f)); if (d) setSignals(d); }, []);
  const loadRovers = useCallback(async (f: string) => { const d = await getJSON<Rover[]>(withFleet("/api/v1/rovers", f)); if (d) setRovers(d); }, []);
  const loadRoutines = useCallback(async (f: string) => { const d = await getJSON<Routine[]>(withFleet("/api/v1/routines", f)); if (d) setRoutines(d); }, []);
  const loadMissionCounts = useCallback(async (f: string) => {
    const d = await getJSON<{ by_mission?: Record<string, number> }>(withFleet("/api/v1/missions/stats?metrics=by_mission", f));
    if (d?.by_mission) setMissionCounts(d.by_mission);
  }, []);
  const loadMembers = useCallback(async (f: string) => {
    const [m, inv] = await Promise.all([getJSON<Member[]>(fleetPath(f, "/members")), getJSON<Invitation[]>(withFleet("/api/v1/invitations", f))]);
    if (m) setMembers(m);
    setFleetInvites(inv ?? []);
  }, []);
  const loadMyInvites = useCallback(async () => { const d = await getJSON<MyInvite[]>(`/api/v1/invitations/mine`); setMyInvites(d ?? []); }, []);
  const loadMeta = useCallback(async (f: string) => {
    const [m, a, c, rv, t, lb, rt] = await Promise.all([
      getJSON<Mission[]>(withFleet("/api/v1/missions", f)),
      getJSON<Pilot[]>(withFleet("/api/v1/pilots", f)),
      getJSON<Crew[]>(withFleet("/api/v1/crews", f)),
      getJSON<Rover[]>(withFleet("/api/v1/rovers", f)),
      getJSON<EnrollmentCode[]>("/api/v1/enrollment-codes"),
      getJSON<Label[]>(withFleet("/api/v1/labels", f)),
      getJSON<Routine[]>(withFleet("/api/v1/routines", f)),
    ]);
    if (m) setMissions(m);
    if (a) setPilots(a);
    if (c) setCrews(c);
    if (lb) setLabels(lb);
    if (rt) setRoutines(rt);
    if (rv) setRovers(rv);
    if (t) {
      setEnrollmentCodes(t);
      setNewEnrollmentCodeId((id) => {
        if (id != null && !t.some((it) => it.id === id)) {
          setNewEnrollmentCode(null);
          return null;
        }
        return id;
      });
    }
  }, []);
  const loadOperationDetail = useCallback(async (id: string) => { const d = await getJSON<OperationDetail>(`/api/v1/operations/${id}`); if (d) setOperationDetail(d); }, []);
  const loadRunDetail = useCallback(async (id: string) => { const d = await getJSON<RunDetail>(`/api/v1/runs/${id}`); if (d) setRunDetail(d); }, []);

  const resync = useCallback((f = fleetRef.current) => {
    loadMeta(f); loadSignals(f); loadMissionCounts(f); loadMembers(f); loadMyInvites(); bumpBoard();
    if (f === fleetRef.current) {
      if (operationRef.current != null) loadOperationDetail(operationRef.current);
      if (runRef.current != null) loadRunDetail(runRef.current);
    }
  }, [loadMeta, loadSignals, loadMissionCounts, loadMembers, loadMyInvites, loadOperationDetail, loadRunDetail, bumpBoard]);

  useEffect(() => {
    resync(fleet);
  }, [fleet, resync]);

  useEffect(() => {
    const ws = new WebSocketClient();
    ws.onReconnect(resync);
    ws.onEvent((event) => {
      const f = fleetRef.current;
      if (event.fleetId !== f) return;
      switch (event.type) {
        case "operation":
          bumpBoard(); loadMissionCounts(f); loadRoutines(f);
          if (operationRef.current != null) loadOperationDetail(operationRef.current);
          break;
        case "comment":
          if (operationRef.current != null) loadOperationDetail(operationRef.current);
          break;
        case "run":
          bumpBoard(); loadRovers(f);
          if (operationRef.current != null) loadOperationDetail(operationRef.current);
          if (runRef.current != null) loadRunDetail(runRef.current);
          break;
        case "run_message":
          if (runRef.current != null) loadRunDetail(runRef.current);
          break;
        case "signal":
          loadSignals(f);
          break;
        case "rover":
          loadMeta(f);
          break;
        case "routine":
          loadRoutines(f);
          break;
      }
    });
    ws.connect();
    return () => ws.close();
  }, [resync, loadSignals, loadMissionCounts, loadRovers, loadRoutines, loadMeta, loadOperationDetail, loadRunDetail, bumpBoard]);

  useEffect(() => { if (selectedOperation != null) loadOperationDetail(selectedOperation); else setOperationDetail(null); }, [fleet, selectedOperation, loadOperationDetail]);
  useEffect(() => { if (selectedRun != null) loadRunDetail(selectedRun); else setRunDetail(null); }, [fleet, selectedRun, loadRunDetail]);
  useEffect(() => {
    if (selectedUserId == null) {
      setUserProfile(null);
      return;
    }
    let canceled = false;
    (async () => {
      const profile = await getJSON<UserProfile>(`/api/v1/users/${selectedUserId}`);
      if (canceled) return;
      if (!profile) {
        toast.error("Profile not found");
        setSelectedUserId(null);
        return;
      }
      setUserProfile(profile);
    })();
    return () => { canceled = true; };
  }, [selectedUserId]);

  const switchFleet = useCallback((id: string) => {
    localStorage.setItem("ufo.fleet", id);
    operationBackStackRef.current = [];
    setSelectedOperation(null); setOperationDetail(null); setSelectedRun(null); setRunDetail(null);
    setSelectedUserId(null); setUserProfile(null); setNewEnrollmentCode(null); setNewEnrollmentCodeId(null);
    setFleet(id);
  }, []);
  const signOut = useCallback(async () => { await postJSON(`/api/v1/auth/logout`); window.location.href = "/login"; }, []);
  const openOperation = useCallback((id: string | null) => {
    if (id == null) operationBackStackRef.current = [];
    else if (operationRef.current && operationRef.current !== id) operationBackStackRef.current.push(operationRef.current);
    setSelectedUserId(null); setUserProfile(null);
    setSelectedOperation(id); setSelectedRun(null); setRunDetail(null);
  }, []);
  const backOperation = useCallback(() => {
    setSelectedOperation(operationBackStackRef.current.pop() ?? null);
    setSelectedRun(null); setRunDetail(null);
  }, []);
  const openUser = useCallback((id: string | null) => {
    setSelectedOperation(null); setOperationDetail(null); setSelectedRun(null); setRunDetail(null);
    operationBackStackRef.current = [];
    setSelectedUserId(id);
    if (id == null) setUserProfile(null);
  }, []);

  const fail = (res: Response, fallback: string) => res.json().then((d) => toast.error(d.error || fallback)).catch(() => toast.error(fallback));

  const createFleet: Ctx["createFleet"] = useCallback(async (name, context = "") => {
    const res = await postJSON(`/api/v1/fleets`, { name, metadata: contextMetadata(undefined, context) });
    if (!res.ok) { await fail(res, "Create fleet failed"); return; }
    const f = (await res.json()) as Fleet;
    setFleets((prev) => [...prev, f]);
    switchFleet(f.id);
    toast.success("Fleet created");
  }, [switchFleet]);

  const updateFleet: Ctx["updateFleet"] = useCallback(async (id, name) => {
    const res = await patchJSON(`/api/v1/fleets/${id}`, { name });
    if (!res.ok) { await fail(res, "Rename fleet failed"); return false; }
    const f = (await res.json()) as Fleet;
    setFleets((prev) => prev.map((it) => it.id === f.id ? f : it));
    toast.success("Fleet renamed");
    return true;
  }, []);
  const setFleetContext: Ctx["setFleetContext"] = useCallback(async (context) => {
    const current = fleets.find((it) => it.id === fleet);
    if (!current) return false;
    const res = await patchJSON(`/api/v1/fleets/${current.id}`, { metadata: contextMetadata(current.metadata, context) });
    if (!res.ok) { await fail(res, "Update fleet context failed"); return false; }
    const next = (await res.json()) as Fleet;
    setFleets((prev) => prev.map((it) => it.id === next.id ? next : it));
    toast.success("Fleet context saved");
    return true;
  }, [fleet, fleets]);
  const setFleetWorktree: Ctx["setFleetWorktree"] = useCallback(async (enabled) => {
    const current = fleets.find((it) => it.id === fleet);
    if (!current) return;
    const res = await patchJSON(`/api/v1/fleets/${current.id}`, { metadata: worktreeMetadata(current.metadata, enabled) });
    if (!res.ok) { await fail(res, "Worktree setting failed"); return; }
    const next = (await res.json()) as Fleet;
    setFleets((prev) => prev.map((it) => it.id === next.id ? next : it));
    loadMeta(fleet);
  }, [fleet, fleets, loadMeta]);
  const updateUserName: Ctx["updateUserName"] = useCallback(async (name) => {
    const res = await patchJSON("/api/v1/users/me", { name });
    if (!res.ok) { await fail(res, "Update name failed"); return false; }
    const next = (await res.json()) as User;
    setUser(next);
    setMembers((prev) => prev.map((m) => m.id === next.id ? { ...m, name: next.name } : m));
    return true;
  }, []);

  const createOperation: Ctx["createOperation"] = useCallback(async (input) => {
    const res = await postJSON(`/api/v1/operations`, { ...input, fleet_id: fleet });
    if (!res.ok) { await fail(res, "Create failed"); return null; }
    const op = (await res.json()) as Operation;
    bumpBoard(); loadMissionCounts(fleet);
    if (input.main_operation_id && operationRef.current === input.main_operation_id) loadOperationDetail(input.main_operation_id);
    toast.success("Operation created");
    return op;
  }, [fleet, bumpBoard, loadMissionCounts, loadOperationDetail]);

  const reassign: Ctx["reassign"] = useCallback(async (operationId, assignee_type, assignee_id) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { assignee_type, assignee_id });
    bumpBoard(); loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);

  const setOperationTags: Ctx["setOperationTags"] = useCallback(async (operationId, required_tags, excluded_tags) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { required_tags, excluded_tags });
    loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const setOperationWorktree: Ctx["setOperationWorktree"] = useCallback(async (operationId, enabled) => {
    const res = await patchJSON(`/api/v1/operations/${operationId}`, { worktree_enabled: enabled });
    if (!res.ok) { await fail(res, "Worktree setting failed"); return; }
    bumpBoard();
    loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);

  const updateOperation: Ctx["updateOperation"] = useCallback(async (operationId, input) => {
    const res = await patchJSON(`/api/v1/operations/${operationId}`, input);
    if (!res.ok) { await fail(res, "Update operation failed"); return false; }
    bumpBoard();
    loadOperationDetail(operationId);
    return true;
  }, [fleet, bumpBoard, loadOperationDetail]);

  const setOperationMission: Ctx["setOperationMission"] = useCallback(async (operationId, missionId) => {
    const res = await patchJSON(`/api/v1/operations/${operationId}`, { mission_id: missionId });
    if (!res.ok) { await fail(res, "Change mission failed"); return false; }
    bumpBoard();
    loadOperationDetail(operationId);
    return true;
  }, [fleet, bumpBoard, loadOperationDetail]);

  const setPriority: Ctx["setPriority"] = useCallback(async (operationId, priority) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { priority });
    bumpBoard(); loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);
  const setDates: Ctx["setDates"] = useCallback(async (operationId, start_date, due_date) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { start_date, due_date });
    bumpBoard();
    loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);
  const setMainOperation: Ctx["setMainOperation"] = useCallback(async (operationId, main_operation_id) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { main_operation_id });
    bumpBoard(); loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);
  const setArchived: Ctx["setArchived"] = useCallback(async (operationId, archived) => {
    await patchJSON(`/api/v1/operations/${operationId}`, { archived });
    bumpBoard();
    if (archived && operationRef.current === operationId) { openOperation(null); return; }
    loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail, openOperation]);
  const createLabel: Ctx["createLabel"] = useCallback(async (name, color) => {
    const res = await postJSON(`/api/v1/labels`, { name, color, fleet_id: fleet });
    if (!res.ok) { await fail(res, "Create label failed"); return null; }
    const l = (await res.json()) as Label;
    setLabels((prev) => [...prev, l].sort((a, b) => a.name.localeCompare(b.name)));
    return l;
  }, [fleet]);
  const deleteLabel: Ctx["deleteLabel"] = useCallback(async (id) => {
    await del(`/api/v1/labels/${id}`); loadMeta(fleet); bumpBoard();
  }, [fleet, loadMeta, bumpBoard]);
  const updateLabel: Ctx["updateLabel"] = useCallback(async (id, name, color = "") => {
    const res = await patchJSON(`/api/v1/labels/${id}`, { name, color });
    if (!res.ok) { await fail(res, "Rename label failed"); return null; }
    const label = (await res.json()) as Label;
    loadMeta(fleet); bumpBoard();
    if (operationRef.current) loadOperationDetail(operationRef.current);
    return label;
  }, [fleet, loadMeta, bumpBoard, loadOperationDetail]);
  const createRoutine: Ctx["createRoutine"] = useCallback(async (input) => {
    const res = await postJSON(`/api/v1/routines`, { fleet_id: fleet, ...input });
    if (!res.ok) { await fail(res, "Create routine failed"); return null; }
    const routine = (await res.json()) as Routine;
    setRoutines((prev) => [routine, ...prev]);
    toast.success("Routine saved");
    return routine;
  }, [fleet]);
  const updateRoutine: Ctx["updateRoutine"] = useCallback(async (id, input) => {
    const res = await patchJSON(`/api/v1/routines/${id}`, input);
    if (!res.ok) { await fail(res, "Update routine failed"); return null; }
    const routine = (await res.json()) as Routine;
    setRoutines((prev) => prev.map((it) => it.id === routine.id ? routine : it));
    toast.success("Routine saved");
    return routine;
  }, []);
  const deleteRoutine: Ctx["deleteRoutine"] = useCallback(async (id) => {
    const res = await del(`/api/v1/routines/${id}`);
    if (!res.ok) { await fail(res, "Delete routine failed"); return; }
    setRoutines((prev) => prev.filter((it) => it.id !== id));
  }, [fleet]);
  const pulseRoutine: Ctx["pulseRoutine"] = useCallback(async (id) => {
    const res = await postJSON(`/api/v1/pulses`, { routine_id: id });
    if (!res.ok) { await fail(res, "Pulse routine failed"); return null; }
    const pulse = (await res.json()) as Pulse;
    bumpBoard(); loadMissionCounts(fleet); loadRoutines(fleet); toast.success("Pulse sent");
    return pulse;
  }, [fleet, bumpBoard, loadMissionCounts, loadRoutines]);
  const attachLabel: Ctx["attachLabel"] = useCallback(async (operationId, labelId) => {
    await putJSON(`/api/v1/operations/${operationId}/labels/${labelId}`);
    bumpBoard(); loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);
  const detachLabel: Ctx["detachLabel"] = useCallback(async (operationId, labelId) => {
    await del(`/api/v1/operations/${operationId}/labels/${labelId}`);
    bumpBoard(); loadOperationDetail(operationId);
  }, [fleet, bumpBoard, loadOperationDetail]);
  const addRelation: Ctx["addRelation"] = useCallback(async (operationId, kind, target) => {
    await postJSON(`/api/v1/relations`, { operation_id: operationId, kind, target }); loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const removeRelation: Ctx["removeRelation"] = useCallback(async (relationId, operationId) => {
    await del(`/api/v1/relations/${relationId}`); loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const createSourceAction: Ctx["createSourceAction"] = useCallback(async (operationId, kind) => {
    const res = await postJSON(`/api/v1/source-actions`, { operation_id: operationId, kind });
    if (!res.ok) { await fail(res, kind === "create_source_branch" ? "Source branch request failed" : kind === "refresh_from_source" ? "Refresh from source request failed" : "Apply to source request failed"); return; }
    loadOperationDetail(operationId);
    toast.success(kind === "create_source_branch" ? "Source branch queued" : kind === "refresh_from_source" ? "Refresh from source queued" : "Apply to source queued");
  }, [fleet, loadOperationDetail]);
  const addPullRequest: Ctx["addPullRequest"] = useCallback(async (operationId, url, title) => {
    await postJSON(`/api/v1/pull-requests`, { operation_id: operationId, url, title });
    loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const deletePullRequest: Ctx["deletePullRequest"] = useCallback(async (pullRequestId, operationId) => {
    await del(`/api/v1/pull-requests/${pullRequestId}`); loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const uploadAsset: Ctx["uploadAsset"] = useCallback(async (file, options) => {
    const intentRes = await postJSON(`/api/v1/assets`, {
      fleet_id: fleet,
      context: options?.operationId ? { operation_id: options.operationId } : undefined,
      filename: file.name,
      content_type: file.type || "application/octet-stream",
      byte_size: file.size,
    });
    if (!intentRes.ok) { await fail(intentRes, "Upload failed"); return null; }
    const intent = (await intentRes.json()) as AssetUploadIntent;
    const uploadRes = await putRaw(intent.url, file, intent.headers);
    if (!uploadRes.ok) { toast.error("Upload failed"); return null; }
    const completeRes = await patchJSON(`/api/v1/assets/${intent.asset_id}`, { status: "ready" });
    if (!completeRes.ok) { await fail(completeRes, "Upload verification failed"); return null; }
    toast.success("File uploaded");
    return (await completeRes.json()) as Asset;
  }, [fleet]);
  const searchOperations: Ctx["searchOperations"] = useCallback(async (q) => {
    const toRef = (op: Operation): OperationReference => ({
      id: op.id, title: op.title, status: op.status, sequence: op.sequence, mission_id: op.mission_id,
    });
    const rows = ((await getJSON<Operation[]>(withFleet(`/api/v1/operations?q=${encodeURIComponent(q)}`, fleet))) ?? []).map(toRef);
    if (rows.length > 0) return rows;
    const code = operationCodeSearch(q);
    if (!code) return rows;
    const mission = missions.find((m) => m.key.toUpperCase() === code.key);
    const fallback = ((await getJSON<Operation[]>(withFleet(`/api/v1/operations?q=${code.sequence}`, fleet))) ?? []).map(toRef);
    return fallback.filter((op) => op.sequence === code.sequence && (!mission || op.mission_id === mission.id));
  }, [fleet, missions]);
  const react: Ctx["react"] = useCallback(async (kind, id, emoji, operationId, on = true) => {
    const path = `/api/v1/${kind}/${id}/reactions/${encodeURIComponent(emoji)}`;
    await (on ? putJSON(path) : del(path));
    loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);

  const cancelRun: Ctx["cancelRun"] = useCallback(async (runId, operationId) => {
    const res = await patchJSON(`/api/v1/runs/${runId}`, { status: "canceled" });
    if (!res.ok) { await fail(res, "Stop failed"); return; }
    bumpBoard(); loadOperationDetail(operationId); toast.success("Run stopped");
  }, [fleet, bumpBoard, loadOperationDetail]);

  // The board applies the move optimistically; here we persist then bump to reconcile.
  const moveOperation: Ctx["moveOperation"] = useCallback(async (operationId, status) => {
    const res = await patchJSON(`/api/v1/operations/${operationId}`, { status });
    if (!res.ok) await fail(res, "Move failed");
    bumpBoard(); loadSignals(fleet);
  }, [fleet, bumpBoard, loadSignals]);

  const addComment: Ctx["addComment"] = useCallback(async (operationId, body) => {
    await postJSON(`/api/v1/comments`, { operation_id: operationId, body });
    loadOperationDetail(operationId);
  }, [fleet, loadOperationDetail]);
  const updateComment: Ctx["updateComment"] = useCallback(async (operationId, commentId, body) => {
    const res = await patchJSON(`/api/v1/comments/${commentId}`, { body });
    loadOperationDetail(operationId);
    if (!res.ok) { await fail(res, "Update comment failed"); return false; }
    return true;
  }, [fleet, loadOperationDetail]);
  const deleteComment: Ctx["deleteComment"] = useCallback(async (operationId, commentId) => {
    const res = await del(`/api/v1/comments/${commentId}`);
    loadOperationDetail(operationId);
    if (!res.ok) { await fail(res, "Delete comment failed"); return false; }
    return true;
  }, [fleet, loadOperationDetail]);

  const addCrew: Ctx["addCrew"] = useCallback(async (name) => { await postJSON(`/api/v1/crews`, { name, fleet_id: fleet }); loadMeta(fleet); }, [fleet, loadMeta]);
  const renameCrew: Ctx["renameCrew"] = useCallback(async (id, name) => {
    const res = await patchJSON(`/api/v1/crews/${id}`, { name });
    if (!res.ok) { await fail(res, "Rename crew failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const delCrew: Ctx["delCrew"] = useCallback(async (id) => { await del(`/api/v1/crews/${id}`); loadMeta(fleet); }, [fleet, loadMeta]);
  const addMission: Ctx["addMission"] = useCallback(async (name, key, context = "") => {
    const res = await postJSON(`/api/v1/missions`, { name, key, fleet_id: fleet, metadata: contextMetadata(undefined, context) });
    if (!res.ok) { await fail(res, "Add mission failed"); return false; }
    loadMeta(fleet);
    return true;
  }, [fleet, loadMeta]);
  const updateMission: Ctx["updateMission"] = useCallback(async (id, name, key, context) => {
    const mission = missions.find((it) => it.id === id);
    const body: Record<string, unknown> = { name, key };
    if (context !== undefined) {
      if (!mission) return false;
      body.metadata = contextMetadata(mission.metadata, context);
    }
    const res = await patchJSON(`/api/v1/missions/${id}`, body);
    if (!res.ok) { await fail(res, "Update mission failed"); return false; }
    loadMeta(fleet); bumpBoard();
    return true;
  }, [fleet, missions, loadMeta, bumpBoard]);
  const setMissionWorktree: Ctx["setMissionWorktree"] = useCallback(async (id, enabled) => {
    const mission = missions.find((it) => it.id === id);
    if (!mission) return;
    const res = await patchJSON(`/api/v1/missions/${id}`, {
      name: mission.name,
      key: mission.key,
      metadata: worktreeMetadata(mission.metadata, enabled),
    });
    if (!res.ok) { await fail(res, "Worktree setting failed"); return; }
    loadMeta(fleet);
    bumpBoard();
  }, [fleet, missions, loadMeta, bumpBoard]);
  const addMember: Ctx["addMember"] = useCallback(async (crewId, value, role, userId) => {
    const [member_type, id] = value === "me" ? ["user", userId] : value.split(":");
    await putJSON(`/api/v1/crews/${crewId}/members/${member_type}/${id}`, { role });
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const removeMember: Ctx["removeMember"] = useCallback(async (crewId, member_type, member_id) => {
    await del(`/api/v1/crews/${crewId}/members/${member_type}/${member_id}`);
    loadMeta(fleet);
  }, [fleet, loadMeta]);

  const createEnrollmentCode: Ctx["createEnrollmentCode"] = useCallback(async ({ name = "", expiresAt = "", uses }) => {
    const body: Record<string, unknown> = { fleet_id: fleet };
    if (expiresAt) body.expires_at = expiresAt;
    if (uses && uses > 1) {
      body.uses = uses;
      body.name = name;
    }
    const res = await postJSON(`/api/v1/enrollment-codes`, body);
    if (res.ok) {
      const code = (await res.json()) as EnrollmentCode;
      setNewEnrollmentCode(code.code);
      setNewEnrollmentCodeId(code.id);
      loadMeta(fleet);
    }
    else await fail(res, "Enrollment code failed");
  }, [fleet, loadMeta]);
  const revokeRover: Ctx["revokeRover"] = useCallback(async (id) => { await del(`/api/v1/rovers/${id}`); loadMeta(fleet); }, [fleet, loadMeta]);
  const renameRover: Ctx["renameRover"] = useCallback(async (id, name) => {
    const res = await patchJSON(`/api/v1/rovers/${id}`, { name });
    if (!res.ok) { await fail(res, "Rename rover failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const setRoverTags: Ctx["setRoverTags"] = useCallback(async (id, tags) => {
    const res = await patchJSON(`/api/v1/rovers/${id}`, { tags });
    if (!res.ok) { await fail(res, "Tag update failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const setRoverUnits: Ctx["setRoverUnits"] = useCallback(async (id, units) => {
    const res = await patchJSON(`/api/v1/rovers/${id}`, { units });
    if (!res.ok) { await fail(res, "Units update failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const revokeEnrollmentCode: Ctx["revokeEnrollmentCode"] = useCallback(async (id) => { await del(`/api/v1/enrollment-codes/${id}`); loadMeta(fleet); }, [fleet, loadMeta]);
  const savePendingRover: Ctx["savePendingRover"] = useCallback(async (code, { name = "", units, tags }) => {
    const body: Record<string, unknown> = { code, pending: true };
    if (name.trim()) body.name = name.trim();
    if (units != null) body.units = units;
    if (tags != null) body.tags = tags;
    const res = await postJSON(`/api/v1/enrollment-codes`, body);
    if (!res.ok) { await fail(res, "Save pending rover failed"); return null; }
    const enrollment = (await res.json()) as EnrollmentCode;
    setEnrollmentCodes((prev) => [enrollment, ...prev.filter((it) => it.id !== enrollment.id)]);
    loadMeta(fleet);
    return enrollment;
  }, [fleet, loadMeta]);
  const approvePendingRover: Ctx["approvePendingRover"] = useCallback(async (id, { fleetId, name = "", units, tags }) => {
    const targetFleet = fleetId || fleet;
    if (!targetFleet) { toast.error("Choose a fleet before approving"); return false; }
    const body: Record<string, unknown> = { fleet_id: targetFleet, kind: "web:approved", name: name.trim() };
    if (units != null) body.units = units;
    if (tags != null) body.tags = tags;
    const res = await patchJSON(`/api/v1/enrollment-codes/${id}`, body);
    if (!res.ok) { await fail(res, "Approve failed"); if (res.status === 410) loadMeta(fleet); return false; }
    toast.success("Rover approved");
    loadMeta(fleet);
    return true;
  }, [fleet, loadMeta]);
  const denyPendingRover: Ctx["denyPendingRover"] = useCallback(async (id) => {
    const res = await patchJSON(`/api/v1/enrollment-codes/${id}`, { kind: "web:denied" });
    if (!res.ok) { await fail(res, "Deny failed"); if (res.status === 410) loadMeta(fleet); return false; }
    toast.success("Enrollment denied");
    loadMeta(fleet);
    return true;
  }, [fleet, loadMeta]);
  const openSignal: Ctx["openSignal"] = useCallback(async (it) => {
    if (!it.read) { await patchJSON(`/api/v1/signals/${it.id}`, { read: true }); loadSignals(fleet); }
    if (it.operation_id != null) openOperation(it.operation_id);
  }, [fleet, loadSignals, openOperation]);
  const archiveSignal: Ctx["archiveSignal"] = useCallback(async (id) => { await patchJSON(`/api/v1/signals/${id}`, { archived: true }); loadSignals(fleet); }, [fleet, loadSignals]);

  const invite: Ctx["invite"] = useCallback(async (email, role) => {
    const res = await postJSON(`/api/v1/invitations`, { email, role, fleet_id: fleet });
    if (!res.ok) { await fail(res, "Invite failed"); return false; }
    loadMembers(fleet); toast.success("Invitation sent");
    return true;
  }, [fleet, loadMembers]);
  const revokeInvite: Ctx["revokeInvite"] = useCallback(async (id) => { await del(`/api/v1/invitations/${id}`); loadMembers(fleet); }, [fleet, loadMembers]);
  const acceptInvite: Ctx["acceptInvite"] = useCallback(async (id, fleetId) => {
    const res = await patchJSON(`/api/v1/invitations/${id}`, { status: "accepted" });
    if (!res.ok) { await fail(res, "Accept failed"); return; }
    localStorage.setItem("ufo.fleet", fleetId);
    window.location.reload();
  }, []);
  const declineInvite: Ctx["declineInvite"] = useCallback(async (id) => {
    await patchJSON(`/api/v1/invitations/${id}`, { status: "declined" });
    loadMyInvites();
  }, [loadMyInvites]);
  const setMemberRole: Ctx["setMemberRole"] = useCallback(async (userId, role) => {
    const res = await patchJSON(`/api/v1/fleets/${fleet}/members/${userId}`, { role });
    if (!res.ok) { await fail(res, "Role change failed"); return; }
    loadMembers(fleet);
  }, [fleet, loadMembers]);
  const removeFleetMember: Ctx["removeFleetMember"] = useCallback(async (userId) => {
    const res = await del(`/api/v1/fleets/${fleet}/members/${userId}`);
    if (!res.ok) { await fail(res, "Remove failed"); return; }
    loadMembers(fleet);
  }, [fleet, loadMembers]);

  const value: Ctx = useMemo(() => ({
    user, updateUserName, fleets, fleet, switchFleet, createFleet, updateFleet, setFleetContext, setFleetWorktree, signOut,
    missions, missionCounts, pilots, crews, labels, routines, rovers, enrollmentCodes, signals, newEnrollmentCode, boardTick,
    members, myRole, fleetInvites, myInvites,
    selectedOperation, openOperation, backOperation, operationDetail, selectedRun, setSelectedRun, runDetail,
    selectedUserId, userProfile, openUser,
    createOperation, setOperationTags, setOperationWorktree, updateOperation, setOperationMission, setPriority, setDates, setMainOperation, setArchived,
    createLabel, updateLabel, deleteLabel, createRoutine, updateRoutine, deleteRoutine, pulseRoutine, attachLabel, detachLabel, addRelation, removeRelation, createSourceAction, addPullRequest, deletePullRequest, uploadAsset, searchOperations, react,
    reassign, cancelRun, moveOperation, addComment, updateComment, deleteComment,
    addCrew, renameCrew, delCrew, addMission, updateMission, setMissionWorktree, addMember, removeMember,
    createEnrollmentCode, revokeRover, renameRover, setRoverTags, setRoverUnits, revokeEnrollmentCode, savePendingRover, approvePendingRover, denyPendingRover, openSignal, archiveSignal,
    invite, revokeInvite, acceptInvite, declineInvite, setMemberRole, removeFleetMember,
  }), [
    user, fleets, fleet, missions, missionCounts, pilots, crews, labels, routines, rovers, enrollmentCodes, signals, newEnrollmentCode, boardTick,
    members, myRole, fleetInvites, myInvites,
    selectedOperation, operationDetail, selectedRun, runDetail, selectedUserId, userProfile,
    updateUserName, switchFleet, createFleet, updateFleet, setFleetContext, setFleetWorktree, signOut,
    openOperation, backOperation, setSelectedRun, openUser,
    createOperation, setOperationTags, setOperationWorktree, updateOperation, setOperationMission, setPriority, setDates, setMainOperation, setArchived,
    createLabel, updateLabel, deleteLabel, createRoutine, updateRoutine, deleteRoutine, pulseRoutine, attachLabel, detachLabel, addRelation, removeRelation, createSourceAction, addPullRequest, deletePullRequest, uploadAsset, searchOperations, react,
    reassign, cancelRun, moveOperation, addComment, updateComment, deleteComment,
    addCrew, renameCrew, delCrew, addMission, updateMission, setMissionWorktree, addMember, removeMember,
    createEnrollmentCode, revokeRover, renameRover, setRoverTags, setRoverUnits, revokeEnrollmentCode, savePendingRover, approvePendingRover, denyPendingRover, openSignal, archiveSignal,
    invite, revokeInvite, acceptInvite, declineInvite, setMemberRole, removeFleetMember,
  ]);
  return <AppCtx.Provider value={value}>{children}</AppCtx.Provider>;
}
