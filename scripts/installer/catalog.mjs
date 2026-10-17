function freezeHarness(harness) {
  return Object.freeze({
    ...harness,
    projectPath: Object.freeze(harness.projectPath),
    globalPath: Object.freeze(harness.globalPath),
    projectMarkers: Object.freeze(harness.projectMarkers.map((marker) => Object.freeze(marker))),
    globalMarkers: Object.freeze(harness.globalMarkers.map((marker) => Object.freeze(marker))),
    commands: Object.freeze(harness.commands),
  });
}

export const harnesses = Object.freeze([
  freezeHarness({
    name: "universal",
    label: "Universal Agent Skills",
    projectPath: [".agents", "skills"],
    globalPath: [".agents", "skills"],
    projectMarkers: [[".agents"]],
    globalMarkers: [[".agents"]],
    commands: [],
  }),
  freezeHarness({
    name: "codex",
    label: "Codex",
    projectPath: [".agents", "skills"],
    globalPath: [".agents", "skills"],
    projectMarkers: [[".agents"], [".codex"]],
    globalMarkers: [[".agents"], [".codex"]],
    commands: ["codex"],
  }),
  freezeHarness({
    name: "claude",
    label: "Claude Code",
    projectPath: [".claude", "skills"],
    globalPath: [".claude", "skills"],
    projectMarkers: [[".claude"]],
    globalMarkers: [[".claude"]],
    commands: ["claude"],
  }),
  freezeHarness({
    name: "cursor",
    label: "Cursor",
    projectPath: [".cursor", "skills"],
    globalPath: [".cursor", "skills"],
    projectMarkers: [[".cursor"]],
    globalMarkers: [[".cursor"]],
    commands: ["cursor"],
  }),
  freezeHarness({
    name: "gemini",
    label: "Gemini CLI",
    projectPath: [".gemini", "skills"],
    globalPath: [".gemini", "skills"],
    projectMarkers: [[".gemini"]],
    globalMarkers: [[".gemini"]],
    commands: ["gemini"],
  }),
  freezeHarness({
    name: "opencode",
    label: "OpenCode",
    projectPath: [".opencode", "skills"],
    globalPath: [".config", "opencode", "skills"],
    projectMarkers: [[".opencode"]],
    globalMarkers: [[".config", "opencode"]],
    commands: ["opencode"],
  }),
  freezeHarness({
    name: "pi",
    label: "Pi",
    projectPath: [".pi", "skills"],
    globalPath: [".pi", "agent", "skills"],
    projectMarkers: [[".pi"]],
    globalMarkers: [[".pi", "agent"]],
    commands: ["pi"],
  }),
  freezeHarness({
    name: "hermes",
    label: "Hermes",
    projectPath: [".hermes", "skills"],
    globalPath: [".hermes", "skills"],
    projectMarkers: [[".hermes"]],
    globalMarkers: [[".hermes"]],
    commands: ["hermes"],
  }),
]);

const harnessesByName = new Map(harnesses.map((harness) => [harness.name, harness]));

export function getHarness(name) {
  return harnessesByName.get(name) ?? null;
}

export function isHarnessName(name) {
  return harnessesByName.has(name);
}
