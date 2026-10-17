import { emitKeypressEvents } from "node:readline";
import { createPickerState, transitionPicker } from "./picker-state.mjs";

function renderPicker({ error, index, multiple, options, selectedValues, title }, output) {
  const selected = new Set(selectedValues);
  const lines = [title, ""];
  options.forEach((option, optionIndex) => {
    const current = optionIndex === index;
    const marker = multiple ? (selected.has(option.value) ? "[x]" : "[ ]") : "";
    const cursor = current ? "❯" : " ";
    const color = current ? "\u001b[36m" : "";
    const reset = current ? "\u001b[0m" : "";
    lines.push(`${color}${cursor} ${marker ? `${marker} ` : ""}${option.label}${reset}`);
    if (option.description) lines.push(`      ${option.description}`);
  });
  lines.push("");
  lines.push(multiple
    ? "↑↓ move  Space select  Enter continue  a all  n none  q cancel"
    : "↑↓ move  Enter select  q cancel");
  if (error) lines.push(`\u001b[31m${error}\u001b[0m`);
  output.write(`\u001b[2J\u001b[H${lines.join("\n")}\n`);
}

export function pickOptions({ initialValues = [], multiple = false, options, title, input, output }) {
  if (!input.isTTY || !output.isTTY || typeof input.setRawMode !== "function") return null;

  return new Promise((resolve, reject) => {
    let state = createPickerState({ initialValues, multiple });
    let settled = false;

    const cleanup = () => {
      const failures = [];
      try { input.off("keypress", onKeypress); } catch (error) { failures.push(error); }
      try { input.setRawMode(false); } catch (error) { failures.push(error); }
      try { input.pause(); } catch (error) { failures.push(error); }
      try { output.write("\u001b[?25h\u001b[0m"); } catch (error) { failures.push(error); }
      if (failures.length) {
        const details = failures.map((failure) => failure instanceof Error ? failure.message : String(failure));
        throw new Error(`terminal cleanup failed: ${details.join("; ")}`);
      }
    };

    const finish = (value) => {
      if (settled) return;
      settled = true;
      try {
        cleanup();
        resolve(value);
      } catch (error) {
        reject(error);
      }
    };

    const fail = (error) => {
      if (settled) return;
      settled = true;
      try {
        cleanup();
      } catch (cleanupError) {
        const reason = error instanceof Error ? error.message : String(error);
        const cleanupReason = cleanupError instanceof Error ? cleanupError.message : String(cleanupError);
        reject(new Error(`${reason}; ${cleanupReason}`));
        return;
      }
      reject(error);
    };

    const draw = () => renderPicker({ ...state, options, title }, output);
    const onKeypress = (character, key = {}) => {
      try {
        let action;
        if ((key.ctrl && key.name === "c") || key.name === "escape" || key.name === "q") action = "cancel";
        else if (key.name === "up" || key.name === "k") action = "up";
        else if (key.name === "down" || key.name === "j") action = "down";
        else if (multiple && (key.name === "space" || character === " ")) action = "toggle";
        else if (multiple && key.name === "a") action = "all";
        else if (multiple && key.name === "n") action = "none";
        else if (key.name === "return" || key.name === "enter") action = "enter";
        else return;

        const result = transitionPicker(state, action, options.map((option) => option.value));
        state = result.state;
        if (result.done) {
          finish(result.value);
        } else {
          draw();
        }
      } catch (error) {
        fail(error);
      }
    };

    try {
      emitKeypressEvents(input);
      input.setRawMode(true);
      input.resume();
      input.on("keypress", onKeypress);
      output.write("\u001b[?25l");
      draw();
    } catch (error) {
      fail(error);
    }
  });
}
