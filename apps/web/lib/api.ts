// Thin fetch helpers over the same-origin /api proxy. All UI calls are
// fleet-scoped via ?fleet=<id> (membership-checked server-side).

export async function getJSON<T>(path: string): Promise<T | null> {
  const res = await fetch(path, { cache: "no-store" });
  return res.ok ? ((await res.json()) as T) : null;
}

export async function postJSON(path: string, body?: unknown): Promise<Response> {
  return fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body == null ? undefined : JSON.stringify(body),
  });
}

export async function del(path: string): Promise<Response> {
  return fetch(path, { method: "DELETE" });
}

export function maxExpiryDate(): string {
  const d = new Date();
  d.setFullYear(d.getFullYear() + 1);
  return d.toISOString().slice(0, 10);
}
