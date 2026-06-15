"use client";

import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { del, getJSON, postJSON } from "@/lib/api";
import { WSClient } from "@/lib/ws";
import type {
  EnrollmentCode, Pilot, Comment, Crew, Fleet, Invitation, Label, Member, MyInvite, OpRef, Signal, Mission, Operation,
  OperationDetail, Rover, Run, RunDetail, User,
} from "@/lib/types";

type Ctx = {
  user: User;
  fleets: Fleet[];
  fleet: string;
  switchFleet: (id: string) => void;
  createFleet: (name: string) => Promise<void>;
  signOut: () => void;
  // data (board fetches its own pages; provider holds the small lists)
  missions: Mission[];
  missionCounts: Record<string, number>;
  pilots: Pilot[];
  crews: Crew[];
  labels: Label[];
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
  selectedOp: string | null;
  openOp: (id: string | null) => void;
  opDetail: OperationDetail | null;
  selectedRun: string | null;
  setSelectedRun: (id: string | null) => void;
  runDetail: RunDetail | null;
  // actions
  createOperation: (i: { title: string; body: string; mission_id: string | null; assignee_type: string | null; assignee_id: string | null; required_tags?: string[]; excluded_tags?: string[]; priority?: number; parent_id?: string | null; start_date?: string | null; due_date?: string | null }) => Promise<Operation | null>;
  setOperationTags: (opId: string, required_tags: string[], excluded_tags: string[]) => Promise<void>;
  setPriority: (opId: string, priority: number) => Promise<void>;
  setDates: (opId: string, start_date: string | null, due_date: string | null) => Promise<void>;
  setParent: (opId: string, parent_id: string | null) => Promise<void>;
  setArchived: (opId: string, archived: boolean) => Promise<void>;
  createLabel: (name: string, color: string) => Promise<Label | null>;
  deleteLabel: (id: string) => Promise<void>;
  attachLabel: (opId: string, labelId: string) => Promise<void>;
  detachLabel: (opId: string, labelId: string) => Promise<void>;
  addPR: (opId: string, url: string, title: string) => Promise<void>;
  deletePR: (prId: string, opId: string) => Promise<void>;
  addRelation: (opId: string, kind: string, target: string) => Promise<void>;
  removeRelation: (relId: string, opId: string) => Promise<void>;
  searchOps: (q: string) => Promise<OpRef[]>;
  react: (kind: "comments" | "operations", id: string, emoji: string, opId: string) => Promise<void>;
  reassign: (opId: string, assignee_type: string | null, assignee_id: string | null) => Promise<void>;
  runOp: (opId: string) => Promise<void>;
  moveOp: (opId: string, status: string) => Promise<void>;
  addComment: (opId: string, body: string) => Promise<void>;
  addPilot: (name: string, kind: string) => Promise<void>;
  delPilot: (id: string) => Promise<void>;
  addCrew: (name: string) => Promise<void>;
  delCrew: (id: string) => Promise<void>;
  addMission: (name: string, key: string) => Promise<boolean>;
  updateMission: (id: string, name: string, key: string) => Promise<boolean>;
  addMember: (crewId: string, value: string, role: string, userId: string) => Promise<void>;
  removeMember: (crewId: string, member_type: string, member_id: string) => Promise<void>;
  addRover: () => Promise<void>;
  createReusableEnrollmentCode: (expiresAt: string) => Promise<void>;
  revokeRover: (id: string) => Promise<void>;
  setRoverTags: (id: string, tags: string[]) => Promise<void>;
  revokeEnrollmentCode: (id: string) => Promise<void>;
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

export function AppProvider({ user, fleets: initialFleets, initialFleet, children }: { user: User; fleets: Fleet[]; initialFleet: string; children: React.ReactNode }) {
  const [fleets, setFleets] = useState<Fleet[]>(initialFleets);
  const [fleet, setFleet] = useState(initialFleet);
  const [missions, setMissions] = useState<Mission[]>([]);
  const [missionCounts, setMissionCounts] = useState<Record<string, number>>({});
  const [pilots, setPilots] = useState<Pilot[]>([]);
  const [crews, setCrews] = useState<Crew[]>([]);
  const [labels, setLabels] = useState<Label[]>([]);
  const [rovers, setRovers] = useState<Rover[]>([]);
  const [enrollmentCodes, setEnrollmentCodes] = useState<EnrollmentCode[]>([]);
  const [signals, setSignals] = useState<Signal[]>([]);
  const [newEnrollmentCode, setNewEnrollmentCode] = useState<string | null>(null);
  const [members, setMembers] = useState<Member[]>([]);
  const [fleetInvites, setFleetInvites] = useState<Invitation[]>([]);
  const [myInvites, setMyInvites] = useState<MyInvite[]>([]);
  const [boardTick, setBoardTick] = useState(0);
  const myRole = members.find((m) => m.id === user.id)?.role ?? "member";

  const [selectedOp, setSelectedOp] = useState<string | null>(null);
  const [opDetail, setOpDetail] = useState<OperationDetail | null>(null);
  const [selectedRun, setSelectedRun] = useState<string | null>(null);
  const [runDetail, setRunDetail] = useState<RunDetail | null>(null);

  const opRef = useRef<string | null>(null); opRef.current = selectedOp;
  const runRef = useRef<string | null>(null); runRef.current = selectedRun;
  const bumpBoard = useCallback(() => setBoardTick((t) => t + 1), []);

  const loadSignals = useCallback(async (f: string) => { const d = await getJSON<Signal[]>(`/api/signals?fleet=${f}`); if (d) setSignals(d); }, []);
  const loadRovers = useCallback(async (f: string) => { const d = await getJSON<Rover[]>(`/api/rovers?fleet=${f}`); if (d) setRovers(d); }, []);
  const loadMissionCounts = useCallback(async (f: string) => { const d = await getJSON<Record<string, number>>(`/api/missions/counts?fleet=${f}`); if (d) setMissionCounts(d); }, []);
  const loadMembers = useCallback(async (f: string) => {
    const [m, inv] = await Promise.all([getJSON<Member[]>(`/api/members?fleet=${f}`), getJSON<Invitation[]>(`/api/invitations?fleet=${f}`)]);
    if (m) setMembers(m);
    setFleetInvites(inv ?? []); // null when not a manager
  }, []);
  const loadMyInvites = useCallback(async () => { const d = await getJSON<MyInvite[]>(`/api/invitations/mine`); setMyInvites(d ?? []); }, []);
  const loadMeta = useCallback(async (f: string) => {
    const [m, a, c, rv, t, lb] = await Promise.all([
      getJSON<Mission[]>(`/api/missions?fleet=${f}`),
      getJSON<Pilot[]>(`/api/pilots?fleet=${f}`),
      getJSON<Crew[]>(`/api/crews?fleet=${f}`),
      getJSON<Rover[]>(`/api/rovers?fleet=${f}`),
      getJSON<EnrollmentCode[]>(`/api/enrollment-codes?fleet=${f}`),
      getJSON<Label[]>(`/api/labels?fleet=${f}`),
    ]);
    if (m) setMissions(m);
    if (a) setPilots(a);
    if (c) setCrews(c);
    if (lb) setLabels(lb);
    if (rv) setRovers(rv);
    if (t) setEnrollmentCodes(t);
  }, []);
  const loadOpDetail = useCallback(async (f: string, id: string) => { const d = await getJSON<OperationDetail>(`/api/operations/${id}?fleet=${f}`); if (d) setOpDetail(d); }, []);
  const loadRunDetail = useCallback(async (f: string, id: string) => { const d = await getJSON<RunDetail>(`/api/runs/${id}?fleet=${f}`); if (d) setRunDetail(d); }, []);

  useEffect(() => {
    const resync = () => {
      loadMeta(fleet); loadSignals(fleet); loadMissionCounts(fleet); loadMembers(fleet); loadMyInvites(); bumpBoard();
      if (opRef.current != null) loadOpDetail(fleet, opRef.current);
      if (runRef.current != null) loadRunDetail(fleet, runRef.current);
    };
    resync();
    const ws = new WSClient(fleet);
    ws.onReconnect(resync);
    ws.onEvent((type) => {
      switch (type) {
        case "operation":
          bumpBoard(); loadMissionCounts(fleet);
          if (opRef.current != null) loadOpDetail(fleet, opRef.current);
          break;
        case "comment":
          if (opRef.current != null) loadOpDetail(fleet, opRef.current);
          break;
        case "run":
          bumpBoard();
          if (opRef.current != null) loadOpDetail(fleet, opRef.current);
          if (runRef.current != null) loadRunDetail(fleet, runRef.current);
          break;
        case "run_message":
          if (runRef.current != null) loadRunDetail(fleet, runRef.current);
          break;
        case "signal":
          loadSignals(fleet);
          break;
        case "rover":
          loadRovers(fleet);
          break;
      }
    });
    ws.connect();
    return () => ws.close();
  }, [fleet, loadMeta, loadSignals, loadMissionCounts, loadRovers, loadMembers, loadMyInvites, loadOpDetail, loadRunDetail, bumpBoard]);

  useEffect(() => { if (selectedOp != null) loadOpDetail(fleet, selectedOp); else setOpDetail(null); }, [fleet, selectedOp, loadOpDetail]);
  useEffect(() => { if (selectedRun != null) loadRunDetail(fleet, selectedRun); else setRunDetail(null); }, [fleet, selectedRun, loadRunDetail]);

  const switchFleet = useCallback((id: string) => {
    localStorage.setItem("ufo.fleet", id);
    setSelectedOp(null); setOpDetail(null); setSelectedRun(null); setRunDetail(null); setNewEnrollmentCode(null);
    setFleet(id);
  }, []);
  const signOut = useCallback(async () => { await postJSON(`/api/auth/logout`); window.location.href = "/login"; }, []);
  const openOp = useCallback((id: string | null) => { setSelectedOp(id); setSelectedRun(null); setRunDetail(null); }, []);

  const fail = (res: Response, fallback: string) => res.json().then((d) => toast.error(d.error || fallback)).catch(() => toast.error(fallback));

  const createFleet: Ctx["createFleet"] = useCallback(async (name) => {
    const res = await postJSON(`/api/fleets`, { name });
    if (!res.ok) { await fail(res, "Create fleet failed"); return; }
    const f = (await res.json()) as Fleet;
    setFleets((prev) => [...prev, f]);
    switchFleet(f.id);
    toast.success("Fleet created");
  }, [switchFleet]);

  const createOperation: Ctx["createOperation"] = useCallback(async (input) => {
    const res = await postJSON(`/api/operations?fleet=${fleet}`, input);
    if (!res.ok) { await fail(res, "Create failed"); return null; }
    const op = (await res.json()) as Operation;
    bumpBoard(); loadMissionCounts(fleet);
    toast.success("Operation created");
    return op;
  }, [fleet, bumpBoard, loadMissionCounts]);

  const reassign: Ctx["reassign"] = useCallback(async (opId, assignee_type, assignee_id) => {
    await postJSON(`/api/operations/${opId}/assign?fleet=${fleet}`, { assignee_type, assignee_id });
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);

  const setOperationTags: Ctx["setOperationTags"] = useCallback(async (opId, required_tags, excluded_tags) => {
    await postJSON(`/api/operations/${opId}/tags?fleet=${fleet}`, { required_tags, excluded_tags });
    loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);

  const setPriority: Ctx["setPriority"] = useCallback(async (opId, priority) => {
    await postJSON(`/api/operations/${opId}/priority?fleet=${fleet}`, { priority });
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);
  const setDates: Ctx["setDates"] = useCallback(async (opId, start_date, due_date) => {
    await postJSON(`/api/operations/${opId}/dates?fleet=${fleet}`, { start_date, due_date });
    loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);
  const setParent: Ctx["setParent"] = useCallback(async (opId, parent_id) => {
    await postJSON(`/api/operations/${opId}/parent?fleet=${fleet}`, { parent_id });
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);
  const setArchived: Ctx["setArchived"] = useCallback(async (opId, archived) => {
    await postJSON(`/api/operations/${opId}/archive?fleet=${fleet}`, { archived });
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);
  const createLabel: Ctx["createLabel"] = useCallback(async (name, color) => {
    const res = await postJSON(`/api/labels?fleet=${fleet}`, { name, color });
    if (!res.ok) { await fail(res, "Create label failed"); return null; }
    const l = (await res.json()) as Label;
    setLabels((prev) => [...prev, l].sort((a, b) => a.name.localeCompare(b.name)));
    return l;
  }, [fleet]);
  const deleteLabel: Ctx["deleteLabel"] = useCallback(async (id) => {
    await del(`/api/labels/${id}?fleet=${fleet}`); loadMeta(fleet); bumpBoard();
  }, [fleet, loadMeta, bumpBoard]);
  const attachLabel: Ctx["attachLabel"] = useCallback(async (opId, labelId) => {
    await postJSON(`/api/operations/${opId}/labels?fleet=${fleet}`, { label: labelId });
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);
  const detachLabel: Ctx["detachLabel"] = useCallback(async (opId, labelId) => {
    await del(`/api/operations/${opId}/labels?fleet=${fleet}&label=${labelId}`);
    bumpBoard(); loadOpDetail(fleet, opId);
  }, [fleet, bumpBoard, loadOpDetail]);
  const addPR: Ctx["addPR"] = useCallback(async (opId, url, title) => {
    await postJSON(`/api/operations/${opId}/prs?fleet=${fleet}`, { url, title });
    loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);
  const deletePR: Ctx["deletePR"] = useCallback(async (prId, opId) => {
    await del(`/api/prs/${prId}?fleet=${fleet}`); loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);
  const addRelation: Ctx["addRelation"] = useCallback(async (opId, kind, target) => {
    await postJSON(`/api/operations/${opId}/relations?fleet=${fleet}`, { kind, target }); loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);
  const removeRelation: Ctx["removeRelation"] = useCallback(async (relId, opId) => {
    await del(`/api/relations/${relId}?fleet=${fleet}`); loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);
  const searchOps: Ctx["searchOps"] = useCallback(async (q) => {
    return (await getJSON<OpRef[]>(`/api/operations/search?fleet=${fleet}&q=${encodeURIComponent(q)}`)) ?? [];
  }, [fleet]);
  const react: Ctx["react"] = useCallback(async (kind, id, emoji, opId) => {
    await postJSON(`/api/${kind}/${id}/reactions?fleet=${fleet}`, { emoji });
    loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);

  const runOp: Ctx["runOp"] = useCallback(async (opId) => {
    const res = await postJSON(`/api/operations/${opId}/run?fleet=${fleet}`);
    if (!res.ok) { await fail(res, "Run failed"); return; }
    bumpBoard(); loadOpDetail(fleet, opId); toast.success("Run dispatched");
  }, [fleet, bumpBoard, loadOpDetail]);

  // The board applies the move optimistically; here we persist then bump to reconcile.
  const moveOp: Ctx["moveOp"] = useCallback(async (opId, status) => {
    const res = await postJSON(`/api/operations/${opId}/status?fleet=${fleet}`, { status });
    if (!res.ok) await fail(res, "Move failed");
    bumpBoard(); loadSignals(fleet);
  }, [fleet, bumpBoard, loadSignals]);

  const addComment: Ctx["addComment"] = useCallback(async (opId, body) => {
    await postJSON(`/api/operations/${opId}/comments?fleet=${fleet}`, { body });
    loadOpDetail(fleet, opId);
  }, [fleet, loadOpDetail]);

  const addPilot: Ctx["addPilot"] = useCallback(async (name, kind) => {
    const res = await postJSON(`/api/pilots?fleet=${fleet}`, { name, kind });
    if (!res.ok) { await fail(res, "Add pilot failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const delPilot: Ctx["delPilot"] = useCallback(async (id) => { await del(`/api/pilots/${id}?fleet=${fleet}`); loadMeta(fleet); }, [fleet, loadMeta]);

  const addCrew: Ctx["addCrew"] = useCallback(async (name) => { await postJSON(`/api/crews?fleet=${fleet}`, { name }); loadMeta(fleet); }, [fleet, loadMeta]);
  const delCrew: Ctx["delCrew"] = useCallback(async (id) => { await del(`/api/crews/${id}?fleet=${fleet}`); loadMeta(fleet); }, [fleet, loadMeta]);
  const addMission: Ctx["addMission"] = useCallback(async (name, key) => {
    const res = await postJSON(`/api/missions?fleet=${fleet}`, { name, key });
    if (!res.ok) { await fail(res, "Add mission failed"); return false; }
    loadMeta(fleet);
    return true;
  }, [fleet, loadMeta]);
  const updateMission: Ctx["updateMission"] = useCallback(async (id, name, key) => {
    const res = await postJSON(`/api/missions/${id}?fleet=${fleet}`, { name, key });
    if (!res.ok) { await fail(res, "Update mission failed"); return false; }
    loadMeta(fleet); bumpBoard();
    return true;
  }, [fleet, loadMeta, bumpBoard]);
  const addMember: Ctx["addMember"] = useCallback(async (crewId, value, role, userId) => {
    const [member_type, id] = value === "me" ? ["user", userId] : value.split(":");
    await postJSON(`/api/crews/${crewId}/members?fleet=${fleet}`, { member_type, member_id: id, role });
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const removeMember: Ctx["removeMember"] = useCallback(async (crewId, member_type, member_id) => {
    await del(`/api/crews/${crewId}/members?fleet=${fleet}&member_type=${member_type}&member_id=${member_id}`);
    loadMeta(fleet);
  }, [fleet, loadMeta]);

  const addRover: Ctx["addRover"] = useCallback(async () => {
    const res = await postJSON(`/api/enrollment-codes?fleet=${fleet}`, { label: "rover", reusable: false });
    if (res.ok) { setNewEnrollmentCode((await res.json()).code); loadMeta(fleet); }
  }, [fleet, loadMeta]);
  const createReusableEnrollmentCode: Ctx["createReusableEnrollmentCode"] = useCallback(async (expiresAt) => {
    const res = await postJSON(`/api/enrollment-codes?fleet=${fleet}`, { label: "ci", reusable: true, expires_at: new Date(expiresAt + "T00:00:00").toISOString() });
    if (res.ok) { setNewEnrollmentCode((await res.json()).code); loadMeta(fleet); } else await fail(res, "Reusable enrollment code failed");
  }, [fleet, loadMeta]);
  const revokeRover: Ctx["revokeRover"] = useCallback(async (id) => { await del(`/api/rovers/${id}?fleet=${fleet}`); loadMeta(fleet); }, [fleet, loadMeta]);
  const setRoverTags: Ctx["setRoverTags"] = useCallback(async (id, tags) => {
    const res = await postJSON(`/api/rovers/${id}/tags?fleet=${fleet}`, { tags });
    if (!res.ok) { await fail(res, "Tag update failed"); return; }
    loadMeta(fleet);
  }, [fleet, loadMeta]);
  const revokeEnrollmentCode: Ctx["revokeEnrollmentCode"] = useCallback(async (id) => { await del(`/api/enrollment-codes/${id}?fleet=${fleet}`); loadMeta(fleet); }, [fleet, loadMeta]);

  const openSignal: Ctx["openSignal"] = useCallback(async (it) => {
    if (!it.read) { await postJSON(`/api/signals/${it.id}/read?fleet=${fleet}`); loadSignals(fleet); }
    if (it.operation_id != null) openOp(it.operation_id);
  }, [fleet, loadSignals, openOp]);
  const archiveSignal: Ctx["archiveSignal"] = useCallback(async (id) => { await postJSON(`/api/signals/${id}/archive?fleet=${fleet}`); loadSignals(fleet); }, [fleet, loadSignals]);

  const invite: Ctx["invite"] = useCallback(async (email, role) => {
    const res = await postJSON(`/api/invitations?fleet=${fleet}`, { email, role });
    if (!res.ok) { await fail(res, "Invite failed"); return false; }
    loadMembers(fleet); toast.success("Invitation sent");
    return true;
  }, [fleet, loadMembers]);
  const revokeInvite: Ctx["revokeInvite"] = useCallback(async (id) => { await del(`/api/invitations/${id}?fleet=${fleet}`); loadMembers(fleet); }, [fleet, loadMembers]);
  const acceptInvite: Ctx["acceptInvite"] = useCallback(async (id, fleetId) => {
    const res = await postJSON(`/api/invitations/${id}/accept`);
    if (!res.ok) { await fail(res, "Accept failed"); return; }
    localStorage.setItem("ufo.fleet", fleetId);
    window.location.reload();
  }, []);
  const declineInvite: Ctx["declineInvite"] = useCallback(async (id) => { await postJSON(`/api/invitations/${id}/decline`); loadMyInvites(); }, [loadMyInvites]);
  const setMemberRole: Ctx["setMemberRole"] = useCallback(async (userId, role) => {
    const res = await postJSON(`/api/members/${userId}/role?fleet=${fleet}`, { role });
    if (!res.ok) { await fail(res, "Role change failed"); return; }
    loadMembers(fleet);
  }, [fleet, loadMembers]);
  const removeFleetMember: Ctx["removeFleetMember"] = useCallback(async (userId) => {
    const res = await del(`/api/members/${userId}?fleet=${fleet}`);
    if (!res.ok) { await fail(res, "Remove failed"); return; }
    loadMembers(fleet);
  }, [fleet, loadMembers]);

  const value: Ctx = {
    user, fleets, fleet, switchFleet, createFleet, signOut,
    missions, missionCounts, pilots, crews, labels, rovers, enrollmentCodes, signals, newEnrollmentCode, boardTick,
    members, myRole, fleetInvites, myInvites,
    selectedOp, openOp, opDetail, selectedRun, setSelectedRun, runDetail,
    createOperation, setOperationTags, setPriority, setDates, setParent, setArchived,
    createLabel, deleteLabel, attachLabel, detachLabel, addPR, deletePR, addRelation, removeRelation, searchOps, react,
    reassign, runOp, moveOp, addComment,
    addPilot, delPilot, addCrew, delCrew, addMission, updateMission, addMember, removeMember,
    addRover, createReusableEnrollmentCode, revokeRover, setRoverTags, revokeEnrollmentCode, openSignal, archiveSignal,
    invite, revokeInvite, acceptInvite, declineInvite, setMemberRole, removeFleetMember,
  };
  return <AppCtx.Provider value={value}>{children}</AppCtx.Provider>;
}
