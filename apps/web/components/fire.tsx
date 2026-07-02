import { cn } from "@/lib/utils";
import type { Operation } from "@/lib/types";

export function onFire(op: Operation) {
  return op.priority >= 4 && op.status !== "done" && op.status !== "canceled";
}

// Deterministic PRNG from a string seed.
function seededRng(seed: string) {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return () => {
    h += 0x6d2b79f5;
    let t = Math.imul(h ^ (h >>> 15), 1 | h);
    t ^= t + Math.imul(t ^ (t >>> 7), 61 | t);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Left-edge flames; per-operation tongue shapes + timing (seeded by id).
export function Flames({ detail, seed }: { detail?: boolean; seed?: string }) {
  const r = seededRng(seed ?? "ufo");
  const tongues = Array.from({ length: 5 }, () => {
    const width = 34 + Math.round(r() * 60);
    return {
      width,
      left: Math.round(r() * (100 - width)),
      height: 60 + Math.round(r() * 40),
      opacity: 0.55 + r() * 0.45,
      dur: 600 + Math.round(r() * 760),
      delay: -Math.round(r() * 1400),
    };
  });
  return (
    <span aria-hidden className={cn("ufo-fire", detail && "ufo-detail")}>
      <span className="ufo-flames">
        {tongues.map((t, k) => (
          <i
            key={k}
            style={{ left: `${t.left}%`, width: `${t.width}%`, height: `${t.height}%`, opacity: t.opacity, animationDuration: `${t.dur}ms`, animationDelay: `${t.delay}ms` }}
          />
        ))}
      </span>
      <span className="ufo-sparks"><b /><b /><b /><b /></span>
    </span>
  );
}

const FLAME_PATH =
  "M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.07-2.14-.22-4.05 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.15.43-2.29 1-3a2.5 2.5 0 0 0 2.5 2.5z";

type Fly = { left: string; top: string; size: number; path: string; dur: string; delay: string; peak: number; spin: string; rev?: boolean; flame?: boolean; e?: string };
const FLY: Fly[] = [
  { left: "0%",  top: "34%", size: 84, path: "ufo-path-cross",  dur: "14s",   delay: "0s",   peak: 0.7,  spin: "7s",   flame: true },
  { left: "96%", top: "90%", size: 64, path: "ufo-path-diag2",  dur: "16s",   delay: "2.5s", peak: 0.62, spin: "5s",   rev: true, e: "😱" },
  { left: "44%", top: "44%", size: 70, path: "ufo-path-orbit",  dur: "12s",   delay: "4.5s", peak: 0.66, spin: "6s",   e: "🥵" },
  { left: "2%",  top: "92%", size: 66, path: "ufo-path-diag",   dur: "15s",   delay: "1.2s", peak: 0.62, spin: "8s",   rev: true, e: "🫠" },
  { left: "0%",  top: "66%", size: 66, path: "ufo-path-cross",  dur: "17s",   delay: "6s",   peak: 0.64, spin: "4.5s", rev: true, e: "😨" },
  { left: "0%",  top: "78%", size: 70, path: "ufo-path-arc",    dur: "15s",   delay: "3.4s", peak: 0.64, spin: "6.5s", e: "🤯" },
  { left: "30%", top: "96%", size: 66, path: "ufo-path-rise",   dur: "13s",   delay: "7.5s", peak: 0.6,  spin: "5.5s", rev: true, e: "🥶" },
  { left: "72%", top: "30%", size: 62, path: "ufo-path-orbit",  dur: "12.5s", delay: "5.2s", peak: 0.58, spin: "7.5s", rev: true, e: "🙃" },
  { left: "68%", top: "96%", size: 64, path: "ufo-path-rise",   dur: "14.5s", delay: "8.6s", peak: 0.58, spin: "9s",   e: "🙄" },
];

export function DetailFire() {
  return (
    <span aria-hidden className="ufo-flydeck">
      {FLY.map((f, i) => {
        const spin = { animationName: f.rev ? "ufo-spin-rev" : "ufo-spin", animationDuration: f.spin };
        return (
          <span
            key={i}
            className="ufo-fly"
            style={{ left: f.left, top: f.top, animationName: f.path, animationDuration: f.dur, animationDelay: f.delay, ["--peak" as string]: f.peak }}
          >
            {f.flame ? (
              <span className="ufo-flyspin" style={spin}>
                <svg viewBox="0 0 24 24" width={f.size} height={f.size} className="ufo-flyflame"><path d={FLAME_PATH} fill="currentColor" /></svg>
              </span>
            ) : (
              <span className="ufo-flyspin" style={{ ...spin, fontSize: f.size }}>{f.e}</span>
            )}
          </span>
        );
      })}
    </span>
  );
}
