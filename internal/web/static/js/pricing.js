let pricingTierDeleteReady = false;

export function initPricingEditors(confirmUI, root = document) {
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  const normalizePricingMode = (mode) => (mode === "tiered" ? "tiered_expr" : mode || "token");
  const syncPricingEditorMode = (editor) => {
    if (!(editor instanceof HTMLElement)) {
      return;
    }
    const checked = editor.querySelector('input[name="billing_mode"]:checked');
    const mode = normalizePricingMode(checked instanceof HTMLInputElement ? checked.value : "");
    editor.dataset.pricingMode = mode;
    const scope = editor.closest("form") || editor;
    scope.querySelectorAll("[data-pricing-mode-panel]").forEach((panel) => {
      if (!(panel instanceof HTMLElement)) {
        return;
      }
      panel.hidden = normalizePricingMode(panel.dataset.pricingModePanel || "") !== mode;
    });
  };

  scope.querySelectorAll(".pricing-editor").forEach((editor) => {
    syncPricingEditorMode(editor);
    if (editor.dataset.pricingEditorReady === "true") {
      return;
    }
    editor.dataset.pricingEditorReady = "true";
    editor.querySelectorAll('input[name="billing_mode"]').forEach((input) => {
      input.addEventListener("change", () => syncPricingEditorMode(editor));
    });
  });


  scope.querySelectorAll("[data-pricing-tier-add]").forEach((button) => {
    if (button.dataset.pricingTierAddReady === "true") {
      return;
    }
    button.dataset.pricingTierAddReady = "true";
    const pricingEditor = button.closest(".pricing-editor");
    const actionTarget = button.hasAttribute("data-model-create-action-target")
      ? pricingEditor?.closest("form")?.querySelector(".form-actions")
      : null;
    if (actionTarget instanceof HTMLElement) {
      actionTarget.prepend(button);
    }

    button.addEventListener("click", () => {
      const tierEditor = pricingEditor?.querySelector(".tier-editor");
      const template = pricingEditor?.querySelector("[data-pricing-tier-template]");
      if (
        !(tierEditor instanceof HTMLElement) ||
        !(template instanceof HTMLTemplateElement)
      ) {
        return;
      }
      const row = template.content.firstElementChild?.cloneNode(true);
      if (!(row instanceof HTMLElement)) {
        return;
      }
      row.querySelectorAll("input").forEach((input) => {
        if (input instanceof HTMLInputElement) {
          input.value = "";
        }
      });
      tierEditor.append(row);
      const firstInput = row.querySelector("input");
      if (firstInput instanceof HTMLInputElement) {
        firstInput.focus();
      }
    });
  });

  if (pricingTierDeleteReady) {
    return;
  }
  pricingTierDeleteReady = true;
  document.addEventListener("click", (event) => {
    const target = event.target;
    const button = target instanceof Element ? target.closest("[data-pricing-tier-delete]") : null;
    if (!(button instanceof HTMLElement)) {
      return;
    }
    const row = button.closest(".tier-editor-row");
    if (!(row instanceof HTMLElement)) {
      return;
    }
    const message = (button.dataset.confirm || "").trim();
    if (message) {
      confirmUI.open(message, button.dataset.confirmTitle || "").then((accepted) => {
        if (accepted) {
          row.remove();
        }
      });
      return;
    }
    if (row instanceof HTMLElement) {
      row.remove();
    }
  });

}
