// Parse source only: never link, evaluate, or import inspected modules.
import { readFileSync } from "node:fs";
import { dirname, posix } from "node:path";
import { SourceTextModule } from "node:vm";

// SourceTextModule owns syntax validation. These finite lexical observations
// do not prove semantic purity. Import aliases are module-wide candidates;
// shadowing can over-report. Dynamic property names, assignment aliases and
// arbitrary returned objects are not resolved. Inspected code never executes.
function advance(state, count) {
  state.line += state.source.slice(state.offset, state.offset + count).split("\n").length - 1;
  state.offset += count;
}

function quotedToken(state, quote) {
  const line = state.line;
  let value = "";
  advance(state, 1);
  while (state.offset < state.source.length) {
    const character = state.source[state.offset];
    advance(state, 1);
    if (character === quote) return { kind: "string", value, line };
    if (character !== "\\") {
      value += character;
      continue;
    }
    const rest = state.source.slice(state.offset);
    const unicode = /^(?:u\{([0-9a-f]+)\}|u([0-9a-f]{4})|x([0-9a-f]{2}))/i.exec(rest);
    if (unicode) {
      value += String.fromCodePoint(Number.parseInt(unicode[1] ?? unicode[2] ?? unicode[3], 16));
      advance(state, unicode[0].length);
    } else {
      value += rest[0];
      advance(state, 1);
    }
  }
  throw new Error("unterminated string after syntax validation");
}

function skipRegexp(state) {
  const previous = state.tokens.at(-1)?.value;
  if (previous && !["=", "(", "[", "{", ",", ":", ";", "!", "?", "=>", "return", "throw", "case", "&&", "||", "??"].includes(previous)) return false;
  let bracket = false;
  for (let offset = state.offset + 1; offset < state.source.length; offset += 1) {
    const character = state.source[offset];
    if (character === "\n") return false;
    if (character === "\\") { offset += 1; continue; }
    if (character === "[") bracket = true;
    if (character === "]") bracket = false;
    if (character === "/" && !bracket) {
      const line = state.line;
      advance(state, offset - state.offset + 1);
      const flags = /^[a-z]*/i.exec(state.source.slice(state.offset))[0];
      advance(state, flags.length);
      state.tokens.push({ kind: "literal", value: "<regexp>", line });
      return true;
    }
  }
  return false;
}

function templateTokens(state) {
  const line = state.line;
  advance(state, 1);
  while (state.offset < state.source.length) {
    const character = state.source[state.offset];
    if (character === "\\") { advance(state, 2); continue; }
    if (character === "`") {
      advance(state, 1);
      state.tokens.push({ kind: "literal", value: "<template>", line });
      return;
    }
    if (state.source.startsWith("${", state.offset)) {
      advance(state, 2);
      codeTokens(state, true);
    } else {
      advance(state, 1);
    }
  }
  throw new Error("unterminated template after syntax validation");
}

function codeTokens(state, interpolation = false) {
  let braces = 0;
  while (state.offset < state.source.length) {
    const rest = state.source.slice(state.offset);
    const trivia = /^(?:\s+|\/\/[^\n]*|\/\*[\s\S]*?\*\/)/.exec(rest);
    if (trivia) { advance(state, trivia[0].length); continue; }
    if (rest[0] === "'" || rest[0] === '"') {
      state.tokens.push(quotedToken(state, rest[0]));
      continue;
    }
    if (rest[0] === "`") { templateTokens(state); continue; }
    if (rest[0] === "/" && skipRegexp(state)) continue;
    if (interpolation && rest[0] === "}" && braces === 0) { advance(state, 1); return; }
    if (rest[0] === "{") braces += 1;
    if (rest[0] === "}") braces -= 1;
    const identifier = /^(?:[$_\p{ID_Start}]|\\u(?:[0-9a-f]{4}|\{[0-9a-f]+\}))(?:[$\u200c\u200d\p{ID_Continue}]|\\u(?:[0-9a-f]{4}|\{[0-9a-f]+\}))*/iu.exec(rest);
    const operator = /^(?:\?\.|=>|&&|\|\||\?\?)/.exec(rest);
    const raw = identifier?.[0] ?? operator?.[0] ?? rest[0];
    const value = raw.replace(/\\u(?:([0-9a-f]{4})|\{([0-9a-f]+)\})/gi,
      (_, fixed, variable) => String.fromCodePoint(Number.parseInt(fixed ?? variable, 16)));
    state.tokens.push({ kind: identifier ? "identifier" : "punctuation", value, line: state.line });
    advance(state, raw.length);
  }
}

