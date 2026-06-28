let modelGroupGlobalListenersReady = false;

const modelGroupPanel = (details) => {
  if (!(details instanceof HTMLElement)) {
    return null;
  }
  const panel = details.querySelector(".model-group-panel");
  return panel instanceof HTMLElement ? panel : null;
};

const modelGroupSummary = (details) => {
  if (!(details instanceof HTMLElement)) {
    return null;
  }
  const summary = details.querySelector("summary");
  return summary instanceof HTMLElement ? summary : null;
};

const resetModelGroupPanelPosition = (details) => {
  const panel = modelGroupPanel(details);
  if (!panel) {
    return;
  }
  panel.classList.remove("is-fixed");
  panel.style.left = "";
  panel.style.right = "";
  panel.style.top = "";
  panel.style.bottom = "";
  panel.style.width = "";
  panel.style.maxHeight = "";
  panel.style.zIndex = "";
  panel.dataset.placement = "";
};

const modelGroupPanelZIndex = (details) => {
  const layer = details.closest(".settings-overlay, .confirm-overlay, .prompt-overlay");
  if (!(layer instanceof HTMLElement)) {
    return "85";
  }
  const zIndex = Number.parseInt(window.getComputedStyle(layer).zIndex, 10);
  return Number.isFinite(zIndex) ? String(zIndex + 5) : "95";
};

const positionModelGroupPanel = (details) => {
  if (!(details instanceof HTMLDetailsElement) || !details.open) {
    resetModelGroupPanelPosition(details);
    return;
  }
  const summary = modelGroupSummary(details);
  const panel = modelGroupPanel(details);
  if (!summary || !panel) {
    return;
  }
  const rect = summary.getBoundingClientRect();
  if (rect.width <= 0 || rect.height <= 0) {
    return;
  }

  const viewportMargin = 8;
  const panelGap = 6;
  const viewportHeight = document.documentElement.clientHeight || window.innerHeight;
  const viewportWidth = document.documentElement.clientWidth || window.innerWidth;
  const spaceBelow = viewportHeight - rect.bottom - viewportMargin;
  const spaceAbove = rect.top - viewportMargin;
  const openAbove = spaceBelow < 180 && spaceAbove > spaceBelow;
  const availableHeight = Math.max(120, (openAbove ? spaceAbove : spaceBelow) - panelGap);
  const preferredWidth = Math.max(rect.width, 280);
  const width = Math.min(preferredWidth, Math.max(120, viewportWidth - viewportMargin * 2));
  const left = Math.min(
    Math.max(viewportMargin, rect.left),
    Math.max(viewportMargin, viewportWidth - width - viewportMargin)
  );

  panel.classList.add("is-fixed");
  panel.style.left = `${left}px`;
  panel.style.right = "auto";
  panel.style.width = `${width}px`;
  panel.style.maxHeight = `${Math.min(320, availableHeight)}px`;
  panel.style.zIndex = modelGroupPanelZIndex(details);
  panel.dataset.placement = openAbove ? "top" : "bottom";
  if (openAbove) {
    panel.style.top = "auto";
    panel.style.bottom = `${viewportHeight - rect.top + panelGap}px`;
  } else {
    panel.style.top = `${rect.bottom + panelGap}px`;
    panel.style.bottom = "auto";
  }
};

const openModelGroupPopovers = () => Array.from(document.querySelectorAll(".model-group-popover[open]"))
  .filter((details) => details instanceof HTMLDetailsElement);

export function initModelGroupPopovers(root = document) {
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  const updateModelGroupSummary = (root) => {
    if (!(root instanceof HTMLElement)) {
      return;
    }
    const summary = root.querySelector("[data-model-groups-summary]");
    if (!(summary instanceof HTMLElement)) {
      return;
    }
    const selected = Array.from(root.querySelectorAll("input[type='checkbox']:checked"))
      .filter((input) => input instanceof HTMLInputElement && input.name === "visible_group")
      .map((input) => {
        if (!(input instanceof HTMLInputElement)) {
          return "";
        }
        const label = input.closest("label")?.querySelector("span");
        return label?.textContent?.trim() || "";
      })
      .filter(Boolean);
    root.querySelectorAll("input[type='checkbox']").forEach((input) => {
      if (input instanceof HTMLInputElement && input.name === "visible_group") {
        input.disabled = false;
      }
    });
    summary.textContent = selected.length ? selected.join(", ") : root.dataset.emptyLabel || "none";
  };

  scope.querySelectorAll(".model-group-popover").forEach((popover) => {
    if (popover.dataset.modelGroupReady === "true") {
      updateModelGroupSummary(popover);
      return;
    }
    popover.dataset.modelGroupReady = "true";
    updateModelGroupSummary(popover);
    popover.addEventListener("change", (event) => {
      const target = event.target;
      if (target instanceof HTMLInputElement && target.name === "visible_group") {
        updateModelGroupSummary(popover);
      }
    });
    popover.querySelectorAll("[data-model-groups-select]").forEach((button) => {
      button.addEventListener("click", () => {
        const mode = button.dataset.modelGroupsSelect || "";
        popover.querySelectorAll("input[type='checkbox']").forEach((input) => {
          if (input instanceof HTMLInputElement) {
            if (input.name === "visible_group") {
              input.checked = mode === "all";
            }
            input.disabled = false;
          }
        });
        updateModelGroupSummary(popover);
      });
    });
  });

  const closeModelGroupPopovers = (except = null) => {
    document.querySelectorAll(".model-group-popover[open]").forEach((details) => {
      if (details !== except) {
        details.removeAttribute("open");
        resetModelGroupPanelPosition(details);
      }
    });
  };

  scope.querySelectorAll(".model-group-popover").forEach((details) => {
    if (details.dataset.modelGroupToggleReady === "true") {
      return;
    }
    details.dataset.modelGroupToggleReady = "true";
    details.addEventListener("toggle", () => {
      if (details.open) {
        closeModelGroupPopovers(details);
        positionModelGroupPanel(details);
      } else {
        resetModelGroupPanelPosition(details);
      }
    });
    details.closest(".settings-overlay")?.addEventListener("modal:close", () => {
      details.removeAttribute("open");
      resetModelGroupPanelPosition(details);
    });
  });

  if (modelGroupGlobalListenersReady) {
    return;
  }
  modelGroupGlobalListenersReady = true;
  document.addEventListener("click", (event) => {
    const target = event.target;
    openModelGroupPopovers().forEach((details) => {
      if (target instanceof Node && details.contains(target)) {
        return;
      }
      details.removeAttribute("open");
      resetModelGroupPanelPosition(details);
    });
  });
  window.addEventListener("resize", () => {
    openModelGroupPopovers().forEach(positionModelGroupPanel);
  });
  window.addEventListener(
    "scroll",
    () => {
      openModelGroupPopovers().forEach(positionModelGroupPanel);
    },
    true
  );
}
