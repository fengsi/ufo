import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function hideFlowControlFlags(text: string) {
  return text
    .replace(/^[ \t]*@@UFO_STATUS:(?:in_review|done|blocked|canceled)(?:@@)?[ \t]*(?:\r?\n|$)/gim, "")
    .replace(/^[ \t]*@@UFO_OPERATIONS@@.*(?:\r?\n|$)/gim, "")
    .replace(/^[ \t]*@@UFO_SUB_OPERATIONS(?:_FEEDBACK)?@@.*(?:\r?\n|$)/gim, "")
    .replace(/@@UFO_NEEDS_INPUT@@\s*/g, "")
    .replace(/@@UFO_STATUS:(?:in_review|done|blocked|canceled)(?:@@)?/gi, "")
    .replace(/@@UFO_OPERATIONS@@/g, "")
    .replace(/@@UFO_SUB_OPERATIONS(?:_FEEDBACK)?@@/g, "");
}