function importBindings(tokens) {
  const aliases = new Map();
  const declarations = new Set();
  for (let index = 0; index < tokens.length; index += 1) {
    if (tokens[index].value !== "import" || ["(", "."].includes(tokens[index + 1]?.value)) continue;
    let end = index + 1;
    while (end < tokens.length && tokens[end].kind !== "string") end += 1;
    if (end === tokens.length) continue;
    const module = tokens[end].value.replace(/^node:/, "");
    for (let part = index; part <= end; part += 1) declarations.add(part);
    if (!["path", "path/posix", "path/win32"].includes(module)) { index = end; continue; }
    const moduleName = `node:${module.replace("/", ".")}`;
    let named = false;
    for (let part = index + 1; part < end; part += 1) {
      const token = tokens[part];
      if (token.value === "{") { named = true; continue; }
      if (token.kind !== "identifier" || ["as", "from"].includes(token.value)) continue;
      const target = named ? `${moduleName}.${token.value}` : moduleName;
      let local = token.value;
      if (tokens[part + 1]?.value === "as") { local = tokens[part + 2].value; part += 2; }
      const candidates = aliases.get(local) ?? new Set();
      candidates.add(target);
      aliases.set(local, candidates);
    }
    index = end;
  }
  return { aliases, declarations };
}

function memberChain(tokens, index) {
  let name = tokens[index].value;
  let end = index + 1;
  while (end < tokens.length) {
    if ([".", "?."].includes(tokens[end].value) && tokens[end + 1]?.kind === "identifier") {
      name += `.${tokens[end + 1].value}`;
      end += 2;
    } else if (tokens[end].value === "[" && tokens[end + 1]?.kind === "string" && tokens[end + 2]?.value === "]") {
      name += `.${tokens[end + 1].value}`;
      end += 3;
    } else break;
  }
  return { name, end };
}

function effectName(name, tokens, index, end) {
  const global = name.replace(/^globalThis\./, "");
  if (global === "process" || global.startsWith("process.")) return "process";
  for (const reference of ["Math.random", "Date.now"]) {
    if (global === reference || global.startsWith(reference + ".")) return reference;
  }
  if (global === "Date") {
    const construction = tokens[index - 1]?.value === "new";
    const argumentsList = tokens[end]?.value === "(";
    if (construction && (!argumentsList || tokens[end + 1]?.value === ")")) return "Date clock";
    if (!construction && argumentsList) return "Date clock";
  }
  for (const reference of ["node:path.resolve", "node:path.posix.resolve", "node:path.win32.resolve"]) {
    if (name === reference || name.startsWith(reference + ".")) return "node:path.resolve";
  }
  return null;
}

function lexicalEffects(source) {
  const state = { source, offset: 0, line: 1, tokens: [] };
  codeTokens(state);
  const { tokens } = state;
  const { aliases, declarations } = importBindings(tokens);
  const effects = [];
  const observed = new Set();
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (declarations.has(index) || token.kind !== "identifier") continue;
    if ([".", "?."].includes(tokens[index - 1]?.value) || tokens[index + 1]?.value === ":") continue;
    const { name, end } = memberChain(tokens, index);
    const candidates = [name];
    for (const target of aliases.get(token.value) ?? []) candidates.push(target + name.slice(token.value.length));
    for (const candidate of candidates) {
      const effect = effectName(candidate, tokens, index, end);
      const key = `${token.line}:${effect}`;
      if (effect && !observed.has(key)) {
        observed.add(key);
        effects.push({ name: effect, line: token.line });
      }
    }
    index = end - 1;
  }
  return effects;
}

function sourceFacts(path) {
  const source = readFileSync(path, "utf8");
  const parsed = new SourceTextModule(source, { identifier: path });
  const imports = parsed.dependencySpecifiers.map((specifier) => ({
    target: `js:${specifier.startsWith(".") ? posix.normalize(posix.join(dirname(path), specifier)) : specifier}`,
    // Module metadata identifies dependencies without observing their positions.
    line: 0,
  }));
  if (parsed.status !== "unlinked") throw new Error("source parser evaluated input");
  return { path, module: `js:${path}`, imports, effects: lexicalEffects(source) };
}

const paths = JSON.parse(readFileSync(0, "utf8"));
if (!Array.isArray(paths) || !paths.every((path) => typeof path === "string")) {
  throw new Error("expected a list of source paths");
}
process.stdout.write(JSON.stringify(paths.map(sourceFacts)));
