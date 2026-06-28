export function initMCPScopeForms(root = document) {
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  scope.querySelectorAll("[data-mcp-scope-list]").forEach((list) => {
    if (!(list instanceof HTMLElement) || list.dataset.mcpScopeReady === "true") {
      return;
    }
    list.dataset.mcpScopeReady = "true";
    const inputs = Array.from(list.querySelectorAll('input[type="checkbox"][name="scopes"]'))
      .filter((input) => input instanceof HTMLInputElement);
    const publicRead = inputs.find((input) => input.hasAttribute("data-mcp-public-read"));
    if (!(publicRead instanceof HTMLInputElement)) {
      return;
    }
    const operationInputs = inputs.filter((input) => input !== publicRead);

    const setScopeDisabled = (input, disabled) => {
      input.disabled = disabled;
      if (disabled) {
        input.checked = false;
      }
      const option = input.closest("[data-mcp-scope-option]");
      if (option instanceof HTMLElement) {
        option.classList.toggle("is-disabled", disabled);
        option.setAttribute("aria-disabled", disabled ? "true" : "false");
      }
      const note = option?.nextElementSibling;
      if (note instanceof HTMLElement && note.hasAttribute("data-mcp-scope-note")) {
        note.classList.toggle("is-disabled", disabled);
      }
    };

    const syncScopes = (changedInput = null) => {
      if (changedInput && changedInput !== publicRead && changedInput.checked) {
        publicRead.checked = false;
      }
      operationInputs.forEach((input) => setScopeDisabled(input, publicRead.checked));
    };

    inputs.forEach((input) => {
      input.addEventListener("change", () => syncScopes(input));
    });
    syncScopes(publicRead.checked ? publicRead : null);
  });
}
