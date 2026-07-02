import type { Member, User } from "@/lib/types";

const MENTION_RE = /(^|[\s(])@("([^"]+)"|([^\s@][^\s]*))/g;

export type MentionTarget = { id: string; label: string };

export function resolveUserMention(token: string, members: Member[], me: User): MentionTarget | null {
  const raw = token.trim();
  if (!raw) return null;
  const lower = raw.toLowerCase();
  const byEmail = members.find((m) => m.email.toLowerCase() === lower);
  if (byEmail) return { id: byEmail.id, label: displayMentionLabel(byEmail, members) };
  if (me.email.toLowerCase() === lower) return { id: me.id, label: displayMentionLabel({ id: me.id, name: me.name, email: me.email }, members) };

  const nameMatches = members.filter((m) => (m.name || "").trim().toLowerCase() === lower);
  if (nameMatches.length === 1) return { id: nameMatches[0].id, label: displayMentionLabel(nameMatches[0], members) };
  if (nameMatches.length > 1) return null;
  if ((me.name || "").trim().toLowerCase() === lower && !members.some((m) => m.id !== me.id && (m.name || "").trim().toLowerCase() === lower)) {
    return { id: me.id, label: displayMentionLabel({ id: me.id, name: me.name, email: me.email }, members) };
  }
  return null;
}

export function displayMentionLabel(user: { id: string; name: string; email: string }, members: Member[]): string {
  const name = (user.name || "").trim();
  if (!name) return user.email;
  const sameName = members.filter((m) => (m.name || "").trim().toLowerCase() === name.toLowerCase());
  if (sameName.length > 1) return `${name} (${user.email})`;
  return name;
}

export function linkUserMentions(text: string, members: Member[], me: User, fleetId: string): string {
  return text.replace(MENTION_RE, (full, prefix: string, _token: string, quoted?: string, bare?: string) => {
    const token = (quoted ?? bare ?? "").trim();
    const target = resolveUserMention(token, members, me);
    if (!target) return full;
    const href = `/fleets/${fleetId}/users/${target.id}`;
    const label = `@${target.label}`;
    return `${prefix}[${label}](${href})`;
  });
}

export function userHrefID(href?: string): string | null {
  const parts = href?.split("?")[0].split("/").filter(Boolean) ?? [];
  return parts[0] === "fleets" && parts[2] === "users" ? parts[3] : null;
}
