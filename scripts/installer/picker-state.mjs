export function createPickerState({ initialValues = [], multiple = false }) {
  return {
    index: 0,
    multiple,
    selectedValues: [...new Set(initialValues)],
    error: undefined,
  };
}

export function transitionPicker(state, action, optionValues) {
  if (action === "cancel") {
    return { state: { ...state, error: undefined }, done: true, value: null };
  }
  if (action === "up" || action === "down") {
    if (optionValues.length === 0) return { state, done: false };
    const offset = action === "up" ? -1 : 1;
    const index = (state.index + optionValues.length + offset) % optionValues.length;
    return { state: { ...state, index, error: undefined }, done: false };
  }
  if (action === "toggle" && state.multiple && optionValues[state.index] !== undefined) {
    const value = optionValues[state.index];
    const selectedValues = state.selectedValues.includes(value)
      ? state.selectedValues.filter((selected) => selected !== value)
      : [...state.selectedValues, value];
    return { state: { ...state, selectedValues, error: undefined }, done: false };
  }
  if (action === "all" && state.multiple) {
    return { state: { ...state, selectedValues: [...new Set(optionValues)], error: undefined }, done: false };
  }
  if (action === "none" && state.multiple) {
    return { state: { ...state, selectedValues: [], error: undefined }, done: false };
  }
  if (action === "enter") {
    if (state.multiple && state.selectedValues.length === 0) {
      return {
        state: { ...state, error: "Select at least one harness, or press q to cancel." },
        done: false,
      };
    }
    const value = state.multiple
      ? optionValues.filter((option) => state.selectedValues.includes(option))
      : optionValues[state.index];
    return { state: { ...state, error: undefined }, done: true, value };
  }
  return { state, done: false };
}
