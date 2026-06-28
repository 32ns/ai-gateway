const enhanced = [];
let enhancedSelectListenersReady = false;

const cleanupDisconnectedEnhancedSelects = () => {
  for (let index = enhanced.length - 1; index >= 0; index -= 1) {
    const entry = enhanced[index];
    if (!entry.shell.isConnected) {
      if (typeof entry.cleanup === "function") {
        entry.cleanup();
      }
      entry.menu.remove();
      enhanced.splice(index, 1);
    }
  }
};

const openEnhancedSelects = () => enhanced.filter((entry) => !entry.menu.hidden);

export function initEnhancedSelects(root = document) {
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  cleanupDisconnectedEnhancedSelects();
  scope.querySelectorAll("select").forEach((select) => {
    if (select.multiple || select.size > 1) {
      return;
    }
    if (select.dataset.enhanced === "true") {
      return;
    }

    const shell = document.createElement("div");
    shell.className = "select-shell";

    const trigger = document.createElement("button");
    trigger.type = "button";
    trigger.className = "select-trigger";
    trigger.setAttribute("aria-haspopup", "listbox");
    trigger.setAttribute("aria-expanded", "false");
    if (select.hasAttribute("data-modal-autofocus")) {
      trigger.dataset.modalAutofocus = select.dataset.modalAutofocus || "true";
      select.removeAttribute("data-modal-autofocus");
    }

    const triggerLabel = document.createElement("span");
    triggerLabel.className = "select-trigger-label";
    trigger.appendChild(triggerLabel);

    const menu = document.createElement("ul");
    menu.className = "select-menu";
    menu.setAttribute("role", "listbox");
    menu.hidden = true;

    const options = [];
    let optionSignature = "";
    const preferredMaxHeight = 240;
    const viewportMargin = 8;
    const menuGap = 4;

    const currentOptionSignature = () => Array.from(select.options)
      .map((option) => `${option.value}\u0000${option.textContent || ""}\u0000${option.dataset.selectDescription || ""}\u0000${option.disabled ? "1" : "0"}\u0000${option.dataset.selectSeparator === "true" ? "1" : "0"}`)
      .join("\u0001");

    const renderOptionContent = (node, option, mainClass, descriptionClass) => {
      const text = option ? option.textContent || "" : "";
      const description = (option?.dataset.selectDescription || "").trim();
      node.classList.toggle("has-description", Boolean(description));
      if (!description) {
        node.textContent = text;
        return;
      }
      const main = document.createElement("span");
      main.className = mainClass;
      main.textContent = text;
      const descriptionNode = document.createElement("span");
      descriptionNode.className = descriptionClass;
      descriptionNode.textContent = description;
      node.replaceChildren(main, descriptionNode);
    };

    const rebuildOptions = () => {
      const nextSignature = currentOptionSignature();
      if (nextSignature === optionSignature && options.length === select.options.length) {
        return;
      }
      optionSignature = nextSignature;
      options.splice(0, options.length);
      menu.replaceChildren();

      Array.from(select.options).forEach((option, index) => {
        const item = document.createElement("li");
        if (option.dataset.selectSeparator === "true") {
          item.className = "select-separator-item";
          item.setAttribute("aria-hidden", "true");
          const separator = document.createElement("div");
          separator.className = "select-separator";
          item.appendChild(separator);
          menu.appendChild(item);
          options.push(null);
          return;
        }

        const optionButton = document.createElement("button");
        optionButton.type = "button";
        optionButton.className = "select-option";
        optionButton.setAttribute("role", "option");
        renderOptionContent(optionButton, option, "select-option-main", "select-option-description");
        optionButton.disabled = option.disabled;

        optionButton.addEventListener("click", () => {
          const currentOption = select.options[index];
          if (!currentOption || currentOption.disabled || select.disabled) {
            return;
          }
          select.selectedIndex = index;
          select.dispatchEvent(new Event("change", { bubbles: true }));
          syncFromSelect();
          closeMenu();
          trigger.focus();
        });

        optionButton.addEventListener("keydown", (event) => {
          if (event.key === "Escape") {
            event.preventDefault();
            closeMenu();
            trigger.focus();
            return;
          }

          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") {
            return;
          }

          event.preventDefault();
          const direction = event.key === "ArrowDown" ? 1 : -1;
          let nextIndex = index;
          do {
            nextIndex += direction;
          } while (nextIndex >= 0 && nextIndex < options.length && (!options[nextIndex] || options[nextIndex].disabled));

          if (nextIndex >= 0 && nextIndex < options.length) {
            options[nextIndex].focus();
          }
        });

        item.appendChild(optionButton);
        menu.appendChild(item);
        options.push(optionButton);
      });
    };

    const syncFromSelect = () => {
      rebuildOptions();
      const selectedIndex = select.selectedIndex >= 0 ? select.selectedIndex : 0;
      const selectedOption = select.options[selectedIndex];
      renderOptionContent(triggerLabel, selectedOption, "select-trigger-main", "select-trigger-description");
      trigger.disabled = select.disabled;
      trigger.setAttribute("aria-disabled", select.disabled ? "true" : "false");
      options.forEach((optionButton, index) => {
        if (!optionButton) {
          return;
        }
        const isSelected = index === selectedIndex;
        optionButton.setAttribute("aria-selected", isSelected ? "true" : "false");
        optionButton.classList.toggle("is-active", isSelected);
      });
    };

    const resetMenuPosition = () => {
      menu.classList.remove("is-portaled");
      menu.style.left = "";
      menu.style.right = "";
      menu.style.top = "";
      menu.style.bottom = "";
      menu.style.width = "";
      menu.style.maxHeight = "";
      menu.style.zIndex = "";
      menu.dataset.placement = "";
      if (menu.parentNode !== shell) {
        shell.appendChild(menu);
      }
    };

    const portalZIndex = () => {
      const layer = shell.closest(".settings-overlay, .confirm-overlay, .prompt-overlay");
      if (!(layer instanceof HTMLElement)) {
        return "";
      }
      const zIndex = Number.parseInt(window.getComputedStyle(layer).zIndex, 10);
      if (!Number.isFinite(zIndex)) {
        return "";
      }
      return String(zIndex + 5);
    };

    const positionMenu = () => {
      if (menu.hidden) {
        return;
      }
      const rect = trigger.getBoundingClientRect();
      if (rect.width <= 0 || rect.height <= 0) {
        closeMenu();
        return;
      }
      if (menu.parentNode !== document.body) {
        document.body.appendChild(menu);
      }
      menu.classList.add("is-portaled");
      const viewportHeight = document.documentElement.clientHeight || window.innerHeight;
      const viewportWidth = document.documentElement.clientWidth || window.innerWidth;
      const spaceBelow = viewportHeight - rect.bottom - viewportMargin;
      const spaceAbove = rect.top - viewportMargin;
      const openAbove = spaceBelow < 160 && spaceAbove > spaceBelow;
      const available = Math.max(96, (openAbove ? spaceAbove : spaceBelow) - menuGap);
      const maxHeight = Math.min(preferredMaxHeight, available);
      const maxWidth = Math.max(96, viewportWidth - viewportMargin * 2);
      const width = Math.min(Math.max(rect.width, 92), maxWidth);
      const left = Math.min(
        Math.max(viewportMargin, rect.left),
        Math.max(viewportMargin, viewportWidth - width - viewportMargin)
      );

      menu.style.left = `${left}px`;
      menu.style.right = "auto";
      menu.style.width = `${width}px`;
      menu.style.maxHeight = `${maxHeight}px`;
      menu.style.zIndex = portalZIndex();
      menu.dataset.placement = openAbove ? "top" : "bottom";
      if (openAbove) {
        menu.style.top = "auto";
        menu.style.bottom = `${viewportHeight - rect.top + menuGap}px`;
      } else {
        menu.style.top = `${rect.bottom + menuGap}px`;
        menu.style.bottom = "auto";
      }
    };

    function closeMenu() {
      shell.classList.remove("is-open");
      trigger.setAttribute("aria-expanded", "false");
      menu.hidden = true;
      resetMenuPosition();
    }

    const openMenu = () => {
      syncFromSelect();
      if (select.disabled) {
        return;
      }
      shell.classList.add("is-open");
      trigger.setAttribute("aria-expanded", "true");
      menu.hidden = false;
      positionMenu();
      const selected = options[select.selectedIndex] || options.find((option) => option && !option.disabled);
      if (selected) {
        selected.focus();
      }
    };

    trigger.addEventListener("click", () => {
      if (select.disabled) {
        return;
      }
      if (menu.hidden) {
        openMenu();
      } else {
        closeMenu();
      }
    });

    trigger.addEventListener("keydown", (event) => {
      if (event.key === "ArrowDown" || event.key === "ArrowUp" || event.key === " " || event.key === "Enter") {
        event.preventDefault();
        openMenu();
      }
      if (event.key === "Escape") {
        closeMenu();
      }
    });

    select.addEventListener("change", syncFromSelect);
    select.addEventListener("select:refresh", () => {
      syncFromSelect();
      positionMenu();
    });
    const observer = new MutationObserver(syncFromSelect);
    observer.observe(select, {
      attributes: true,
      attributeFilter: ["disabled", "data-select-description"],
      childList: true,
      subtree: true,
    });

    select.classList.add("select-native");
    select.dataset.enhanced = "true";
    select.parentNode.insertBefore(shell, select);
    shell.appendChild(select);
    shell.appendChild(trigger);
    shell.appendChild(menu);

    syncFromSelect();
    select.closest(".settings-overlay")?.addEventListener("modal:close", closeMenu);

    enhanced.push({ shell, menu, closeMenu, positionMenu, cleanup: () => observer.disconnect() });
  });

  if (enhancedSelectListenersReady) {
    return;
  }
  enhancedSelectListenersReady = true;
  document.addEventListener("click", (event) => {
    const openEntries = openEnhancedSelects();
    if (openEntries.length === 0) {
      return;
    }
    cleanupDisconnectedEnhancedSelects();
    openEntries.forEach((entry) => {
      if (!entry.shell.contains(event.target) && !entry.menu.contains(event.target)) {
        entry.closeMenu();
      }
    });
  });

  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") {
      return;
    }
    const openEntries = openEnhancedSelects();
    if (openEntries.length === 0) {
      return;
    }
    cleanupDisconnectedEnhancedSelects();
    openEntries.forEach((entry) => entry.closeMenu());
  });

  window.addEventListener("resize", () => {
    const openEntries = openEnhancedSelects();
    if (openEntries.length === 0) {
      return;
    }
    cleanupDisconnectedEnhancedSelects();
    openEntries.forEach((entry) => entry.positionMenu());
  });

  window.addEventListener(
    "scroll",
    () => {
      const openEntries = openEnhancedSelects();
      if (openEntries.length === 0) {
        return;
      }
      cleanupDisconnectedEnhancedSelects();
      openEntries.forEach((entry) => entry.positionMenu());
    },
    true
  );
}
