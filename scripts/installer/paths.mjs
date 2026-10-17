import { isAbsolute, join } from "node:path";

export function normalizeAbsolute(value) {
  if (!isAbsolute(value)) throw new Error("Expected an absolute path");
  return join(value, ".");
}

export function resolveFrom(base, value) {
  if (isAbsolute(value)) return normalizeAbsolute(value);
  if (typeof base !== "string" || !isAbsolute(base)) {
    throw new Error("A relative configuration path requires an absolute working directory");
  }
  return join(base, value, ".");
}
