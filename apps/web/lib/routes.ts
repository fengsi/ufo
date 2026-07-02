export const APP_SECTIONS = ["operations", "missions", "routines", "crews", "rovers", "members", "settings"] as const;

export type Section = (typeof APP_SECTIONS)[number];

export type AppRoute = {
  fleetId: string | null;
  section: Section;
  operationId: string | null;
  userId: string | null;
};

function isSection(value: string | undefined): value is Section {
  return APP_SECTIONS.some((section) => section === value);
}

export function parseAppPath(path: string): AppRoute {
  const segments = path.split("/").filter(Boolean);
  const hasFleetId = segments[0] === "fleets" && segments[1] != null;
  const fleetId = hasFleetId ? segments[1] : null;
  const sectionIndex = hasFleetId ? 2 : 0;
  const head = segments[sectionIndex];

  if (head === "users" && segments[sectionIndex + 1]) {
    return {
      fleetId,
      section: "members",
      operationId: null,
      userId: segments[sectionIndex + 1],
    };
  }

  const section = isSection(head) ? head : "operations";
  const operationId = section === "operations" ? (segments[sectionIndex + 1] ?? null) : null;

  return { fleetId, section, operationId, userId: null };
}

export function appPath(fleetId: string, section: Section, operationId: string | null, userId: string | null = null): string {
  if (userId != null) return `/fleets/${fleetId}/users/${userId}`;
  return operationId == null ? `/fleets/${fleetId}/${section}` : `/fleets/${fleetId}/operations/${operationId}`;
}
