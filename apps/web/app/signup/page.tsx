"use client";

import { useState } from "react";
import { AuthCard, AuthInput, AuthButton } from "../auth-ui";

export default function SignupPage() {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    const res = await fetch("/api/auth/signup", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, email, password }),
    });
    setBusy(false);
    if (res.ok) window.location.href = "/";
    else {
      const d = await res.json().catch(() => ({}));
      setError(d.error || "Sign up failed");
    }
  }

  return (
    <AuthCard title="Create your account" error={error} footer={{ text: "Already have an account?", href: "/login", label: "Sign in" }}>
      <form onSubmit={submit} className="space-y-3">
        <AuthInput placeholder="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <AuthInput type="email" placeholder="Email" value={email} onChange={(e) => setEmail(e.target.value)} />
        <AuthInput type="password" placeholder="Password (8+ chars)" value={password} onChange={(e) => setPassword(e.target.value)} />
        <AuthButton className="w-full" disabled={busy} type="submit">{busy ? "Creating…" : "Create account"}</AuthButton>
      </form>
    </AuthCard>
  );
}
