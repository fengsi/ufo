"use client";

import { useState } from "react";
import { AuthCard, AuthInput, AuthButton } from "../auth-ui";

export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    const res = await fetch("/api/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password }),
    });
    setBusy(false);
    if (res.ok) window.location.href = "/";
    else {
      const d = await res.json().catch(() => ({}));
      setError(d.error || "Login failed");
    }
  }

  return (
    <AuthCard title="Sign in" error={error} footer={{ text: "Need an account?", href: "/signup", label: "Sign up" }}>
      <form onSubmit={submit} className="space-y-3">
        <AuthInput type="email" placeholder="Email" value={email} onChange={(e) => setEmail(e.target.value)} />
        <AuthInput type="password" placeholder="Password" value={password} onChange={(e) => setPassword(e.target.value)} />
        <AuthButton className="w-full" disabled={busy} type="submit">{busy ? "Signing in…" : "Sign in"}</AuthButton>
      </form>
    </AuthCard>
  );
}
