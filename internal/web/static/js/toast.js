export { createToastUI, showServerRenderedToasts };

function createToastUI() {
  const region = document.createElement("div");
  region.className = "toast-region";
  region.setAttribute("aria-live", "polite");
  region.setAttribute("aria-atomic", "false");
  document.body.appendChild(region);

  const dismiss = (toast) => {
    if (!toast || toast.dataset.dismissed === "true") {
      return;
    }
    toast.dataset.dismissed = "true";
    toast.classList.remove("is-visible");
    toast.classList.add("is-leaving");
    window.setTimeout(() => toast.remove(), 180);
  };

  return {
    show(options) {
      const message = String(options?.message || "").trim();
      if (!message) {
        return;
      }

      while (region.children.length >= 4) {
        const first = region.firstElementChild;
        if (!first) {
          break;
        }
        first.remove();
      }

      const tone = options?.tone || "info";
      const toast = document.createElement("div");
      toast.className = `toast-message toast-${tone}`;
      toast.setAttribute("role", tone === "error" ? "alert" : "status");

      const text = document.createElement("div");
      text.className = "toast-text";
      renderToastText(text, message);
      toast.appendChild(text);

      const close = document.createElement("button");
      close.type = "button";
      close.className = "toast-close";
      close.setAttribute("aria-label", "Close notification");
      close.setAttribute("title", "Close");
      const closeIcon = document.createElement("span");
      closeIcon.setAttribute("aria-hidden", "true");
      closeIcon.textContent = "×";
      close.appendChild(closeIcon);
      close.addEventListener("click", () => dismiss(toast));
      toast.appendChild(close);

      region.appendChild(toast);

      window.requestAnimationFrame(() => toast.classList.add("is-visible"));
      window.setTimeout(() => dismiss(toast), 5000);
    },
  };
}

function renderToastText(target, message) {
  const lines = String(message || "")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
  if (lines.length <= 1) {
    target.textContent = String(message || "");
    return;
  }
  lines.forEach((line, index) => {
    const row = document.createElement("span");
    row.className = "toast-line";
    if (index === 0) {
      row.classList.add("toast-line-summary");
    } else if (line.endsWith(":") || line.endsWith("\uff1a")) {
      row.classList.add("toast-line-heading");
    } else if (line.startsWith("- ")) {
      row.classList.add("toast-line-item");
      line = line.slice(2).trim();
    }
    row.textContent = line;
    target.appendChild(row);
  });
}

function showServerRenderedToasts(toastUI, root = document) {
  const clearParams = new Set();
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  scope.querySelectorAll("[data-clear-url-params]").forEach((element) => {
    (element.dataset.clearUrlParams || "")
      .split(",")
      .map((param) => param.trim())
      .filter(Boolean)
      .forEach((param) => clearParams.add(param));
  });

  scope.querySelectorAll(".system-notice, .notice").forEach((notice) => {
    const message = (notice.textContent || "").trim();
    if (!message) {
      return;
    }

    const tone = notice.classList.contains("tone-bad") || notice.classList.contains("error")
      ? "error"
      : notice.classList.contains("tone-good")
        ? "ok"
        : "info";
    toastUI.show({ message, tone });
  });
  if (scope === document) {
    clearURLParams(clearParams);
  }
}

function clearURLParams(params) {
  if (!params.size || !window.history || typeof window.history.replaceState !== "function") {
    return;
  }

  const url = new URL(window.location.href);
  let changed = false;
  params.forEach((param) => {
    if (url.searchParams.has(param)) {
      url.searchParams.delete(param);
      changed = true;
    }
  });
  if (!changed) {
    return;
  }

  const next = `${url.pathname}${url.search}${url.hash}`;
  window.history.replaceState(window.history.state, "", next);
}

