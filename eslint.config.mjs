import { createRequire } from "node:module";

// Tool dependencies live outside the published package and have their own lock.
const require = createRequire(new URL("./bin/.lint-js/package.json", import.meta.url));
const js = require("@eslint/js");
const globals = require("globals");

export default [{
  files: ["**/*.mjs"],
  languageOptions: { ecmaVersion: 2022, sourceType: "module", globals: globals.node },
  rules: js.configs.recommended.rules,
}];
