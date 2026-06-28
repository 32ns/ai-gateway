import { initSupportNotificationPermissionPrompt, maybeNotifySupportMessage, setSupportUnread } from "./support_notifications.js?v=2026061511";

export function initConsoleEvents(root = document) {
  if (root !== document || typeof window.EventSource !== "function") {
    return;
  }
  if (window.__agConsoleEventsReady) {
    return;
  }
  const hasConsoleUser = document.querySelector("[data-nav-messages-link], [data-current-balance], [data-support-root]");
  if (!hasConsoleUser) {
    return;
  }
  window.__agConsoleEventsReady = true;
  initSupportNotificationPermissionPrompt();

  let stateRefreshTimer = 0;
  let partialRefreshTimer = 0;
  const partialRefreshVersions = new Map();

  const setUnreadMessages = (count) => {
    const value = Math.max(0, Number(count || 0));
    document.querySelectorAll("[data-nav-messages-link]").forEach((link) => {
      if (!(link instanceof HTMLElement)) {
        return;
      }
      let badge = link.querySelector(".nav-badge");
      if (value <= 0) {
        badge?.remove();
        return;
      }
      if (!(badge instanceof HTMLElement)) {
        badge = document.createElement("span");
        badge.className = "nav-badge";
        link.appendChild(badge);
      }
      badge.textContent = String(value);
    });
  };

  const setBalance = (value) => {
    const text = String(value || "").trim();
    if (!text) {
      return;
    }
    document.querySelectorAll("[data-current-balance]").forEach((node) => {
      if (node instanceof HTMLElement) {
        node.textContent = text;
      }
    });
  };

  const refreshState = () => {
    if (stateRefreshTimer) {
      window.clearTimeout(stateRefreshTimer);
    }
    stateRefreshTimer = window.setTimeout(async () => {
      stateRefreshTimer = 0;
      try {
        const response = await fetch("/console/events/state", {
          method: "GET",
          credentials: "same-origin",
          cache: "no-store",
          headers: {
            Accept: "application/json",
            "X-Requested-With": "fetch",
          },
        });
        if (!response.ok) {
          return;
        }
        const payload = await response.json();
        setUnreadMessages(payload.unread_messages);
        setSupportUnread(payload.unread_support_messages);
        setBalance(payload.balance_display);
      } catch {
        // EventSource will reconnect on its own; state refresh can wait for the next event.
      }
    }, 150);
  };

  const replacePartialFromCurrentPage = async (partialName) => {
    const target = document.querySelector(`[data-partial="${CSS.escape(partialName)}"]`);
    if (!(target instanceof HTMLElement)) {
      return;
    }
    const refreshVersion = (partialRefreshVersions.get(partialName) || 0) + 1;
    partialRefreshVersions.set(partialName, refreshVersion);
    try {
      const response = await fetch(window.location.href, {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
        },
      });
      if (!response.ok) {
        return;
      }
      const html = await response.text();
      if (partialRefreshVersions.get(partialName) !== refreshVersion) {
        return;
      }
      const doc = new DOMParser().parseFromString(html, "text/html");
      const incoming = doc.querySelector(`[data-partial="${CSS.escape(partialName)}"]`);
      if (incoming instanceof HTMLElement) {
        target.replaceWith(incoming);
        incoming.dispatchEvent(new CustomEvent("ag:fragment-replaced", { bubbles: true }));
      }
    } catch {
      // Keep the current UI on transient refresh failures.
    }
  };

  const refreshCurrentPartials = (names) => {
    if (partialRefreshTimer) {
      window.clearTimeout(partialRefreshTimer);
    }
    partialRefreshTimer = window.setTimeout(() => {
      partialRefreshTimer = 0;
      names.forEach((name) => replacePartialFromCurrentPage(name));
    }, 250);
  };

  const parseEvent = (event) => {
    try {
      return JSON.parse(event.data || "{}");
    } catch {
      return {};
    }
  };

  const financeAutoRefreshIgnoredReasons = new Set(["usage_settled", "usage_released", "usage_account_updated"]);

  const source = new EventSource("/console/events");

  source.addEventListener("message.unread_count", (event) => {
    const payload = parseEvent(event).payload || {};
    setUnreadMessages(payload.count);
  });

  source.addEventListener("message.updated", () => {
    refreshState();
    if (document.querySelector('[data-partial="messages-page"]')) {
      refreshCurrentPartials(["messages-page"]);
    }
  });

  source.addEventListener("support.unread_count", (event) => {
    const payload = parseEvent(event).payload || {};
    setSupportUnread(payload.count);
  });

  source.addEventListener("support.message.created", (event) => {
    const payload = parseEvent(event).payload || {};
    const admin = document.querySelector("[data-nav-support-link]") instanceof HTMLElement;
    setSupportUnread(payload.unread);
    if (!document.querySelector("[data-support-root]")) {
      maybeNotifySupportMessage(payload, admin);
    }
    document.dispatchEvent(new CustomEvent("ag:support-message-created", { detail: payload }));
  });

  source.addEventListener("balance.updated", (event) => {
    const payload = parseEvent(event).payload || {};
    setBalance(payload.balance_display);
  });

  source.addEventListener("payment.updated", (event) => {
    const payload = parseEvent(event).payload || {};
    if (payload.balance_display) {
      setBalance(payload.balance_display);
    }
    if (document.querySelector('[data-partial="payments-list"]')) {
      refreshCurrentPartials(["payments-list"]);
    }
  });

  source.addEventListener("finance.changed", (event) => {
    const payload = parseEvent(event).payload || {};
    if (financeAutoRefreshIgnoredReasons.has(String(payload.reason || "").trim())) {
      return;
    }
    if (document.querySelector('[data-partial="finance-page"]')) {
      refreshCurrentPartials(["finance-page"]);
    }
  });

  source.addEventListener("usage_log.changed", () => {
    if (document.querySelector('[data-partial="usage-logs-page"]')) {
      refreshCurrentPartials(["usage-logs-page"]);
    }
  });

  source.addEventListener("audit.updated", () => {
    if (document.querySelector('[data-partial="audit-page"]')) {
      refreshCurrentPartials(["audit-page"]);
    }
  });

  source.addEventListener("account_batch.updated", (event) => {
    const payload = parseEvent(event).payload || {};
    document.dispatchEvent(new CustomEvent("ag:account-batch-updated", { detail: payload }));
  });

  source.addEventListener("account_pool.changed", () => {
    document.dispatchEvent(new CustomEvent("ag:account-pool-changed"));
  });

  source.addEventListener("image_job.updated", (event) => {
    const payload = parseEvent(event).payload || {};
    document.dispatchEvent(new CustomEvent("ag:image-job-updated", { detail: payload.job || payload }));
  });

  source.addEventListener("models.changed", () => {
    const partials = [];
    if (document.querySelector('[data-partial="models-list"]')) {
      partials.push("models-list");
    }
    if (document.querySelector('[data-partial="models-workspace"]')) {
      partials.push("models-workspace");
    }
    if (document.querySelector('[data-partial="user-models-page"]')) {
      partials.push("user-models-page");
    }
    if (partials.length > 0) {
      refreshCurrentPartials(partials);
    }
  });

  source.addEventListener("settings.updated", () => {
    refreshState();
  });

  source.addEventListener("error", () => {
    refreshState();
  });

  window.addEventListener("beforeunload", () => source.close());
  refreshState();
}
