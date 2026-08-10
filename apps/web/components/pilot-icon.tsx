import { Bot } from "lucide-react";

import { cn } from "@/lib/utils";

type PilotIconAsset = {
  src: string;
  lightOutline?: boolean;
  monochrome?: boolean;
};

const ICONS: Record<string, PilotIconAsset> = {
  claude: { src: "/pilot-icons/claude.svg" },
  codex: { src: "/pilot-icons/codex.svg" },
  antigravity: { src: "/pilot-icons/antigravity.svg" },
  grok: { src: "/pilot-icons/grok.svg", monochrome: true },
  cursor: { src: "/pilot-icons/cursor.svg", monochrome: true },
  copilot: { src: "/pilot-icons/copilot.svg", monochrome: true },
  opencode: { src: "/pilot-icons/opencode.svg", monochrome: true },
  amp: { src: "/pilot-icons/amp.svg" },
  openclaw: { src: "/pilot-icons/openclaw.svg" },
  hermes: { src: "/pilot-icons/hermes.svg", monochrome: true },
  pi: { src: "/pilot-icons/pi.svg", monochrome: true },
  kilo: { src: "/pilot-icons/kilo.svg", monochrome: true },
  mastracode: { src: "/pilot-icons/mastracode.svg", monochrome: true },
  devin: { src: "/pilot-icons/devin.svg" },
  kiro: { src: "/pilot-icons/kiro.svg" },
  kimi: { src: "/pilot-icons/kimi.svg", lightOutline: true },
  codebuddy: { src: "/pilot-icons/codebuddy.svg" },
  qoder: { src: "/pilot-icons/qoder.svg" },
  trae: { src: "/pilot-icons/trae.svg" },
};

export function PilotIcon({ kind, size = 15 }: { kind: string; size?: number }) {
  const icon = ICONS[kind];
  if (!icon) return <Bot size={size} aria-hidden style={{ flex: "none" }} />;
  return (
    <img
      alt=""
      aria-hidden
      className={cn(
        "ufo-pilot-icon",
        icon.lightOutline && "ufo-pilot-icon-light-outline",
        icon.monochrome && "ufo-pilot-icon-monochrome",
      )}
      draggable={false}
      height={size}
      src={icon.src}
      style={{ flex: "none", height: size, width: size }}
      width={size}
    />
  );
}
