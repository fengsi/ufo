export const APP_SECTIONS = ["operations", "missions", "crews", "rovers", "members", "settings"] as const;

export type Section = (typeof APP_SECTIONS)[number];

export type AppRoute = {
  fleetId: string | null;
  section: Section;
  operationId: string | null;
};

function isSection(value: string | undefined): value is Section {
  return APP_SECTIONS.some((section) => section === value);
}

export function parseAppPath(path: string): AppRoute {
  const segments = path.split("/").filter(Boolean);
  const hasFleetId = segments[0] === "fleets" && segments[1] != null;
  const fleetId = hasFleetId ? segments[1] : null;
  const sectionIndex = hasFleetId ? 2 : 0;
  const section = isSection(segments[sectionIndex]) ? segments[sectionIndex] : "operations";
  const operationId = section === "operations" ? (segments[sectionIndex + 1] ?? null) : null;

  return { fleetId, section, operationId };
}

export function appPath(fleetId: string, section: Section, operationId: string | null): string {
  return operationId == null ? `/fleets/${fleetId}/${section}` : `/fleets/${fleetId}/operations/${operationId}`;
}
