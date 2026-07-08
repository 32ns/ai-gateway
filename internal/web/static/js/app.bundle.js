import { createConfirmUI, createPromptUI } from "./dialogs.js?v=2026060401";
import { createToastUI, showServerRenderedToasts } from "./toast.js?v=2026070401";
import { initPricingEditors } from "./pricing.js?v=2026051501";
import { initModelGroupPopovers } from "./model_groups.js?v=2026052003";
import { initCopySecrets } from "./clipboard.js?v=2026051501";
import { initEnhancedSelects } from "./selects.js?v=2026061702";
import { initAccountLoginForms, initEmailCodeSenders, initEmailProviderFields, initPasswordToggles, initRegistrationEmailDomains, initSettingsTabs } from "./forms.js?v=2026052801";
import { initImageLab } from "./image_lab.js?v=2026070802";

document.addEventListener("DOMContentLoaded", () => {
  const confirmUI = createConfirmUI();
  const promptUI = createPromptUI();
  const toastUI = createToastUI();
  let approvedSubmission = null;
  const preservedScrollKey = "ag:preserved-scroll";
  let accountDetailGlobalListenersReady = false;
  let userMenuGlobalListenersReady = false;
  let accountBatchPollTimer = 0;
  const accountBatchRefreshedItems = new Map();
  const accountBatchPendingRefreshItems = new Map();
  const accountBatchTerminalToasts = new Set();
  const accountBatchJobActions = new Set(["refresh_quota", "test", "move_group"]);
  const accountSelectionCache = new WeakMap();
  const accountBatchButtonCache = new WeakMap();
  const accountFilterInputCache = new WeakMap();
  const openAccountDetails = new Set();
  const openUserMenus = new Set();
  const autoSubmitControls = new WeakMap();
  let longRunningSubmission = null;
  const statusAutoRefreshTimers = new WeakMap();
  let monitorCountdownTimer = 0;

  const restorePreservedScroll = () => {
    let payload = null;
    try {
      payload = JSON.parse(window.sessionStorage?.getItem(preservedScrollKey) || "null");
      window.sessionStorage?.removeItem(preservedScrollKey);
    } catch {
      return;
    }
    if (!payload || payload.path !== window.location.pathname) {
      return;
    }
    if (Date.now() - Number(payload.createdAt || 0) > 60000) {
      return;
    }
    const x = Math.max(0, Number(payload.x || 0));
    const y = Math.max(0, Number(payload.y || 0));
    window.requestAnimationFrame(() => window.scrollTo(x, y));
  };

  const preserveScrollForSubmit = (form, submitter) => {
    const mode = (submitter?.dataset.preserveScroll || form.dataset.preserveScroll || "").trim();
    if (mode === "off") {
      return;
    }
    const method = (submitter?.getAttribute("formmethod") || form.getAttribute("method") || "get").toLowerCase();
    if (mode !== "on" && method === "get") {
      return;
    }
    try {
      window.sessionStorage?.setItem(
        preservedScrollKey,
        JSON.stringify({
          path: window.location.pathname,
          x: window.scrollX,
          y: window.scrollY,
          createdAt: Date.now(),
        })
      );
    } catch {
      // Ignore storage failures; the form submission should continue normally.
    }
  };

  const scopedElements = (root, selector) => {
    const out = [];
    if (root instanceof Element && root.matches(selector)) {
      out.push(root);
    }
    if (root && typeof root.querySelectorAll === "function") {
      out.push(...root.querySelectorAll(selector));
    }
    return out;
  };

  const associatedForm = (control) => {
    if (control?.form instanceof HTMLFormElement) {
      return control.form;
    }
    const form = control?.closest?.("form");
    if (form instanceof HTMLFormElement) {
      return form;
    }
    const formID = control?.getAttribute?.("form") || "";
    if (formID) {
      const linked = document.getElementById(formID);
      if (linked instanceof HTMLFormElement) {
        return linked;
      }
    }
    return null;
  };

  const controlLoadingState = new WeakMap();

  const startControlLoading = (control, options = {}) => {
    if (!(control instanceof HTMLElement)) {
      return;
    }
    const disable = options.disable !== false;
    if (!controlLoadingState.has(control)) {
      controlLoadingState.set(control, {
        disabled: control instanceof HTMLButtonElement || control instanceof HTMLInputElement ? control.disabled : null,
        ariaDisabled: control.getAttribute("aria-disabled"),
      });
    }
    control.classList.add("is-loading");
    control.setAttribute("aria-busy", "true");
    if (disable && (control instanceof HTMLButtonElement || control instanceof HTMLInputElement)) {
      control.disabled = true;
    } else if (disable && control instanceof HTMLAnchorElement) {
      control.setAttribute("aria-disabled", "true");
    }
  };

  const stopControlLoading = (control) => {
    if (!(control instanceof HTMLElement)) {
      return;
    }
    const state = controlLoadingState.get(control);
    controlLoadingState.delete(control);
    control.classList.remove("is-loading");
    control.removeAttribute("aria-busy");
    if (control instanceof HTMLButtonElement || control instanceof HTMLInputElement) {
      control.disabled = Boolean(state?.disabled);
    } else if (control instanceof HTMLAnchorElement) {
      if (state?.ariaDisabled === null || state?.ariaDisabled === undefined) {
        control.removeAttribute("aria-disabled");
      } else {
        control.setAttribute("aria-disabled", state.ariaDisabled);
      }
    }
  };

  const showMonitorRunPending = (control) => {
    if (!(control instanceof HTMLButtonElement) || !control.classList.contains("monitor-run-button")) {
      return;
    }
    if (control.dataset.monitorRunPending === "true") {
      return;
    }
    control.dataset.monitorRunPending = "true";
    control.classList.add("is-monitor-running");
    control.disabled = true;
    control.setAttribute("aria-busy", "true");
    control.innerHTML = `
      <span class="monitor-run-indicator" role="status" aria-live="polite">
        <span class="monitor-run-spinner" aria-hidden="true"></span>
        <span class="sr-only">${control.dataset.runningText || "Checking"}</span>
      </span>
    `;
  };

  const showLongRunningSubmitOverlay = (form, submitter) => {
    const message = (
      submitter?.dataset.longRunningMessage ||
      form.dataset.longRunningMessage ||
      document.body?.dataset.longRunningMessage ||
      "Processing, please wait..."
    ).trim();
    const detail = (
      submitter?.dataset.longRunningDetail ||
      form.dataset.longRunningDetail ||
      document.body?.dataset.longRunningDetail ||
      ""
    ).trim();
    let overlay = document.querySelector("[data-long-running-overlay]");
    if (!(overlay instanceof HTMLElement)) {
      overlay = document.createElement("div");
      overlay.className = "long-running-overlay";
      overlay.dataset.longRunningOverlay = "true";
      overlay.setAttribute("role", "status");
      overlay.setAttribute("aria-live", "polite");
      overlay.innerHTML = `
        <div class="long-running-dialog">
          <span class="long-running-spinner" aria-hidden="true"></span>
          <div>
            <strong data-long-running-title></strong>
            <p data-long-running-detail></p>
          </div>
        </div>
      `;
      document.body.appendChild(overlay);
    }
    const title = overlay.querySelector("[data-long-running-title]");
    const detailNode = overlay.querySelector("[data-long-running-detail]");
    if (title instanceof HTMLElement) {
      title.textContent = message;
    }
    if (detailNode instanceof HTMLElement) {
      detailNode.textContent = detail;
      detailNode.hidden = detail === "";
    }
    overlay.hidden = false;
    overlay.classList.add("is-visible");
    document.body.classList.add("long-running-open");
  };

  const beginLongRunningSubmit = (form, submitter, loadingControl) => {
    if (!(form instanceof HTMLFormElement)) {
      return false;
    }
    if (longRunningSubmission?.form === form) {
      return false;
    }
    longRunningSubmission = { form, submitter };
    startControlLoading(loadingControl, { disable: true });
    showLongRunningSubmitOverlay(form, submitter);
    preserveScrollForSubmit(form, submitter);
    return true;
  };

  const downloadFilenameFromDisposition = (disposition, fallback) => {
    const value = String(disposition || "");
    const utf8Match = value.match(/filename\*=UTF-8''([^;]+)/i);
    if (utf8Match) {
      try {
        return decodeURIComponent(utf8Match[1].trim().replace(/^"|"$/g, ""));
      } catch {
        return utf8Match[1].trim().replace(/^"|"$/g, "") || fallback;
      }
    }
    const quotedMatch = value.match(/filename="([^"]+)"/i);
    if (quotedMatch) {
      return quotedMatch[1].trim() || fallback;
    }
    const plainMatch = value.match(/filename=([^;]+)/i);
    return plainMatch ? plainMatch[1].trim().replace(/^"|"$/g, "") || fallback : fallback;
  };

  const submitDownloadForm = async (form, submitter, trigger = submitter) => {
    const method = formSubmitMethod(form, submitter);
    const url = method === "get" ? formGETURL(form, submitter) : formSubmitURL(form, submitter);
    const csrfToken = new FormData(form).get("csrf_token") || "";
    const { body, headers } = method === "get" ? { body: undefined, headers: {} } : formBody(form, submitter);
    startControlLoading(trigger, { disable: true });
    try {
      const response = await fetch(url, {
        method: method.toUpperCase(),
        credentials: "same-origin",
        headers: {
          "X-Requested-With": "fetch",
          "X-CSRF-Token": String(csrfToken),
          ...headers,
        },
        body,
      });
      if (!response.ok) {
        const payload = await readResponsePayload(response);
        throw new Error(payload.message || response.statusText);
      }
      const blob = await response.blob();
      const filename = downloadFilenameFromDisposition(
        response.headers.get("Content-Disposition"),
        submitter?.dataset.downloadFilename || form.dataset.downloadFilename || "download"
      );
      const objectURL = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = objectURL;
      link.download = filename;
      link.style.display = "none";
      document.body.appendChild(link);
      link.click();
      link.remove();
      window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
      return true;
    } catch (error) {
      toastUI.show({ message: error instanceof Error ? error.message : "Download failed", tone: "error" });
      return true;
    } finally {
      stopControlLoading(trigger);
    }
  };

  const readResponsePayload = async (response) => {
    const text = await response.text();
    if (!text) {
      return {};
    }
    try {
      return JSON.parse(text);
    } catch {
      return { message: text };
    }
  };

  const autoSubmitLoadingControl = (control) => {
    if (!(control instanceof HTMLElement)) {
      return null;
    }
    const buttonLike = control.closest(".button-link, button");
    return buttonLike instanceof HTMLElement ? buttonLike : control;
  };

  const initAutoSubmitControls = (root = document) => {
    scopedElements(root, "[data-auto-submit]").forEach((control) => {
      if (control.dataset.autoSubmitReady === "true") {
        return;
      }
      control.dataset.autoSubmitReady = "true";
      control.addEventListener("change", () => {
        const form = control instanceof HTMLInputElement ? control.form : control.closest("form");
        if (!(form instanceof HTMLFormElement)) {
          return;
        }
        if (control instanceof HTMLElement) {
          autoSubmitControls.set(form, autoSubmitLoadingControl(control));
        }
        if (typeof form.requestSubmit === "function") {
          form.requestSubmit();
          return;
        }
        const loadingControl = autoSubmitLoadingControl(control);
        if (form.hasAttribute("data-long-running-form")) {
          beginLongRunningSubmit(form, null, loadingControl);
          form.submit();
          return;
        }
        startControlLoading(loadingControl, { disable: false });
        preserveScrollForSubmit(form, null);
        form.submit();
      });
    });
  };

  const accountDetailPanel = (details) => {
    if (!(details instanceof HTMLElement)) {
      return null;
    }
    const panel = details.querySelector(".account-detail-panel");
    return panel instanceof HTMLElement ? panel : null;
  };

  const accountDetailSummary = (details) => {
    if (!(details instanceof HTMLElement)) {
      return null;
    }
    const summary = details.querySelector("summary");
    return summary instanceof HTMLElement ? summary : null;
  };

  const resetAccountDetailPanelPosition = (details) => {
    const panel = accountDetailPanel(details);
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

  const positionAccountDetailPanel = (details) => {
    if (!(details instanceof HTMLDetailsElement) || !details.open) {
      resetAccountDetailPanelPosition(details);
      return;
    }
    const summary = accountDetailSummary(details);
    const panel = accountDetailPanel(details);
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
    const availableHeight = Math.max(160, (openAbove ? spaceAbove : spaceBelow) - panelGap);
    const width = Math.min(380, Math.max(280, viewportWidth - viewportMargin * 2));
    const left = Math.min(
      Math.max(viewportMargin, rect.right - width),
      Math.max(viewportMargin, viewportWidth - width - viewportMargin)
    );

    panel.classList.add("is-fixed");
    panel.style.left = `${left}px`;
    panel.style.right = "auto";
    panel.style.width = `${width}px`;
    panel.style.maxHeight = `${Math.min(560, availableHeight)}px`;
    panel.style.zIndex = "65";
    panel.dataset.placement = openAbove ? "top" : "bottom";
    if (openAbove) {
      panel.style.top = "auto";
      panel.style.bottom = `${viewportHeight - rect.top + panelGap}px`;
    } else {
      panel.style.top = `${rect.bottom + panelGap}px`;
      panel.style.bottom = "auto";
    }
  };

  const closeAccountDetails = (except = null) => {
    openAccountDetails.forEach((details) => {
      if (!details.isConnected) {
        openAccountDetails.delete(details);
        return;
      }
      if (details !== except) {
        details.removeAttribute("open");
        resetAccountDetailPanelPosition(details);
        openAccountDetails.delete(details);
      }
    });
  };

  const initAccountDetails = (root = document) => {
    scopedElements(root, ".account-details").forEach((details) => {
      if (details.dataset.accountDetailsReady === "true") {
        if (details.open) {
          positionAccountDetailPanel(details);
        }
        return;
      }
      details.dataset.accountDetailsReady = "true";
      details.addEventListener("toggle", () => {
        if (details.open) {
          openAccountDetails.add(details);
          closeAccountDetails(details);
          positionAccountDetailPanel(details);
        } else {
          openAccountDetails.delete(details);
          resetAccountDetailPanelPosition(details);
        }
      });
      if (details.open) {
        openAccountDetails.add(details);
        positionAccountDetailPanel(details);
      }
    });
    if (accountDetailGlobalListenersReady) {
      return;
    }
    accountDetailGlobalListenersReady = true;
    window.addEventListener("resize", () => {
      openAccountDetails.forEach((details) => {
        if (details.isConnected) {
          positionAccountDetailPanel(details);
        } else {
          openAccountDetails.delete(details);
        }
      });
    });
    window.addEventListener(
      "scroll",
      () => {
        openAccountDetails.forEach((details) => {
          if (details.isConnected) {
            positionAccountDetailPanel(details);
          } else {
            openAccountDetails.delete(details);
          }
        });
      },
      true
    );
  };

  const renderUserDetailsPlaceholder = (body, message, tone = "") => {
    const placeholder = document.createElement("div");
    placeholder.className = "empty user-details-placeholder";
    placeholder.textContent = message || "";
    if (tone) {
      placeholder.dataset.tone = tone;
    }
    body.replaceChildren(placeholder);
  };

  const loadUserDetails = async (body) => {
    if (!(body instanceof HTMLElement)) {
      return;
    }
    if (body.dataset.userDetailsLoaded === "true" || body.dataset.userDetailsRequesting === "true") {
      return;
    }
    const url = (body.dataset.userDetailsUrl || "").trim();
    if (!url) {
      return;
    }
    const loadingMessage = body.dataset.userDetailsLoadingMessage || body.dataset.userDetailsLoading || "Loading user details...";
    const errorMessage = body.dataset.userDetailsError || "Unable to load user details.";
    body.dataset.userDetailsRequesting = "true";
    body.setAttribute("aria-busy", "true");
    renderUserDetailsPlaceholder(body, loadingMessage);
    try {
      const response = await fetch(url, {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
        },
      });
      const html = await response.text();
      if (!response.ok) {
        throw new Error(html.trim() || response.statusText);
      }
      const template = document.createElement("template");
      template.innerHTML = html.trim();
      if (!template.content.childElementCount) {
        throw new Error(errorMessage);
      }
      body.replaceChildren(template.content.cloneNode(true));
      body.dataset.userDetailsLoaded = "true";
      enhanceFragment(body);
    } catch (error) {
      renderUserDetailsPlaceholder(body, errorMessage, "bad");
      toastUI.show({ message: errorMessage, tone: "error" });
    } finally {
      delete body.dataset.userDetailsRequesting;
      body.removeAttribute("aria-busy");
    }
  };

  const initUserDetailsLazyLoad = (root = document) => {
    scopedElements(root, "[data-user-details-body]").forEach((body) => {
      if (!(body instanceof HTMLElement) || body.dataset.userDetailsReady === "true") {
        return;
      }
      const modal = body.closest(".settings-overlay");
      if (!(modal instanceof HTMLElement)) {
        return;
      }
      body.dataset.userDetailsReady = "true";
      modal.addEventListener("modal:open", () => loadUserDetails(body));
      if (!modal.hidden || modal.classList.contains("is-visible")) {
        loadUserDetails(body);
      }
    });
  };

  const deferredIncomingTarget = (doc, target) => {
    const partial = target.getAttribute("data-partial");
    if (partial) {
      return doc.querySelector(`[data-partial="${CSS.escape(partial)}"]`);
    }
    if (target.id) {
      return doc.getElementById(target.id);
    }
    return null;
  };

  const loadDeferredPartial = async (target) => {
    if (!(target instanceof HTMLElement)) {
      return;
    }
    if (target.dataset.deferredLoaded === "true" || target.dataset.deferredRequesting === "true") {
      return;
    }
    const url = (target.dataset.deferredUrl || "").trim();
    if (!url) {
      return;
    }
    const partial = (target.getAttribute("data-partial") || "").trim();
    const errorMessage = target.dataset.deferredError || "Unable to load content.";
    target.dataset.deferredRequesting = "true";
    target.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(url, {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
          ...(partial ? { "X-Ajax-Partial": partial } : {}),
        },
      });
      const html = await response.text();
      if (!response.ok) {
        throw new Error(html.trim() || response.statusText);
      }
      const doc = new DOMParser().parseFromString(html, "text/html");
      const incoming = deferredIncomingTarget(doc, target);
      if (!(incoming instanceof HTMLElement)) {
        throw new Error(errorMessage);
      }
      const clone = incoming.cloneNode(true);
      target.replaceWith(clone);
      if (clone instanceof HTMLElement) {
        clone.dataset.deferredLoaded = "true";
        enhanceFragment(clone);
      }
    } catch (error) {
      target.dataset.deferredErrorState = "true";
      toastUI.show({ message: errorMessage, tone: "error" });
    } finally {
      delete target.dataset.deferredRequesting;
      target.removeAttribute("aria-busy");
    }
  };

  const initDeferredPartials = (root = document) => {
    scopedElements(root, "[data-deferred-url]").forEach((target) => {
      if (!(target instanceof HTMLElement) || target.dataset.deferredReady === "true") {
        return;
      }
      target.dataset.deferredReady = "true";
      loadDeferredPartial(target);
    });
  };

  const initAccountFilters = (root = document) => {
    scopedElements(root, "[data-account-filter-select]").forEach((select) => {
      if (!(select instanceof HTMLSelectElement) || select.dataset.accountFilterReady === "true") {
        return;
      }
      select.dataset.accountFilterReady = "true";
      const panel = select.closest(".panel");
      if (!(panel instanceof HTMLElement)) {
        return;
      }
      const syncAccountFilter = () => {
        const items = accountSelectionItems(panel);
        const empty = panel.querySelector("[data-account-filter-empty]");
        const value = select.value || "all";
        accountFilterInputs(panel).forEach((input) => {
          input.value = value;
        });
        let visibleCount = 0;
        items.forEach(({ card, checkbox }) => {
          const show =
            value === "all" ||
            (value === "disabled"
              ? card.dataset.accountControlFilter === "disabled"
              : value === "cooling"
                ? card.dataset.accountFilter === "cooling" ||
                  card.dataset.accountFilter === "time_limit" ||
                  card.dataset.accountFilter === "week_limit"
              : card.dataset.accountFilter === value);
          card.hidden = !show;
          if (!show) {
            checkbox.checked = false;
          }
          if (show) {
            visibleCount += 1;
          }
        });
        if (empty instanceof HTMLElement) {
          empty.hidden = value === "all" || visibleCount > 0 || items.length === 0;
        }
        panel.dispatchEvent(new CustomEvent("account-filter:change", { detail: { items } }));
      };
      syncAccountFilter();
      select.addEventListener("change", syncAccountFilter);
    });
  };

  const accountSelectionItems = (panel) => {
    const cached = accountSelectionCache.get(panel);
    if (cached) {
      return cached;
    }
    const items = Array.from(panel.querySelectorAll("[data-account-card]")).reduce((out, card) => {
      if (!(card instanceof HTMLElement)) {
        return out;
      }
      const checkbox = card.querySelector("[data-account-select]");
      if (checkbox instanceof HTMLInputElement) {
        out.push({ card, checkbox });
      }
      return out;
    }, []);
    accountSelectionCache.set(panel, items);
    return items;
  };

  const accountBatchButtons = (panel) => {
    const cached = accountBatchButtonCache.get(panel);
    if (cached) {
      return cached;
    }
    const buttons = Array.from(panel.querySelectorAll("[data-account-batch-action], [data-account-batch-export-selected]"))
      .filter((button) => button instanceof HTMLButtonElement);
    accountBatchButtonCache.set(panel, buttons);
    return buttons;
  };

  const accountFilterInputs = (panel) => {
    const cached = accountFilterInputCache.get(panel);
    if (cached) {
      return cached;
    }
    const inputs = Array.from(panel.querySelectorAll("[data-account-current-filter]"))
      .filter((input) => input instanceof HTMLInputElement);
    accountFilterInputCache.set(panel, inputs);
    return inputs;
  };

  const initAccountBatchForms = (root = document) => {
    scopedElements(root, "[data-account-batch-form]").forEach((form) => {
      if (!(form instanceof HTMLFormElement) || form.dataset.accountBatchReady === "true") {
        return;
      }
      form.dataset.accountBatchReady = "true";
      const panel = form.closest(".panel");
      if (!(panel instanceof HTMLElement)) {
        return;
      }
      const selectedAccountCount = () => accountSelectionItems(panel).reduce((count, item) => count + (item.checkbox.checked ? 1 : 0), 0);
      const batchJobRunning = () => {
        const jobPanel = panel.querySelector("[data-account-batch-job]");
        return jobPanel instanceof HTMLElement && jobPanel.dataset.running === "true";
      };
      const syncBatchSelection = (items = accountSelectionItems(panel)) => {
        let visibleCount = 0;
        let selectedCount = 0;
        items.forEach(({ card, checkbox }) => {
          if (checkbox.checked) {
            selectedCount += 1;
          }
          card.classList.toggle("is-selected", checkbox.checked);
          if (!card.hidden) {
            visibleCount += 1;
          }
        });
        form.dataset.selectedCount = String(selectedCount);
        const summary = form.querySelector("[data-account-batch-summary]");
        if (summary instanceof HTMLElement) {
          const template = summary.dataset.summaryTemplate || "%selected% selected / %visible% visible";
          summary.textContent = template
            .replaceAll("%selected%", String(selectedCount))
            .replaceAll("%visible%", String(visibleCount));
        }
        accountBatchButtons(panel).forEach((button) => {
          button.disabled = selectedCount === 0 || batchJobRunning();
        });
      };
      form.querySelector("[data-account-select-visible]")?.addEventListener("click", () => {
        const items = accountSelectionItems(panel);
        items.forEach(({ card, checkbox }) => {
          if (!card.hidden) {
            checkbox.checked = true;
          }
        });
        syncBatchSelection(items);
      });
      form.querySelector("[data-account-clear-visible]")?.addEventListener("click", () => {
        const items = accountSelectionItems(panel);
        items.forEach(({ card, checkbox }) => {
          if (!card.hidden) {
            checkbox.checked = false;
          }
        });
        syncBatchSelection(items);
      });
      panel.addEventListener("change", (event) => {
        if (event.target instanceof HTMLInputElement && event.target.matches("[data-account-select]")) {
          syncBatchSelection();
        }
      });
      panel.addEventListener("account-filter:change", (event) => {
        syncBatchSelection(Array.isArray(event.detail?.items) ? event.detail.items : undefined);
      });
      form.addEventListener("submit", (event) => {
        const selectedCount = selectedAccountCount();
        if (selectedCount === 0) {
          event.preventDefault();
          event.stopPropagation();
          toastUI.show({ message: form.dataset.noSelectionMessage || "Select at least one account first.", tone: "error" });
          return;
        }
        const submitter = event.submitter instanceof HTMLButtonElement ? event.submitter : null;
        if (submitter && accountBatchJobActions.has(submitter.value || "")) {
          event.preventDefault();
          event.stopPropagation();
          startAccountBatchJob(form, submitter);
          return;
        }
        if (submitter?.value === "delete") {
          const template = submitter.dataset.confirmTemplate || submitter.dataset.confirm || "";
          if (template) {
            submitter.dataset.confirm = template.replaceAll("%selected%", String(selectedCount));
          }
        }
      });
      syncBatchSelection();
    });
  };

  const groupModalState = new WeakMap();

  const getGroupModalState = (modal) => {
    let state = groupModalState.get(modal);
    if (!state) {
      state = {
        previousFocus: null,
        pointerStartedOnOverlay: false,
      };
      groupModalState.set(modal, state);
    }
    return state;
  };

  const initGroupSettingModals = (root = document) => {
    scopedElements(root, "[data-group-settings-open]").forEach((button) => {
      if (button.dataset.groupSettingsReady === "true") {
        return;
      }
      const targetID = button.dataset.groupSettingsOpen || "";
      const modal = targetID ? document.getElementById(targetID) : null;
      if (!(modal instanceof HTMLElement)) {
        return;
      }
      button.dataset.groupSettingsReady = "true";
      const state = getGroupModalState(modal);
      const focusableElements = () =>
        Array.from(
          modal.querySelectorAll(
            'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href]'
          )
        ).filter((element) => element instanceof HTMLElement && element.offsetParent !== null);
      const close = () => {
        modal.classList.remove("is-visible");
        modal.hidden = true;
        document.body.classList.remove("settings-open");
        modal.dispatchEvent(new CustomEvent("modal:close"));
        if (state.previousFocus instanceof HTMLElement && document.contains(state.previousFocus)) {
          state.previousFocus.focus();
        }
        state.previousFocus = null;
      };
      const open = () => {
        closeUserMenus();
        state.previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
        modal.hidden = false;
        modal.classList.add("is-visible");
        document.body.classList.add("settings-open");
        modal.dispatchEvent(new CustomEvent("modal:open"));
        const preferredFocus = modal.querySelector("[data-modal-autofocus]");
        window.setTimeout(() => {
          if (preferredFocus instanceof HTMLElement && !preferredFocus.hidden) {
            preferredFocus.focus();
            if (preferredFocus instanceof HTMLInputElement) {
              preferredFocus.select();
            }
            return;
          }
          (focusableElements()[0] || modal).focus();
        }, 0);
      };
      button.addEventListener("click", open);
      if (modal.dataset.groupSettingsModalReady !== "true") {
        modal.dataset.groupSettingsModalReady = "true";
        const closeButtons = modal.querySelectorAll("[data-group-settings-close]");
        closeButtons.forEach((closeButton) => closeButton.addEventListener("click", close));
        modal.addEventListener("pointerdown", (event) => {
          state.pointerStartedOnOverlay = event.target === modal;
        });
        modal.addEventListener("click", (event) => {
          if (event.target === modal && state.pointerStartedOnOverlay) {
            close();
          }
          state.pointerStartedOnOverlay = false;
        });
        modal.addEventListener("keydown", (event) => {
          if (event.key === "Escape") {
            event.preventDefault();
            close();
            return;
          }
          if (event.key !== "Tab") {
            return;
          }
          const focusable = focusableElements();
          if (focusable.length === 0) {
            event.preventDefault();
            return;
          }
          const first = focusable[0];
          const last = focusable[focusable.length - 1];
          if (event.shiftKey && document.activeElement === first) {
            event.preventDefault();
            last.focus();
            return;
          }
          if (!event.shiftKey && document.activeElement === last) {
            event.preventDefault();
            first.focus();
          }
        });
      }
    });
  };

  const initPaymentCancelOrders = (root = document) => {
    scopedElements(root, "[data-payment-cancel-order]").forEach((modal) => {
      if (!(modal instanceof HTMLElement) || modal.dataset.paymentCancelReady === "true") {
        return;
      }
      modal.dataset.paymentCancelReady = "true";
      let cancelled = false;
      modal.addEventListener("modal:close", async () => {
        const orderID = modal.dataset.paymentCancelOrder || "";
        if (!orderID || cancelled) {
          return;
        }
        cancelled = true;
        try {
          const response = await fetch("/payments/cancel", {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "Content-Type": "application/json",
              "X-CSRF-Token": modal.dataset.paymentCancelCsrf || "",
            },
            body: JSON.stringify({ id: orderID }),
            keepalive: true,
          });
          const payload = await response.json().catch(() => ({}));
          if (payload.deleted === true) {
            modal.closest("tr")?.remove();
          } else if (payload.order_status === "paid") {
            window.location.reload();
          }
        } catch {
          cancelled = false;
        }
      });
    });
  };

  const initPaymentOrderQRModals = (root = document) => {
    scopedElements(root, "[data-payment-order-modal]").forEach((modal) => {
      if (!(modal instanceof HTMLElement) || modal.dataset.paymentOrderQRReady === "true") {
        return;
      }
      modal.dataset.paymentOrderQRReady = "true";
      const orderID = modal.dataset.paymentOrderId || modal.dataset.paymentCancelOrder || "";
      const qrNode = modal.querySelector("[data-payment-order-qr]");
      const statusText = modal.querySelector("[data-payment-status-text]");
      let timer = 0;
      let qrVersion = "";

      const stop = () => {
        if (timer) {
          window.clearInterval(timer);
          timer = 0;
        }
      };
      const setStatusText = (text) => {
        if (!(statusText instanceof HTMLElement)) {
          return;
        }
        statusText.textContent = text;
        statusText.hidden = text.trim() === "";
      };
      const updateQRCode = (version) => {
        if (!(qrNode instanceof HTMLImageElement)) {
          return;
        }
        const versionQuery = version ? `&v=${encodeURIComponent(version)}` : `&t=${Date.now()}`;
        qrNode.src = `/payments/qr?id=${encodeURIComponent(orderID)}${versionQuery}`;
        qrNode.hidden = false;
        qrVersion = version;
      };
      const tick = async () => {
        if (!orderID) {
          stop();
          return;
        }
        try {
          const response = await fetch("/payments/refresh", {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "Content-Type": "application/json",
              "X-CSRF-Token": modal.dataset.paymentCancelCsrf || "",
              "X-Requested-With": "fetch",
            },
            body: JSON.stringify({ id: orderID }),
          });
          const payload = await readResponsePayload(response);
          if (!response.ok || payload.status !== "ok") {
            return;
          }
          const providerStatus = String(payload.provider_status || "");
          const orderStatus = String(payload.order_status || "");
          if (payload.has_code_url === true) {
            const nextVersion = String(payload.code_version || "");
            if (!qrVersion || nextVersion !== qrVersion) {
              updateQRCode(nextVersion);
            }
            if (providerStatus === "paying") {
              setStatusText(statusText instanceof HTMLElement ? statusText.dataset.payingText || "Payment in progress" : "");
            } else if (orderStatus === "pending") {
              setStatusText(statusText instanceof HTMLElement ? statusText.dataset.waitingText || "" : "");
            }
          }
          if (payload.paid === true || orderStatus === "paid") {
            stop();
            setStatusText(statusText instanceof HTMLElement ? statusText.dataset.successText || "Payment successful" : "");
            window.setTimeout(() => window.location.reload(), 900);
            return;
          }
          if (orderStatus === "closed" || orderStatus === "failed") {
            stop();
            setStatusText(
              statusText instanceof HTMLElement && providerStatus === "canceled"
                ? statusText.dataset.canceledText || "Payment canceled"
                : statusText instanceof HTMLElement
                  ? statusText.dataset.expiredText || "QR code expired"
                  : ""
            );
            if (qrNode instanceof HTMLImageElement) {
              qrNode.hidden = true;
              qrNode.removeAttribute("src");
            }
          }
        } catch {
          // Keep the existing QR visible on transient network failures.
        }
      };
      modal.addEventListener("modal:open", () => {
        stop();
        qrVersion = "";
        tick();
        timer = window.setInterval(tick, 1000);
      });
      modal.addEventListener("modal:close", stop);
    });
  };

  const initMessageTargetFields = (root = document) => {
    scopedElements(root, "[data-message-target]").forEach((targetRoot) => {
      if (!(targetRoot instanceof HTMLElement) || targetRoot.dataset.messageTargetReady === "true") {
        return;
      }
      targetRoot.dataset.messageTargetReady = "true";
      const modeSelect = targetRoot.querySelector("[data-message-target-mode]");
      const panels = Array.from(targetRoot.querySelectorAll("[data-message-target-panel]")).filter(
        (panel) => panel instanceof HTMLElement
      );
      const syncMode = () => {
        const mode = modeSelect instanceof HTMLSelectElement ? modeSelect.value : "all";
        panels.forEach((panel) => {
          panel.hidden = panel.dataset.messageTargetPanel !== mode;
        });
        const form = targetRoot.closest("form");
        const popupRow = form?.querySelector("[data-message-popup-row]");
        const popup = form?.querySelector('input[name="popup"]');
        const isWebsiteMessage = mode === "website";
        if (popupRow instanceof HTMLElement) {
          popupRow.hidden = isWebsiteMessage;
        }
        if (popup instanceof HTMLInputElement) {
          popup.disabled = isWebsiteMessage;
          if (isWebsiteMessage) {
            popup.checked = false;
          }
        }
      };
      syncMode();
      modeSelect?.addEventListener("change", syncMode);

      const search = targetRoot.querySelector("[data-message-user-search]");
      const addButton = targetRoot.querySelector("[data-message-user-add]");
      const selected = targetRoot.querySelector("[data-message-selected-users]");
      const results = targetRoot.querySelector("[data-message-user-results]");
      let searchTimer = 0;
      let searchRequest = null;
      let searchSeq = 0;
      let currentResults = [];
      const options = () =>
        Array.from(targetRoot.querySelectorAll("[data-message-user-option]")).filter(
          (option) => option instanceof HTMLElement
        );
      const hasSelectedUser = (userID) =>
        selected instanceof HTMLElement &&
        selected.querySelector(`[data-user-id="${CSS.escape(userID)}"]`) instanceof HTMLElement;
      const selectedUserElement = (userID, username) => {
        const chip = document.createElement("span");
        chip.className = "message-selected-user";
        chip.dataset.userId = userID;
        const hidden = document.createElement("input");
        hidden.type = "hidden";
        hidden.name = selected instanceof HTMLElement ? selected.dataset.userInputName || "target_user_id" : "target_user_id";
        hidden.value = userID;
        const label = document.createElement("span");
        label.textContent = username;
        const remove = document.createElement("button");
        remove.type = "button";
        remove.textContent = "×";
        remove.setAttribute("aria-label", "remove");
        remove.addEventListener("click", () => chip.remove());
        chip.append(hidden, label, remove);
        return chip;
      };
      const hideResults = () => {
        if (results instanceof HTMLElement) {
          results.hidden = true;
          results.replaceChildren();
        }
      };
      const addUserOption = (userID, username) => {
        if (!userID) {
          return;
        }
        const existing = options().find((option) => (option.dataset.userId || "").trim() === userID);
        if (existing instanceof HTMLElement) {
          existing.dataset.userName = username || userID;
          return;
        }
        const option = document.createElement("span");
        option.dataset.messageUserOption = "";
        option.dataset.userId = userID;
        option.dataset.userName = username || userID;
        option.hidden = true;
        targetRoot.append(option);
      };
      const selectUser = (userID, username) => {
        if (!(selected instanceof HTMLElement) || !userID || hasSelectedUser(userID)) {
          hideResults();
          if (search instanceof HTMLInputElement) {
            search.value = "";
            search.focus();
          }
          return;
        }
        addUserOption(userID, username);
        selected.append(selectedUserElement(userID, username || userID));
        hideResults();
        if (search instanceof HTMLInputElement) {
          search.value = "";
          search.focus();
        }
      };
      const renderResults = (users) => {
        if (!(results instanceof HTMLElement)) {
          return;
        }
        currentResults = Array.isArray(users) ? users : [];
        results.replaceChildren();
        currentResults.forEach((user) => {
          const userID = String(user?.id || "").trim();
          const username = String(user?.username || userID).trim();
          if (!userID || hasSelectedUser(userID)) {
            return;
          }
          const button = document.createElement("button");
          button.type = "button";
          button.className = "message-user-result";
          button.dataset.userId = userID;
          button.dataset.userName = username;
          const name = document.createElement("span");
          name.textContent = username;
          const id = document.createElement("small");
          id.textContent = userID;
          button.append(name, id);
          button.addEventListener("click", () => selectUser(userID, username));
          results.append(button);
        });
        results.hidden = results.childElementCount === 0;
      };
      const runSearch = async () => {
        if (!(search instanceof HTMLInputElement)) {
          return;
        }
        const endpoint = search.dataset.messageUserSearchUrl || "";
        const query = search.value.trim();
        if (!endpoint || query.length < 2) {
          currentResults = [];
          hideResults();
          return;
        }
        searchSeq += 1;
        const seq = searchSeq;
        if (searchRequest) {
          searchRequest.abort();
        }
        searchRequest = new AbortController();
        try {
          const url = new URL(endpoint, window.location.origin);
          url.searchParams.set("q", query);
          const response = await fetch(url, {
            headers: { Accept: "application/json" },
            signal: searchRequest.signal
          });
          if (!response.ok || seq !== searchSeq) {
            return;
          }
          const payload = await response.json();
          renderResults(Array.isArray(payload.users) ? payload.users : []);
        } catch (error) {
          if (error?.name !== "AbortError") {
            currentResults = [];
            hideResults();
          }
        }
      };
      const addSelectedUser = () => {
        if (!(search instanceof HTMLInputElement) || !(selected instanceof HTMLElement)) {
          return;
        }
        const rawQuery = search.value.trim();
        if (!rawQuery) {
          return;
        }
        const query = rawQuery.toLowerCase();
        const match = options().find((option) => {
          const username = (option.dataset.userName || "").trim().toLowerCase();
          const userID = (option.dataset.userId || "").trim().toLowerCase();
          return username === query || userID === query;
        }) || currentResults.find((user) => {
          const userID = String(user?.id || "").trim().toLowerCase();
          const username = String(user?.username || "").trim().toLowerCase();
          return username === query || userID === query;
        });
        const userID = match instanceof HTMLElement ? (match.dataset.userId || "").trim() : String(match?.id || rawQuery).trim();
        const username = match instanceof HTMLElement ? (match.dataset.userName || userID).trim() : String(match?.username || userID).trim();
        selectUser(userID, username);
      };
      addButton?.addEventListener("click", addSelectedUser);
      search?.addEventListener("input", () => {
        window.clearTimeout(searchTimer);
        searchTimer = window.setTimeout(runSearch, 180);
      });
      search?.addEventListener("blur", () => {
        window.setTimeout(hideResults, 160);
      });
      search?.addEventListener("keydown", (event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          addSelectedUser();
        } else if (event.key === "Escape") {
          hideResults();
        }
      });
      selected?.querySelectorAll("[data-message-user-remove]").forEach((button) => {
        button.addEventListener("click", () => button.closest(".message-selected-user")?.remove());
      });
    });
  };

  const initSiteMessagePopups = (root = document) => {
    scopedElements(root, "[data-site-message-popup-overlay]").forEach((overlay) => {
      if (!(overlay instanceof HTMLElement) || overlay.dataset.siteMessagePopupReady === "true") {
        return;
      }
      overlay.dataset.siteMessagePopupReady = "true";
      const title = overlay.querySelector("[data-site-message-popup-title]");
      const body = overlay.querySelector("[data-site-message-popup-body]");
      const time = overlay.querySelector("[data-site-message-popup-time]");
      const form = overlay.querySelector("[data-site-message-popup-form]");
      const itemsRoot = overlay.querySelector("[data-site-message-popup-items]");
      const browserReadPrefix = "ag:site-message-popup-read:";
      const browserMessageRead = (item) => {
        const key = (item.dataset.browserReadKey || "").trim();
        if (!key) {
          return false;
        }
        try {
          return window.localStorage?.getItem(browserReadPrefix + key) === "1";
        } catch {
          return false;
        }
      };
      const markBrowserMessageRead = (item) => {
        const key = (item.dataset.browserReadKey || "").trim();
        if (!key) {
          return;
        }
        try {
          window.localStorage?.setItem(browserReadPrefix + key, "1");
        } catch {
          // Browser storage can be unavailable in private or restricted contexts.
        }
      };
      const items = Array.from(itemsRoot?.querySelectorAll("[data-site-message-popup-item]") || []).filter(
        (item) => item instanceof HTMLElement && !browserMessageRead(item)
      );
      let index = 0;
      let activeItem = null;

      const close = () => {
        overlay.classList.remove("is-visible");
        overlay.hidden = true;
        overlay.dispatchEvent(new CustomEvent("modal:close"));
        if (!document.querySelector(".settings-overlay.is-visible")) {
          document.body.classList.remove("settings-open");
        }
      };

      const openCurrent = () => {
        activeItem = items[index] instanceof HTMLElement ? items[index] : null;
        if (!(activeItem instanceof HTMLElement)) {
          close();
          return;
        }
        const itemTitle = activeItem.querySelector("h3");
        const itemTime = activeItem.querySelector("[data-site-message-popup-item-time]");
        const itemBody = activeItem.querySelector("[data-site-message-popup-item-body]");
        if (title instanceof HTMLElement) {
          title.textContent = (itemTitle?.textContent || "").trim();
        }
        if (time instanceof HTMLTimeElement) {
          const timeText = (itemTime?.textContent || "").trim();
          time.textContent = timeText;
          time.dateTime = itemTime instanceof HTMLTimeElement ? itemTime.dateTime : "";
          time.hidden = !timeText;
        }
        if (body instanceof HTMLElement) {
          body.replaceChildren(...Array.from(itemBody?.childNodes || []).map((node) => node.cloneNode(true)));
        }
        if (form instanceof HTMLFormElement) {
          form.action = activeItem.dataset.readUrl || "";
        }
        overlay.hidden = false;
        overlay.classList.add("is-visible");
        document.body.classList.add("settings-open");
        window.setTimeout(() => {
          const submit = form instanceof HTMLFormElement ? form.querySelector('button[type="submit"]') : null;
          if (submit instanceof HTMLButtonElement) {
            submit.focus();
          }
        }, 0);
      };

      const showNext = () => {
        index += 1;
        openCurrent();
      };

      overlay.querySelectorAll("[data-site-message-popup-dismiss]").forEach((button) => {
        button.addEventListener("click", () => close());
      });
      overlay.addEventListener("keydown", (event) => {
        if (event.key === "Escape") {
          event.preventDefault();
          close();
        }
      });
      if (form instanceof HTMLFormElement) {
        form.addEventListener("submit", async (event) => {
          event.preventDefault();
          const readMode = (activeItem?.dataset.readMode || "").trim();
          if (readMode === "browser") {
            markBrowserMessageRead(activeItem);
            activeItem?.remove();
            showNext();
            return;
          }
          const action = form.action || activeItem?.dataset.readUrl || "";
          if (!action) {
            close();
            return;
          }
          const submit = form.querySelector('button[type="submit"]');
          if (submit instanceof HTMLButtonElement) {
            submit.disabled = true;
          }
          try {
            const response = await fetch(action, {
              method: "POST",
              credentials: "same-origin",
              headers: {
                Accept: "text/html",
                "Content-Type": "application/x-www-form-urlencoded",
              },
              body: new URLSearchParams(new FormData(form)),
            });
            if (!response.ok) {
              throw new Error(response.statusText || "Request failed");
            }
            activeItem?.remove();
            showNext();
          } catch (error) {
            toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
          } finally {
            if (submit instanceof HTMLButtonElement) {
              submit.disabled = false;
            }
          }
        });
      }

      if (items.length > 0) {
        window.setTimeout(openCurrent, 120);
      }
    });
  };

  const initProxyTestButtons = (root = document) => {
    scopedElements(root, "[data-proxy-test]").forEach((button) => {
      if (button.dataset.proxyTestReady === "true") {
        return;
      }
      button.dataset.proxyTestReady = "true";
      button.addEventListener("click", async () => {
        const form = associatedForm(button);
        if (!(form instanceof HTMLFormElement)) {
          return;
        }
        const proxyField = button.closest(".proxy-field") || form;
        const input = proxyField.querySelector("[data-proxy-test-input]");
        const csrfInput = form.querySelector('input[name="csrf_token"]');
        const result = proxyField.querySelector("[data-proxy-test-result]");
        if (!(input instanceof HTMLInputElement) || !(csrfInput instanceof HTMLInputElement)) {
          return;
        }
        const proxyURL = input.value.trim();
        const runningText = button.dataset.proxyTestRunning || "Testing proxy...";
        const successText = button.dataset.proxyTestSuccess || "Proxy available";
        const failedText = button.dataset.proxyTestFailed || "Proxy unavailable";
        const endpoint = button.dataset.proxyTestUrl || "/admin/proxy-test";
        const setResult = (message, tone) => {
          if (!(result instanceof HTMLElement)) {
            return;
          }
          result.textContent = message;
          result.classList.remove("is-pending", "is-ok", "is-error", "tone-good", "tone-bad");
          if (tone === "pending") {
            result.classList.add("is-pending");
          } else if (tone === "ok") {
            result.classList.add("is-ok", "tone-good");
          } else if (tone === "error") {
            result.classList.add("is-error", "tone-bad");
          }
        };

        button.disabled = true;
        setResult(runningText, "pending");
        toastUI.show({ message: runningText, tone: "pending" });

        const body = new URLSearchParams();
        body.set("proxy_url", proxyURL);
        body.set("csrf_token", csrfInput.value);

        try {
          const response = await fetch(endpoint, {
            method: "POST",
            credentials: "same-origin",
            headers: {
              "Content-Type": "application/x-www-form-urlencoded",
              "X-CSRF-Token": csrfInput.value,
            },
            body,
          });
          const payload = await response.json();
          if (!response.ok || !payload.ok) {
            throw new Error(payload.message || failedText);
          }
          const details = [];
          if (typeof payload.status_code === "number") {
            details.push(`HTTP ${payload.status_code}`);
          }
          if (typeof payload.duration_ms === "number") {
            details.push(`${payload.duration_ms}ms`);
          }
          const message = details.length > 0 ? `${successText} · ${details.join(" · ")}` : successText;
          setResult(message, "ok");
          toastUI.show({ message, tone: "ok" });
        } catch (error) {
          const message = `${failedText}: ${error.message || error}`;
          setResult(message, "error");
          toastUI.show({ message, tone: "error" });
        } finally {
          button.disabled = false;
        }
      });
    });
  };

  const initEmailTestButtons = (root = document) => {
    scopedElements(root, "[data-email-test]").forEach((button) => {
      if (button.dataset.emailTestReady === "true") {
        return;
      }
      button.dataset.emailTestReady = "true";
      button.addEventListener("click", async () => {
        const form = associatedForm(button);
        if (!(form instanceof HTMLFormElement)) {
          return;
        }
        const testField = button.closest(".proxy-field") || form;
        const csrfInput = form.querySelector('input[name="csrf_token"]');
        const testInput = testField.querySelector("[data-email-test-input]");
        const result = testField.querySelector("[data-email-test-result]");
        if (!(csrfInput instanceof HTMLInputElement)) {
          return;
        }

        const runningText = button.dataset.emailTestRunning || "Sending test email...";
        const successText = button.dataset.emailTestSuccess || "Test email sent";
        const failedText = button.dataset.emailTestFailed || "Test email failed";
        const endpoint = button.dataset.emailTestUrl || "/admin/email-test";

        button.disabled = true;
        if (result instanceof HTMLElement) {
          result.textContent = runningText;
          result.classList.remove("is-ok", "is-error");
          result.classList.add("is-pending");
        }
        toastUI.show({ message: runningText, tone: "pending" });

        try {
          const formData = new FormData(form);
          if (testInput instanceof HTMLInputElement) {
            formData.set("email_test_to", testInput.value.trim());
          }
          formData.set("csrf_token", csrfInput.value);
          const body = new URLSearchParams();
          formData.forEach((value, key) => {
            body.set(key, String(value));
          });
          body.set("csrf_token", csrfInput.value);

          const response = await fetch(endpoint, {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "Content-Type": "application/x-www-form-urlencoded",
              "X-Requested-With": "fetch",
              "X-CSRF-Token": csrfInput.value,
            },
            body,
          });
          const payload = await response.json().catch(() => ({}));
          if (!response.ok || payload.status !== "ok") {
            throw new Error(payload.message || failedText);
          }
          const message = payload.message || successText;
          if (result instanceof HTMLElement) {
            result.textContent = message;
            result.classList.remove("is-pending", "is-error");
            result.classList.add("is-ok");
          }
          toastUI.show({ message, tone: "ok" });
        } catch (error) {
          const message = `${failedText}: ${error.message || error}`;
          if (result instanceof HTMLElement) {
            result.textContent = message;
            result.classList.remove("is-pending", "is-ok");
            result.classList.add("is-error");
          }
          toastUI.show({ message, tone: "error" });
        } finally {
          button.disabled = false;
        }
      });
    });
  };

  const initBackupRestoreForms = (root = document) => {
    scopedElements(root, "[data-backup-restore-form]").forEach((form) => {
      if (!(form instanceof HTMLFormElement) || form.dataset.backupRestoreReady === "true") {
        return;
      }
      form.dataset.backupRestoreReady = "true";
      form.addEventListener("submit", async (event) => {
        if (form.dataset.backupRestoreInspected === "true") {
          delete form.dataset.backupRestoreInspected;
          return;
        }
        const inspectURL = (form.dataset.backupInspectUrl || "").trim();
        const keyInput = form.querySelector("[data-backup-master-key]");
        const fileInput = form.querySelector('input[type="file"][name="backup"]');
        const csrfInput = form.querySelector('input[name="csrf_token"]');
        if (
          !inspectURL ||
          !(keyInput instanceof HTMLInputElement) ||
          !(fileInput instanceof HTMLInputElement) ||
          !(csrfInput instanceof HTMLInputElement) ||
          !fileInput.files ||
          fileInput.files.length === 0
        ) {
          return;
        }

        event.preventDefault();
        event.stopPropagation();
        const submitter = event.submitter instanceof HTMLElement ? event.submitter : null;
        let proceedToRestore = false;
        startControlLoading(submitter, { disable: true });
        try {
          const body = new FormData();
          body.set("csrf_token", csrfInput.value);
          body.set("backup", fileInput.files[0]);
          form.querySelectorAll('input[name="restore_data"]:checked').forEach((input) => {
            if (input instanceof HTMLInputElement) {
              body.append("restore_data", input.value);
            }
          });
          const response = await fetch(inspectURL, {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "X-CSRF-Token": csrfInput.value,
              "X-Requested-With": "fetch",
            },
            body,
          });
          const payload = await response.json().catch(() => ({}));
          if (!response.ok || payload.status === "error") {
            throw new Error(payload.message || response.statusText);
          }
          if (payload.encrypted === true && payload.requires_source_master_key === true) {
            keyInput.value = "";
            const key = await promptUI.open({
              title: form.dataset.backupMasterKeyTitle || "",
              message: form.dataset.backupMasterKeyMessage || "Enter the original master_key.",
              placeholder: form.dataset.backupMasterKeyPlaceholder || "",
              submitLabel: form.dataset.backupMasterKeySubmit || "",
              type: "password",
              autocomplete: "off",
            });
            if (key === null || key.trim() === "") {
              return;
            }
            keyInput.value = key.trim();
          } else {
            keyInput.value = "";
          }
          form.dataset.backupRestoreInspected = "true";
          proceedToRestore = true;
          if (typeof form.requestSubmit === "function") {
            if (submitter instanceof HTMLButtonElement || submitter instanceof HTMLInputElement) {
              submitter.disabled = false;
              form.requestSubmit(submitter);
              return;
            }
            form.requestSubmit();
            return;
          }
          beginLongRunningSubmit(form, submitter, submitter);
          preserveScrollForSubmit(form, submitter);
          form.submit();
        } catch (error) {
          const prefix = form.dataset.backupInspectError || "Unable to inspect backup encryption";
          toastUI.show({ message: `${prefix}: ${error.message || error}`, tone: "error" });
        } finally {
          if (!proceedToRestore) {
            stopControlLoading(submitter);
          }
        }
      });
    });
  };

  const parsePlanRatioDecimal = (raw) => {
    const value = String(raw || "").trim().replace(/^\$/, "").replaceAll(",", "");
    if (!value) {
      return NaN;
    }
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : NaN;
  };

  const parsePlanQuotaPriceRatio = (raw) => {
    const value = String(raw || "1:1").trim() || "1:1";
    const parts = value.split(":");
    if (parts.length !== 2) {
      return null;
    }
    const quota = parsePlanRatioDecimal(parts[0]);
    const price = parsePlanRatioDecimal(parts[1]);
    if (!(quota > 0) || !(price > 0)) {
      return null;
    }
    return { quota, price };
  };

  const parsePlanPeriodCount = (raw) => {
    const value = String(raw || "").trim();
    if (!/^\d+$/.test(value)) {
      return 1;
    }
    const parsed = Number.parseInt(value, 10);
    return parsed > 0 ? parsed : 1;
  };

  const formatPlanRatioAmount = (amount) => {
    if (!Number.isFinite(amount)) {
      return "";
    }
    const rounded = Math.round((amount + Number.EPSILON) * 100) / 100;
    return rounded.toFixed(2);
  };

  const initPlanRatioForms = (root = document) => {
    scopedElements(root, "[data-plan-ratio-form]").forEach((form) => {
      if (!(form instanceof HTMLFormElement) || form.dataset.planRatioReady === "true") {
        return;
      }
      const quotaInput = form.querySelector("[data-plan-quota-input]");
      const priceInput = form.querySelector("[data-plan-price-input]");
      const periodCountInput = form.querySelector("[data-plan-period-count-input]");
      const groupSelect = form.querySelector("[data-plan-group-select]");
      const syncButton = form.querySelector("[data-plan-price-sync-button]");
      if (!(quotaInput instanceof HTMLInputElement) || !(priceInput instanceof HTMLInputElement) || !(periodCountInput instanceof HTMLInputElement) || !(groupSelect instanceof HTMLSelectElement) || !(syncButton instanceof HTMLButtonElement)) {
        return;
      }
      form.dataset.planRatioReady = "true";

      const syncPrice = () => {
        const option = groupSelect.selectedOptions[0];
        const ratio = parsePlanQuotaPriceRatio(option?.dataset.quotaPriceRatio || "1:1");
        const quota = parsePlanRatioDecimal(quotaInput.value);
        const periodCount = parsePlanPeriodCount(periodCountInput.value);
        if (!ratio || !(quota >= 0)) {
          return;
        }
        priceInput.value = formatPlanRatioAmount((quota * periodCount * ratio.price) / ratio.quota);
      };

      syncButton.addEventListener("click", syncPrice);
    });
  };

  const enhanceFragment = (root = document) => {
    initDeferredPartials(root);
    initStatusAutoRefresh(root);
    initMonitorCountdowns(root);
    initAutoSubmitControls(root);
    initAccountDetails(root);
    initUserMenus(root);
    initAccountFilters(root);
    initAccountBatchForms(root);
    initAccountBatchJobs(root);
    initTimedMultiplierEditors(root);
    initGroupSettingModals(root);
    initUserDetailsLazyLoad(root);
    initAjaxLinks(root);
    initPaymentCancelOrders(root);
    initPaymentOrderQRModals(root);
    initMessageTargetFields(root);
    initSiteMessagePopups(root);
    initProxyTestButtons(root);
    initEmailTestButtons(root);
    initBackupRestoreForms(root);
    initPricingEditors(confirmUI, root);
    initPlanRatioForms(root);
    initPlanPurchaseForms(root);
    initModelGroupPopovers(root);
    initCopySecrets(toastUI, root);
    initEnhancedSelects(root);
  };

  document.addEventListener("ag:fragment-replaced", (event) => {
    const root = event.target instanceof HTMLElement ? event.target : document;
    enhanceFragment(root);
  });

  const initPlanPurchaseForms = (root = document) => {
    root.querySelectorAll("[data-plan-purchase-form]").forEach((form) => {
      if (!(form instanceof HTMLFormElement) || form.dataset.planPurchaseReady === "true") {
        return;
      }
      form.dataset.planPurchaseReady = "true";
      form.addEventListener("submit", (event) => {
        if (form.dataset.planPurchaseApproved === "true") {
          form.dataset.planPurchaseApproved = "";
          return;
        }
        event.preventDefault();
        openPlanPurchaseDialog(form).then((selection) => {
          if (!selection?.mode) {
            return;
          }
          confirmUI.open(form.dataset.purchaseConfirm || "", form.dataset.purchaseTitle || "", { tone: "primary" }).then((confirmed) => {
            if (!confirmed) {
              return;
            }
            const modeInput = form.querySelector('input[name="purchase_mode"]');
            const targetInput = form.querySelector('input[name="target_entitlement_id"]');
            if (modeInput instanceof HTMLInputElement) {
              modeInput.value = selection.mode;
            }
            if (targetInput instanceof HTMLInputElement) {
              targetInput.value = selection.targetID || "";
            }
            form.dataset.planPurchaseApproved = "true";
            if (typeof form.requestSubmit === "function") {
              form.requestSubmit();
              return;
            }
            startControlLoading(event.submitter instanceof HTMLElement ? event.submitter : form.querySelector('button[type="submit"]'), { disable: false });
            preserveScrollForSubmit(form, event.submitter instanceof HTMLElement ? event.submitter : null);
            form.submit();
          });
        });
      });
    });
  };

  const planPurchaseTargets = (form) =>
    Array.from(form.querySelectorAll("[data-plan-purchase-target]"))
      .filter((item) => item instanceof HTMLElement && (item.dataset.id || "").trim())
      .map((item, index) => ({
        id: (item.dataset.id || "").trim(),
        label: (item.dataset.label || "").trim(),
        quota: (item.dataset.quota || "").trim(),
        period: (item.dataset.period || "").trim(),
        rank: (item.dataset.rank || "").trim() || String(index + 1),
        canMerge: item.dataset.canMerge === "true",
        canExtend: item.dataset.canExtend === "true",
      }));

  const openPlanPurchaseDialog = (form) => {
    const body = document.body;
    const hasActivePlan = form.dataset.hasActivePlan === "true";
    const samePlan = form.dataset.hasMatchingActivePlan === "true";
    const targets = planPurchaseTargets(form);
    const mergeTargets = targets.filter((target) => target.canMerge);
    const extendTargets = targets.filter((target) => target.canExtend);
    if (!hasActivePlan) {
      return Promise.resolve({ mode: "separate", targetID: "" });
    }
    const options = [
      {
        mode: "separate",
        label: form.dataset.modeSeparate || "Use separately",
        hint: form.dataset.modeSeparateHint || "",
      },
    ];
    if (samePlan && mergeTargets.length > 0) {
      options.push({
        mode: "merge_quota",
        label: form.dataset.modeMerge || "Merge quota",
        hint: form.dataset.modeMergeHint || "",
      });
    }
    if (samePlan && extendTargets.length > 0) {
      options.push({
        mode: "extend_period",
        label: form.dataset.modeExtend || "Extend period",
        hint: form.dataset.modeExtendHint || "",
      });
    }
    if (options.length === 1) {
      return Promise.resolve({ mode: "separate", targetID: "" });
    }

    const overlay = document.createElement("div");
    overlay.className = "confirm-overlay plan-purchase-overlay is-visible";
    const dialog = document.createElement("div");
    dialog.className = "confirm-dialog plan-purchase-dialog";
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");

    const header = document.createElement("div");
    header.className = "confirm-header";
    const headerText = document.createElement("div");
    headerText.className = "confirm-header-text";
    const title = document.createElement("h3");
    title.className = "confirm-title";
    title.textContent = form.dataset.purchaseTitle || "";
    const caption = document.createElement("p");
    caption.className = "confirm-caption";
    caption.textContent = form.dataset.purchaseActiveMessage || "";
    headerText.append(title, caption);
    header.appendChild(headerText);

    const choices = document.createElement("div");
    choices.className = "plan-purchase-choices";
    let selectedMode = options[0].mode;
    let selectedTargetID = targets[0]?.id || "";
    const buttons = [];
    options.forEach((option) => {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "plan-purchase-choice";
      button.dataset.mode = option.mode;
      button.innerHTML = `<strong></strong><span></span>`;
      button.querySelector("strong").textContent = option.label;
      button.querySelector("span").textContent = option.hint;
      buttons.push(button);
      choices.appendChild(button);
    });

    const targetSection = document.createElement("div");
    targetSection.className = "plan-purchase-target-section";
    const targetTitle = document.createElement("strong");
    targetTitle.className = "plan-purchase-target-title";
    targetTitle.textContent = form.dataset.targetTitle || "";
    const targetChoices = document.createElement("div");
    targetChoices.className = "plan-purchase-targets-list";
    const targetButtons = [];
    targets.forEach((target) => {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "plan-purchase-target-choice";
      button.dataset.targetId = target.id;
      button.innerHTML = `<strong></strong><span></span>`;
      button.querySelector("strong").textContent = `${target.label || form.dataset.planName || ""} · #${target.rank}`;
      button.querySelector("span").textContent = `${form.dataset.targetQuotaLabel || ""} ${target.quota} · ${form.dataset.targetPeriodLabel || ""} ${target.period}`;
      targetButtons.push(button);
      targetChoices.appendChild(button);
    });
    targetSection.append(targetTitle, targetChoices);

    const syncSelection = () => {
      buttons.forEach((button) => button.classList.toggle("is-selected", button.dataset.mode === selectedMode));
      const needsTarget = selectedMode === "merge_quota" || selectedMode === "extend_period";
      const visibleTargets = selectedMode === "merge_quota" ? mergeTargets : selectedMode === "extend_period" ? extendTargets : targets;
      targetSection.hidden = !needsTarget || visibleTargets.length === 0;
      if (!needsTarget) {
        selectedTargetID = "";
      } else if (!visibleTargets.some((target) => target.id === selectedTargetID) && visibleTargets[0]) {
        selectedTargetID = visibleTargets[0].id;
      }
      targetButtons.forEach((button) => {
        const targetID = button.dataset.targetId || "";
        const visible = visibleTargets.some((target) => target.id === targetID);
        button.hidden = !visible;
        button.classList.toggle("is-selected", visible && targetID === selectedTargetID);
      });
    };
    buttons.forEach((button) => {
      button.addEventListener("click", () => {
        selectedMode = button.dataset.mode || "separate";
        syncSelection();
      });
    });
    targetButtons.forEach((button) => {
      button.addEventListener("click", () => {
        selectedTargetID = button.dataset.targetId || "";
        syncSelection();
      });
    });
    syncSelection();

    const actions = document.createElement("div");
    actions.className = "confirm-actions";
    const cancel = document.createElement("button");
    cancel.type = "button";
    cancel.className = "ghost";
    cancel.textContent = form.dataset.modeCancel || "Cancel";
    const confirm = document.createElement("button");
    confirm.type = "button";
    confirm.className = "confirm-primary";
    confirm.textContent = form.dataset.modeConfirm || "Confirm";
    actions.append(cancel, confirm);

    dialog.append(header, choices, targetSection, actions);
    overlay.appendChild(dialog);
    body.appendChild(overlay);
    body.classList.add("confirm-open");

    return new Promise((resolve) => {
      const close = (value) => {
        body.classList.remove("confirm-open");
        overlay.remove();
        resolve(value);
      };
      cancel.addEventListener("click", () => close(null));
      confirm.addEventListener("click", () => {
        const needsTarget = selectedMode === "merge_quota" || selectedMode === "extend_period";
        close({ mode: selectedMode, targetID: needsTarget ? selectedTargetID : "" });
      });
      overlay.addEventListener("click", (event) => {
        if (event.target === overlay) {
          close(null);
        }
      });
      overlay.addEventListener("keydown", (event) => {
        if (event.key === "Escape") {
          event.preventDefault();
          close(null);
        }
      });
      buttons[0]?.focus();
    });
  };

  const formSubmitMethod = (form, submitter) =>
    (submitter?.getAttribute("formmethod") || form.getAttribute("method") || "get").toLowerCase();

  const formSubmitURL = (form, submitter) =>
    new URL(submitter?.getAttribute("formaction") || form.getAttribute("action") || window.location.href, window.location.href);

  const confirmOptionForSubmit = (form, submitter) => {
    const name = (submitter?.dataset.confirmOptionName || form.dataset.confirmOptionName || "").trim();
    const label = (submitter?.dataset.confirmOptionLabel || form.dataset.confirmOptionLabel || "").trim();
    if (!name || !label) {
      return null;
    }
    return {
      name,
      label,
      value: (submitter?.dataset.confirmOptionValue || form.dataset.confirmOptionValue || "1").trim() || "1",
    };
  };

  const applyConfirmOption = (form, option, checked) => {
    form.querySelectorAll('input[data-confirm-option-field="true"]').forEach((input) => {
      if (input instanceof HTMLInputElement && input.name === option.name) {
        input.remove();
      }
    });
    if (!checked) {
      return;
    }
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = option.name;
    input.value = option.value;
    input.dataset.confirmOptionField = "true";
    form.appendChild(input);
  };

  const formBody = (form, submitter) => {
    const data = new FormData(form);
    if (submitter instanceof HTMLButtonElement || submitter instanceof HTMLInputElement) {
      const name = submitter.getAttribute("name") || "";
      if (name) {
        data.set(name, submitter.value || "");
      }
    }
    const enctype = (submitter?.getAttribute("formenctype") || form.getAttribute("enctype") || "").toLowerCase();
    if (enctype === "multipart/form-data") {
      return { body: data, headers: {} };
    }
    return {
      body: new URLSearchParams(data),
      headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" },
    };
  };

  const accountBatchJobURL = (template, id) =>
    String(template || "").replace("%id%", encodeURIComponent(String(id || "")));

  const accountBatchJobPanel = (root = document) => {
    const scope = root instanceof Element ? root : document;
    const panel = scope.matches?.("[data-account-batch-job]")
      ? scope
      : scope.querySelector?.("[data-account-batch-job]");
    return panel instanceof HTMLElement ? panel : null;
  };

  const accountBatchRefreshURL = () => {
    const source = accountBatchJobPanel(document);
    let url;
    try {
      url = new URL(String(source?.dataset.refreshUrl || window.location.href), window.location.href);
    } catch {
      return window.location.href;
    }
    const panel = document.querySelector('[data-partial="accounts-panel"]');
    const groupInput = panel?.querySelector('input[name="current_group"]');
    const filterSelect = panel?.querySelector("[data-account-filter-select]");
    const filterInput = panel?.querySelector("[data-account-current-filter]");
    const group = groupInput instanceof HTMLInputElement ? groupInput.value.trim() : "";
    const filter = filterSelect instanceof HTMLSelectElement
      ? filterSelect.value.trim()
      : filterInput instanceof HTMLInputElement
        ? filterInput.value.trim()
        : "";
    if (group) {
      url.searchParams.set("group", group);
    } else {
      url.searchParams.delete("group");
    }
    if (filter && filter !== "all") {
      url.searchParams.set("filter", filter);
    } else {
      url.searchParams.delete("filter");
    }
    return url.href;
  };

  const accountBatchCSRFToken = (form) => String(new FormData(form).get("csrf_token") || "");

  const setAccountBatchControlsDisabled = (panel, disabled) => {
    const host = panel?.closest?.(".accounts-panel") || document;
    const form = host.querySelector("[data-account-batch-form]");
    const selectedCount = Number(form instanceof HTMLFormElement ? form.dataset.selectedCount || "0" : "0");
    accountBatchButtons(host).forEach((button) => {
      button.disabled = Boolean(disabled) || selectedCount === 0;
    });
  };

  const renderAccountBatchJob = (payload, root = document) => {
    const panel = accountBatchJobPanel(root);
    if (!panel) {
      return;
    }
    if (!payload || payload.status === "idle") {
      panel.hidden = true;
      panel.dataset.running = "false";
      return;
    }
    const state = String(payload.state || "");
    const running = state === "queued" || state === "running";
    panel.hidden = false;
    panel.dataset.jobId = String(payload.id || "");
    panel.dataset.running = running ? "true" : "false";
    panel.dataset.state = state;
    panel.classList.toggle("tone-bad", payload.tone === "bad");

    const title = panel.querySelector("[data-account-batch-job-title]");
    if (title instanceof HTMLElement) {
      title.textContent = String(payload.action_text || panel.dataset.title || "");
    }
    const meta = panel.querySelector("[data-account-batch-job-meta]");
    if (meta instanceof HTMLElement) {
      const parts = [payload.counts_text, payload.elapsed_text].map((part) => String(part || "").trim()).filter(Boolean);
      meta.textContent = parts.join(" · ");
    }
    const message = panel.querySelector("[data-account-batch-job-message]");
    if (message instanceof HTMLElement) {
      message.textContent = String(payload.message || "");
    }
    const current = panel.querySelector("[data-account-batch-job-current]");
    if (current instanceof HTMLElement) {
      const text = running ? String(payload.current_text || "") : "";
      current.textContent = text;
      current.hidden = !text;
    }
    const progress = panel.querySelector("[data-account-batch-job-progress]");
    if (progress instanceof HTMLElement) {
      const percent = Math.max(0, Math.min(100, Number(payload.percent || 0)));
      progress.style.width = `${percent}%`;
    }
    const cancel = panel.querySelector("[data-account-batch-job-cancel]");
    if (cancel instanceof HTMLButtonElement) {
      cancel.hidden = !running;
      cancel.disabled = !running;
    }
    setAccountBatchControlsDisabled(panel, running);
  };

  const accountBatchCompletedAccountIDs = (payload) => {
    if (!Array.isArray(payload?.items)) {
      return [];
    }
    return payload.items
      .map((item) => String(item?.account_id || "").trim())
      .filter(Boolean);
  };

  const accountBatchNewlyCompletedAccountIDs = (payload) => {
    if (String(payload?.action || "") === "move_group") {
      return [];
    }
    const jobID = String(payload?.id || "").trim();
    if (!jobID) {
      return [];
    }
    const refreshed = accountBatchRefreshedItems.get(jobID) || new Set();
    const pending = accountBatchPendingRefreshItems.get(jobID) || new Set();
    const out = [];
    accountBatchCompletedAccountIDs(payload).forEach((accountID) => {
      if (refreshed.has(accountID) || pending.has(accountID)) {
        return;
      }
      pending.add(accountID);
      out.push(accountID);
    });
    if (pending.size) {
      accountBatchPendingRefreshItems.set(jobID, pending);
    }
    return out;
  };

  const finishAccountBatchItemRefresh = (jobID, accountIDs, refreshed) => {
    jobID = String(jobID || "").trim();
    if (!jobID || !Array.isArray(accountIDs)) {
      return;
    }
    const pending = accountBatchPendingRefreshItems.get(jobID);
    let completed = accountBatchRefreshedItems.get(jobID);
    if (!completed && refreshed) {
      completed = new Set();
      accountBatchRefreshedItems.set(jobID, completed);
    }
    accountIDs.forEach((accountID) => {
      accountID = String(accountID || "").trim();
      if (!accountID) {
        return;
      }
      pending?.delete(accountID);
      if (refreshed) {
        completed?.add(accountID);
      }
    });
    if (pending && pending.size === 0) {
      accountBatchPendingRefreshItems.delete(jobID);
    }
  };

  const accountBatchTerminalRefreshIDs = (payload) => {
    if (String(payload?.action || "") === "move_group") {
      return [];
    }
    const pending = accountBatchNewlyCompletedAccountIDs(payload);
    if (pending.length) {
      return pending;
    }
    return accountBatchCompletedAccountIDs(payload);
  };

  const accountBatchTargetGroupURL = (payload) => {
    if (String(payload?.action || "") !== "move_group") {
      return "";
    }
    const targetGroup = String(payload?.target_group || "").trim();
    if (!targetGroup) {
      return "";
    }
    let url;
    try {
      url = new URL(accountBatchRefreshURL(), window.location.href);
    } catch {
      url = new URL(window.location.href);
    }
    url.searchParams.set("group", targetGroup);
    url.searchParams.delete("batch_tone");
    url.searchParams.delete("batch_message");
    return url.href;
  };

  const clearAccountBatchRefreshState = (payload) => {
    accountBatchRefreshedItems.delete(String(payload?.id || ""));
    accountBatchPendingRefreshItems.delete(String(payload?.id || ""));
  };

  const showAccountBatchTerminalToast = (payload) => {
    const jobID = String(payload?.id || "").trim();
    const state = String(payload?.state || "").trim();
    const key = `${jobID}:${state}`;
    if (jobID && accountBatchTerminalToasts.has(key)) {
      return;
    }
    if (jobID) {
      accountBatchTerminalToasts.add(key);
    }
    const tone = payload?.tone === "bad" ? "error" : "ok";
    const options = { message: String(payload?.message || ""), tone };
    if (String(payload?.action || "") === "test") {
      if (tone === "error") {
        options.sticky = true;
      } else {
        options.duration = 3000;
      }
    }
    toastUI.show(options);
  };

  const refreshAccountBatchPartials = async (accountIDs = [], refreshURLOverride = "") => {
    const selector = '[data-partial="account-pool"], [data-partial="accounts-panel"]';
    const targets = Array.from(document.querySelectorAll(selector)).filter((target) => target instanceof HTMLElement);
    if (!targets.length) {
      return;
    }
    const refreshURL = refreshURLOverride || accountBatchRefreshURL();
    const response = await fetch(refreshURL, {
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
    const doc = new DOMParser().parseFromString(await response.text(), "text/html");
    const scopedAccountIDs = Array.from(new Set((Array.isArray(accountIDs) ? accountIDs : [])
      .map((accountID) => String(accountID || "").trim())
      .filter(Boolean)));
    if (scopedAccountIDs.length) {
      const pool = document.querySelector('[data-partial="account-pool"]');
      const incomingPool = pool instanceof HTMLElement ? matchedIncomingTarget(doc, pool) : null;
      const newRoots = [];
      if (pool instanceof HTMLElement && incomingPool instanceof HTMLElement) {
        const clone = incomingPool.cloneNode(true);
        pool.replaceWith(clone);
        if (clone instanceof HTMLElement) {
          newRoots.push(clone);
        }
      }
      const panel = document.querySelector('[data-partial="accounts-panel"]');
      scopedAccountIDs.forEach((accountID) => {
        const cardSelector = `[data-account-card][data-account-id="${CSS.escape(accountID)}"]`;
        const card = document.querySelector(cardSelector);
        const incoming = doc.querySelector(cardSelector);
        if (card instanceof HTMLElement && incoming instanceof HTMLElement) {
          const clone = incoming.cloneNode(true);
          card.replaceWith(clone);
          if (clone instanceof HTMLElement) {
            newRoots.push(clone);
          }
        }
      });
      if (panel instanceof HTMLElement) {
        accountSelectionCache.delete(panel);
        accountFilterInputCache.delete(panel);
        panel.querySelector("[data-account-filter-select]")?.dispatchEvent(new Event("change", { bubbles: true }));
      }
      newRoots.forEach((root) => enhanceFragment(root));
      return;
    }
    const replacements = [];
    for (const target of targets) {
      const incoming = matchedIncomingTarget(doc, target);
      if (incoming instanceof HTMLElement) {
        replacements.push({ target, incoming });
      }
    }
    const newRoots = [];
    replacements.forEach(({ target, incoming }) => {
      const clone = incoming.cloneNode(true);
      target.replaceWith(clone);
      if (clone instanceof HTMLElement) {
        newRoots.push(clone);
      }
    });
    newRoots.forEach((root) => enhanceFragment(root));
  };

  const pollAccountBatchJob = (id, root = document) => {
    const panel = accountBatchJobPanel(root);
    if (!panel || !id) {
      return;
    }
    window.clearTimeout(accountBatchPollTimer);
    accountBatchPollTimer = window.setTimeout(async () => {
      if (!document.contains(panel)) {
        return;
      }
      try {
        const response = await fetch(accountBatchJobURL(panel.dataset.statusUrlTemplate, id), {
          method: "GET",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "X-Requested-With": "fetch",
          },
        });
        const payload = await response.json().catch(() => ({}));
        if (!response.ok || payload.status !== "ok") {
          toastUI.show({ message: payload.message || response.statusText, tone: "error" });
          return;
        }
        renderAccountBatchJob(payload);
        const newlyCompleted = accountBatchNewlyCompletedAccountIDs(payload);
        if (newlyCompleted.length) {
          const jobID = String(payload.id || "");
          refreshAccountBatchPartials(newlyCompleted)
            .then(() => finishAccountBatchItemRefresh(jobID, newlyCompleted, true))
            .catch(() => finishAccountBatchItemRefresh(jobID, newlyCompleted, false));
        }
        const terminal = payload.state === "completed" || payload.state === "cancelled";
        if (terminal) {
          showAccountBatchTerminalToast(payload);
          if (String(payload.action || "") === "move_group") {
            const targetURL = accountBatchTargetGroupURL(payload) || accountBatchRefreshURL();
            if (targetURL) {
              window.history.replaceState(window.history.state, "", targetURL);
            }
            refreshAccountBatchPartials([], targetURL).finally(() => {
              clearAccountBatchRefreshState(payload);
            });
            return;
          }
          const refreshIDs = accountBatchTerminalRefreshIDs(payload);
          if (refreshIDs.length) {
            refreshAccountBatchPartials(refreshIDs).finally(() => clearAccountBatchRefreshState(payload));
          } else {
            clearAccountBatchRefreshState(payload);
          }
          return;
        }
        pollAccountBatchJob(id);
      } catch (error) {
        toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
      }
    }, 1200);
  };

  document.addEventListener("ag:account-batch-updated", (event) => {
    const payload = event.detail || {};
    const panel = accountBatchJobPanel(document);
    if (!panel || !payload || payload.status !== "ok") {
      return;
    }
    renderAccountBatchJob(payload);
    const newlyCompleted = accountBatchNewlyCompletedAccountIDs(payload);
    if (newlyCompleted.length) {
      const jobID = String(payload.id || "");
      refreshAccountBatchPartials(newlyCompleted)
        .then(() => finishAccountBatchItemRefresh(jobID, newlyCompleted, true))
        .catch(() => finishAccountBatchItemRefresh(jobID, newlyCompleted, false));
    }
    if (payload.state === "completed" || payload.state === "cancelled") {
      showAccountBatchTerminalToast(payload);
      if (String(payload.action || "") === "move_group") {
        const targetURL = accountBatchTargetGroupURL(payload) || accountBatchRefreshURL();
        if (targetURL) {
          window.history.replaceState(window.history.state, "", targetURL);
        }
        refreshAccountBatchPartials([], targetURL).finally(() => {
          clearAccountBatchRefreshState(payload);
        });
        return;
      }
      const refreshIDs = accountBatchTerminalRefreshIDs(payload);
      if (refreshIDs.length) {
        refreshAccountBatchPartials(refreshIDs).finally(() => clearAccountBatchRefreshState(payload));
      } else {
        clearAccountBatchRefreshState(payload);
      }
    }
  });

  document.addEventListener("ag:account-pool-changed", () => {
    if (!document.querySelector('[data-partial="account-pool"], [data-partial="accounts-panel"]')) {
      return;
    }
    refreshAccountBatchPartials().catch(() => undefined);
  });

  const startAccountBatchJob = async (form, submitter) => {
    const panel = accountBatchJobPanel(document);
    if (!panel || !(form instanceof HTMLFormElement)) {
      return;
    }
    const { body, headers } = formBody(form, submitter);
    startControlLoading(submitter);
    try {
      const response = await fetch(formSubmitURL(form, submitter), {
        method: formSubmitMethod(form, submitter).toUpperCase(),
        credentials: "same-origin",
        headers: {
          Accept: "application/json",
          "X-Requested-With": "fetch",
          "X-CSRF-Token": accountBatchCSRFToken(form),
          ...headers,
        },
        body,
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok || payload.status !== "ok") {
        toastUI.show({ message: payload.message || response.statusText, tone: "error" });
        return;
      }
      closeAjaxFormModal(form);
      renderAccountBatchJob(payload);
      pollAccountBatchJob(payload.id);
    } catch (error) {
      toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
    } finally {
      if (panel.dataset.running !== "true") {
        stopControlLoading(submitter);
      }
    }
  };

  const cancelAccountBatchJob = async (panel) => {
    const id = panel?.dataset.jobId || "";
    if (!panel || !id) {
      return;
    }
    const form = document.getElementById("account-batch-form");
    const cancelURL = accountBatchJobURL(panel.dataset.cancelUrlTemplate, id);
    const button = panel.querySelector("[data-account-batch-job-cancel]");
    startControlLoading(button);
    try {
      const response = await fetch(cancelURL, {
        method: "POST",
        credentials: "same-origin",
        headers: {
          Accept: "application/json",
          "X-Requested-With": "fetch",
          "X-CSRF-Token": form instanceof HTMLFormElement ? accountBatchCSRFToken(form) : "",
        },
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok || payload.status !== "ok") {
        toastUI.show({ message: payload.message || response.statusText, tone: "error" });
        return;
      }
      renderAccountBatchJob(payload);
      pollAccountBatchJob(payload.id);
    } catch (error) {
      toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
    } finally {
      stopControlLoading(button);
    }
  };

  const initAccountBatchJobs = (root = document) => {
    const panel = accountBatchJobPanel(root);
    if (!panel || panel.dataset.accountBatchJobReady === "true") {
      return;
    }
    panel.dataset.accountBatchJobReady = "true";
    const cancel = panel.querySelector("[data-account-batch-job-cancel]");
    if (cancel instanceof HTMLButtonElement) {
      cancel.addEventListener("click", () => cancelAccountBatchJob(panel));
    }
    if (!panel.dataset.activeUrl) {
      return;
    }
    fetch(panel.dataset.activeUrl, {
      method: "GET",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "X-Requested-With": "fetch",
      },
    })
      .then((response) => response.json().catch(() => ({})))
      .then((payload) => {
        if (!payload || payload.status !== "ok") {
          return;
        }
        renderAccountBatchJob(payload, root);
        pollAccountBatchJob(payload.id, root);
      })
      .catch(() => undefined);
  };

  const matchedIncomingTarget = (doc, target) => {
    const partial = target.getAttribute("data-partial");
    if (partial) {
      return doc.querySelector(`[data-partial="${CSS.escape(partial)}"]`);
    }
    if (target.id) {
      return doc.getElementById(target.id);
    }
    const selector = target.getAttribute("data-partial-selector") || "";
    return selector ? doc.querySelector(selector) : null;
  };

  const syncAjaxURL = (doc, responseURL) => {
    if (!window.history || typeof window.history.replaceState !== "function") {
      return;
    }
    const next = new URL(responseURL.href);
    next.searchParams.delete("partial");
    doc.querySelectorAll("[data-clear-url-params]").forEach((element) => {
      (element.dataset.clearUrlParams || "")
        .split(",")
        .map((param) => param.trim())
        .filter(Boolean)
        .forEach((param) => next.searchParams.delete(param));
    });
    const nextPath = `${next.pathname}${next.search}${next.hash}`;
    const currentPath = `${window.location.pathname}${window.location.search}${window.location.hash}`;
    if (next.origin === window.location.origin && nextPath !== currentPath) {
      window.history.replaceState(window.history.state, "", nextPath);
    }
  };

  const ajaxPartialName = (source, targets) => {
    const configured = (source?.dataset?.ajaxPartial || "").trim();
    if (configured) {
      return configured;
    }
    for (const target of targets) {
      const partial = (target.getAttribute("data-partial") || "").trim();
      if (partial) {
        return partial;
      }
    }
    return "";
  };

  const ajaxPartialURL = (url, partial) => {
    const next = new URL(url.href);
    if (partial) {
      next.searchParams.set("partial", partial);
    }
    return next;
  };

  const publicAjaxURL = (url) => {
    const next = new URL(url.href);
    next.searchParams.delete("partial");
    return next;
  };

  const replaceAjaxTargets = (doc, targets) => {
    const replacements = [];
    for (const target of targets) {
      const incoming = matchedIncomingTarget(doc, target);
      if (!(incoming instanceof HTMLElement)) {
        return null;
      }
      replacements.push({ target, incoming });
    }
    const newRoots = [];
    replacements.forEach(({ target, incoming }) => {
      const clone = incoming.cloneNode(true);
      target.replaceWith(clone);
      if (clone instanceof HTMLElement) {
        newRoots.push(clone);
      }
    });
    return newRoots;
  };

  const closeAjaxFormModal = (source) => {
    const modal = source?.closest?.(".settings-overlay");
    if (!(modal instanceof HTMLElement) || !modal.isConnected || !modal.classList.contains("is-visible")) {
      return;
    }
    const closeButton = modal.querySelector("[data-group-settings-close]");
    if (closeButton instanceof HTMLButtonElement) {
      closeButton.click();
      return;
    }
    modal.classList.remove("is-visible");
    modal.hidden = true;
    modal.dispatchEvent(new CustomEvent("modal:close"));
    if (!document.querySelector(".settings-overlay.is-visible")) {
      document.body.classList.remove("settings-open");
    }
  };

  const loadAjaxTargets = async (url, targets, partial = "", trigger = null) => {
    const fetchURL = ajaxPartialURL(url, partial);
    targets.forEach((target) => target.setAttribute("aria-busy", "true"));
    startControlLoading(trigger);
    try {
      const response = await fetch(fetchURL, {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
          ...(partial ? { "X-Ajax-Partial": partial } : {}),
        },
      });
      const responseURL = new URL(response.url || fetchURL.href, window.location.href);
      if (responseURL.origin !== window.location.origin || responseURL.pathname !== window.location.pathname) {
        window.location.href = publicAjaxURL(responseURL).href;
        return true;
      }
      const html = await response.text();
      if (!response.ok) {
        toastUI.show({ message: html.trim() || response.statusText, tone: "error" });
        return true;
      }
      const doc = new DOMParser().parseFromString(html, "text/html");
      const newRoots = replaceAjaxTargets(doc, targets);
      if (!newRoots) {
        window.location.href = publicAjaxURL(responseURL).href;
        return true;
      }
      document.querySelectorAll(".select-menu.is-portaled").forEach((menu) => menu.remove());
      showServerRenderedToasts(toastUI, doc);
      syncAjaxURL(doc, responseURL);
      newRoots.forEach((root) => enhanceFragment(root));
      return true;
    } catch (error) {
      toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
      return true;
    } finally {
      targets.forEach((target) => target.removeAttribute("aria-busy"));
      stopControlLoading(trigger);
    }
  };

  const statusAutoRefreshURL = (partial) => {
    const url = new URL(window.location.href);
    if (partial) {
      url.searchParams.set("partial", partial);
    }
    return url;
  };

  const shouldSkipStatusAutoRefresh = (target) => {
    if (!(target instanceof HTMLElement) || !target.isConnected) {
      return true;
    }
    if (document.visibilityState === "hidden") {
      return true;
    }
    if (target.getAttribute("aria-busy") === "true" || target.dataset.statusAutoRefreshRequesting === "true") {
      return true;
    }
    if (target.querySelector(".settings-overlay.is-visible")) {
      return true;
    }
    const active = document.activeElement;
    return active instanceof HTMLElement && active !== document.body && target.contains(active);
  };

  const refreshStatusAutoPartial = async (target) => {
    const partial = (target.getAttribute("data-partial") || "").trim();
    if (!partial || shouldSkipStatusAutoRefresh(target)) {
      return;
    }
    const url = statusAutoRefreshURL(partial);
    target.dataset.statusAutoRefreshRequesting = "true";
    try {
      const response = await fetch(url, {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
          "X-Ajax-Partial": partial,
        },
      });
      if (!response.ok) {
        return;
      }
      const html = await response.text();
      const doc = new DOMParser().parseFromString(html, "text/html");
      const incoming = matchedIncomingTarget(doc, target);
      if (!(incoming instanceof HTMLElement)) {
        return;
      }
      const openDetails = Array.from(target.querySelectorAll("details")).map((detail) => detail.open);
      const clone = incoming.cloneNode(true);
      if (!(clone instanceof HTMLElement)) {
        return;
      }
      clone.querySelectorAll("details").forEach((detail, index) => {
        detail.open = Boolean(openDetails[index]);
      });
      const timer = statusAutoRefreshTimers.get(target);
      if (timer) {
        window.clearInterval(timer);
        statusAutoRefreshTimers.delete(target);
      }
      target.replaceWith(clone);
      enhanceFragment(clone);
    } catch {
      // Status refresh is best-effort; the next polling cycle can recover.
    } finally {
      delete target.dataset.statusAutoRefreshRequesting;
    }
  };

  const initStatusAutoRefresh = (root = document) => {
    scopedElements(root, "[data-status-auto-refresh][data-partial]").forEach((target) => {
      if (!(target instanceof HTMLElement) || target.dataset.statusAutoRefreshReady === "true") {
        return;
      }
      const partial = (target.getAttribute("data-partial") || "").trim();
      if (!partial) {
        return;
      }
      const interval = Math.max(5000, Number.parseInt(target.dataset.statusRefreshInterval || "15000", 10) || 15000);
      target.dataset.statusAutoRefreshReady = "true";
      const timer = window.setInterval(() => {
        if (!target.isConnected) {
          window.clearInterval(timer);
          statusAutoRefreshTimers.delete(target);
          return;
        }
        refreshStatusAutoPartial(target);
      }, interval);
      statusAutoRefreshTimers.set(target, timer);
    });
  };

  const monitorCountdownSpinnerHTML = (label = "Checking") => `
    <span class="monitor-countdown-spinner" aria-hidden="true"></span>
    <span class="sr-only">${label || "Checking"}</span>
  `;

  const formatMonitorCountdown = (seconds) => {
    const remaining = Math.max(0, Math.ceil(seconds));
    if (remaining <= 0) {
      return "";
    }
    const hours = Math.floor(remaining / 3600);
    const minutes = Math.floor((remaining % 3600) / 60);
    const secs = remaining % 60;
    if (hours > 0) {
      return `${hours}h ${minutes}m`;
    }
    if (minutes > 0) {
      return `${minutes}m ${secs}s`;
    }
    return `${secs}s`;
  };

  const updateMonitorCountdowns = (root = document) => {
    scopedElements(root, "[data-monitor-countdown]").forEach((element) => {
      if (!(element instanceof HTMLElement)) {
        return;
      }
      const next = Number.parseInt(element.dataset.nextCheckAt || "0", 10);
      if (!Number.isFinite(next) || next <= 0) {
        element.textContent = element.dataset.pendingText || "-";
        element.dataset.countdownState = "pending";
        return;
      }
      if (element.dataset.monitorRunning === "true") {
        element.innerHTML = monitorCountdownSpinnerHTML(element.dataset.runningText || "Checking");
        element.dataset.countdownState = "running";
        return;
      }
      const remaining = next - Math.floor(Date.now() / 1000);
      if (remaining <= 0) {
        element.innerHTML = monitorCountdownSpinnerHTML(element.dataset.dueText || element.dataset.runningText || "Checking");
        element.dataset.countdownState = "due";
        return;
      }
      element.textContent = formatMonitorCountdown(remaining);
      element.dataset.countdownState = "waiting";
    });
  };

  const initMonitorCountdowns = (root = document) => {
    updateMonitorCountdowns(root);
    if (monitorCountdownTimer) {
      return;
    }
    monitorCountdownTimer = window.setInterval(() => updateMonitorCountdowns(document), 1000);
  };

  const formGETURL = (form, submitter) => {
    const url = formSubmitURL(form, submitter);
    const data = new FormData(form);
    if (submitter instanceof HTMLButtonElement || submitter instanceof HTMLInputElement) {
      const name = submitter.getAttribute("name") || "";
      if (name) {
        data.set(name, submitter.value || "");
      }
    }
    url.search = "";
    data.forEach((value, key) => {
      if (value instanceof File) {
        return;
      }
      url.searchParams.append(key, String(value));
    });
    return url;
  };

  const submitAjaxForm = async (form, submitter, trigger = submitter) => {
    const selector = (submitter?.dataset.ajaxTarget || form.dataset.ajaxTarget || "").trim();
    if (!selector) {
      return false;
    }
    let targets = [];
    try {
      targets = Array.from(document.querySelectorAll(selector)).filter((target) => target instanceof HTMLElement);
    } catch {
      return false;
    }
    if (!targets.length) {
      return false;
    }
    const method = formSubmitMethod(form, submitter);
    const partial = (submitter?.dataset.ajaxPartial || form.dataset.ajaxPartial || ajaxPartialName(form, targets)).trim();
    if (method === "get") {
      return loadAjaxTargets(formGETURL(form, submitter), targets, partial, trigger);
    }
    const url = formSubmitURL(form, submitter);
    const csrfToken = new FormData(form).get("csrf_token") || "";
    const { body, headers } = formBody(form, submitter);
    targets.forEach((target) => target.setAttribute("aria-busy", "true"));
    startControlLoading(trigger);
    try {
      const response = await fetch(url, {
        method: method.toUpperCase(),
        credentials: "same-origin",
        headers: {
          Accept: "text/html",
          "X-Requested-With": "fetch",
          "X-CSRF-Token": String(csrfToken),
          ...(partial ? { "X-Ajax-Partial": partial } : {}),
          ...headers,
        },
        body,
      });
      const responseURL = new URL(response.url || url.href, window.location.href);
      if (responseURL.origin !== window.location.origin || responseURL.pathname !== window.location.pathname) {
        window.location.href = responseURL.href;
        return true;
      }
      const html = await response.text();
      if (!response.ok) {
        toastUI.show({ message: html.trim() || response.statusText, tone: "error" });
        return true;
      }
      const doc = new DOMParser().parseFromString(html, "text/html");
      const newRoots = replaceAjaxTargets(doc, targets);
      if (!newRoots) {
        window.location.href = publicAjaxURL(responseURL).href;
        return true;
      }
      closeAjaxFormModal(submitter || form);
      document.querySelectorAll(".select-menu.is-portaled").forEach((menu) => menu.remove());
      if (!document.querySelector(".settings-overlay.is-visible")) {
        document.body.classList.remove("settings-open");
      }
      showServerRenderedToasts(toastUI, doc);
      syncAjaxURL(doc, responseURL);
      newRoots.forEach((root) => enhanceFragment(root));
      return true;
    } catch (error) {
      toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
      return true;
    } finally {
      targets.forEach((target) => target.removeAttribute("aria-busy"));
      stopControlLoading(trigger);
    }
  };

  const initAjaxLinks = (root = document) => {
    scopedElements(root, "a[data-ajax-link][data-ajax-target]").forEach((link) => {
      if (!(link instanceof HTMLAnchorElement) || link.dataset.ajaxLinkReady === "true") {
        return;
      }
      link.dataset.ajaxLinkReady = "true";
      link.addEventListener("click", (event) => {
        if (
          event.defaultPrevented ||
          event.button !== 0 ||
          event.metaKey ||
          event.ctrlKey ||
          event.shiftKey ||
          event.altKey ||
          link.target ||
          link.hasAttribute("download")
        ) {
          return;
        }
        const selector = (link.dataset.ajaxTarget || "").trim();
        if (!selector) {
          return;
        }
        let targets = [];
        try {
          targets = Array.from(document.querySelectorAll(selector)).filter((target) => target instanceof HTMLElement);
        } catch {
          return;
        }
        if (!targets.length) {
          return;
        }
        if (controlLoadingState.has(link)) {
          event.preventDefault();
          return;
        }
        event.preventDefault();
        const partial = ajaxPartialName(link, targets);
        loadAjaxTargets(new URL(link.href, window.location.href), targets, partial, link);
      });
    });
  };

  restorePreservedScroll();
  showServerRenderedToasts(toastUI);

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    if (event.defaultPrevented) {
      return;
    }

    const submitter = event.submitter instanceof HTMLElement ? event.submitter : null;
    const autoSubmitTrigger = autoSubmitControls.get(form) || null;
    autoSubmitControls.delete(form);
    const loadingControl = submitter || autoSubmitTrigger;
    const message = (submitter?.dataset.confirm || form.dataset.confirm || "").trim();
    const ajaxEnabled =
      (form.hasAttribute("data-ajax-form") || submitter?.hasAttribute("data-ajax-form")) &&
      !submitter?.hasAttribute("data-ajax-skip");
    const downloadEnabled =
      form.hasAttribute("data-download-form") || submitter?.hasAttribute("data-download-form");
    const longRunningEnabled =
      form.hasAttribute("data-long-running-form") || submitter?.hasAttribute("data-long-running-form");
    if (longRunningEnabled && longRunningSubmission?.form === form) {
      event.preventDefault();
      return;
    }
    if (
      approvedSubmission &&
      approvedSubmission.form === form &&
      approvedSubmission.submitter === submitter
    ) {
      approvedSubmission = null;
      if (ajaxEnabled) {
        event.preventDefault();
        submitAjaxForm(form, submitter, loadingControl);
        return;
      }
      if (downloadEnabled) {
        event.preventDefault();
        submitDownloadForm(form, submitter, loadingControl);
        return;
      }
      if (longRunningEnabled) {
        beginLongRunningSubmit(form, submitter, loadingControl);
      } else {
        showMonitorRunPending(loadingControl);
        startControlLoading(loadingControl, { disable: false });
        preserveScrollForSubmit(form, submitter);
      }
      return;
    }
    if (!message) {
      if (ajaxEnabled) {
        event.preventDefault();
        submitAjaxForm(form, submitter, loadingControl);
        return;
      }
      if (downloadEnabled) {
        event.preventDefault();
        submitDownloadForm(form, submitter, loadingControl);
        return;
      }
      if (longRunningEnabled) {
        beginLongRunningSubmit(form, submitter, loadingControl);
      } else {
        showMonitorRunPending(loadingControl);
        startControlLoading(loadingControl, { disable: false });
        preserveScrollForSubmit(form, submitter);
      }
      return;
    }

    event.preventDefault();
    const confirmOption = confirmOptionForSubmit(form, submitter);
    const confirmTitle = (submitter?.dataset.confirmTitle || form.dataset.confirmTitle || "").trim();
    const confirmTone = (submitter?.dataset.confirmTone || form.dataset.confirmTone || "").trim();
    confirmUI.open(message, confirmTitle, {
      optionLabel: confirmOption?.label || "",
      tone: confirmTone,
    }).then((result) => {
      if (!result) {
        return;
      }
      if (confirmOption) {
        applyConfirmOption(form, confirmOption, typeof result === "object" && result.optionChecked === true);
      }
      approvedSubmission = { form, submitter };
      if (typeof form.requestSubmit === "function") {
        if (submitter instanceof HTMLButtonElement || submitter instanceof HTMLInputElement) {
          form.requestSubmit(submitter);
          return;
        }
        form.requestSubmit();
        return;
      }
      if (ajaxEnabled) {
        submitAjaxForm(form, submitter, loadingControl);
        return;
      }
      if (downloadEnabled) {
        submitDownloadForm(form, submitter, loadingControl);
        return;
      }
      if (longRunningEnabled) {
        beginLongRunningSubmit(form, submitter, loadingControl);
      } else {
        showMonitorRunPending(loadingControl);
        startControlLoading(loadingControl, { disable: false });
        preserveScrollForSubmit(form, submitter);
      }
      form.submit();
    });
  });

  initDeferredPartials();
  initStatusAutoRefresh();
  initMonitorCountdowns();
  initAjaxLinks();
  initAutoSubmitControls();

  document.querySelectorAll("[data-account-group-create]").forEach((button) => {
    button.addEventListener("click", async () => {
      const formID = button.dataset.promptForm || "";
      const form = formID ? document.getElementById(formID) : null;
      if (!(form instanceof HTMLFormElement)) {
        return;
      }
      const nameInput = form.querySelector('input[name="name"]');
      if (!(nameInput instanceof HTMLInputElement)) {
        return;
      }
      const name = await promptUI.open({
        title: button.dataset.promptTitle || "",
        message: button.dataset.promptMessage || "Enter a group name.",
        placeholder: button.dataset.promptPlaceholder || "",
        submitLabel: button.dataset.promptSubmit || "",
      });
      if (name === null || name.trim() === "") {
        return;
      }
      nameInput.value = name.trim();
      form.requestSubmit();
    });
  });

  initPasswordToggles();
  initAccountLoginForms();
  initPricingEditors(confirmUI);
  initEmailProviderFields();
  initRegistrationEmailDomains();
  initSettingsTabs();
  initEmailCodeSenders(toastUI);
  initModelGroupPopovers();
  initMessageTargetFields();
  initSiteMessagePopups();

  initAccountDetails();
  initAccountFilters();
  initAccountBatchForms();
  initAccountBatchJobs();
  initBackupRestoreForms();

  const userMenuPanel = (details) => {
    if (!(details instanceof HTMLElement)) {
      return null;
    }
    const panel = details.querySelector(".user-menu-panel");
    return panel instanceof HTMLElement ? panel : null;
  };

  const userMenuSummary = (details) => {
    if (!(details instanceof HTMLElement)) {
      return null;
    }
    const summary = details.querySelector("summary");
    return summary instanceof HTMLElement ? summary : null;
  };

  const resetUserMenuPanelPosition = (details) => {
    const panel = userMenuPanel(details);
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

  const positionUserMenuPanel = (details) => {
    if (!(details instanceof HTMLDetailsElement) || !details.open) {
      resetUserMenuPanelPosition(details);
      return;
    }
    const summary = userMenuSummary(details);
    const panel = userMenuPanel(details);
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
    const openAbove = spaceBelow < 140 && spaceAbove > spaceBelow;
    const availableHeight = Math.max(100, (openAbove ? spaceAbove : spaceBelow) - panelGap);
    const width = Math.min(Math.max(rect.width, 160), Math.max(128, viewportWidth - viewportMargin * 2));
    const left = Math.min(
      Math.max(viewportMargin, rect.right - width),
      Math.max(viewportMargin, viewportWidth - width - viewportMargin)
    );

    panel.classList.add("is-fixed");
    panel.style.left = `${left}px`;
    panel.style.right = "auto";
    panel.style.width = `${width}px`;
    panel.style.maxHeight = `${Math.min(240, availableHeight)}px`;
    panel.style.zIndex = "85";
    panel.dataset.placement = openAbove ? "top" : "bottom";
    if (openAbove) {
      panel.style.top = "auto";
      panel.style.bottom = `${viewportHeight - rect.top + panelGap}px`;
    } else {
      panel.style.top = `${rect.bottom + panelGap}px`;
      panel.style.bottom = "auto";
    }
  };

  const closeUserMenus = (except = null) => {
    openUserMenus.forEach((details) => {
      if (!details.isConnected) {
        openUserMenus.delete(details);
        return;
      }
      if (details !== except) {
        details.removeAttribute("open");
        resetUserMenuPanelPosition(details);
        openUserMenus.delete(details);
      }
    });
  };

  const initUserMenus = (root = document) => {
    scopedElements(root, ".user-menu").forEach((details) => {
      if (details.dataset.userMenuReady === "true") {
        if (details.open) {
          positionUserMenuPanel(details);
        }
        return;
      }
      details.dataset.userMenuReady = "true";
      details.addEventListener("toggle", () => {
        if (details.open) {
          openUserMenus.add(details);
          closeUserMenus(details);
          positionUserMenuPanel(details);
        } else {
          openUserMenus.delete(details);
          resetUserMenuPanelPosition(details);
        }
      });
      if (details.open) {
        openUserMenus.add(details);
        positionUserMenuPanel(details);
      }
    });
    if (userMenuGlobalListenersReady) {
      return;
    }
    userMenuGlobalListenersReady = true;
    window.addEventListener("resize", () => {
      openUserMenus.forEach((details) => {
        if (details.isConnected) {
          positionUserMenuPanel(details);
        } else {
          openUserMenus.delete(details);
        }
      });
    });
    window.addEventListener(
      "scroll",
      () => {
        openUserMenus.forEach((details) => {
          if (details.isConnected) {
            positionUserMenuPanel(details);
          } else {
            openUserMenus.delete(details);
          }
        });
      },
      true
    );
  };

  initUserMenus();

  document.addEventListener("click", (event) => {
    const target = event.target;
    if (!(target instanceof Node)) {
      return;
    }
    const element = target instanceof Element ? target : target.parentElement;
    if (!(element instanceof Element) || !element.closest(".account-details")) {
      closeAccountDetails();
    }
    if (!(element instanceof Element) || !element.closest(".user-menu")) {
      closeUserMenus();
    }
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      closeAccountDetails();
      closeUserMenus();
    }
  });

  const initTimedMultiplierEditors = (root = document) => {
  scopedElements(root, "[data-timed-multiplier-list]").forEach((list) => {
    if (!(list instanceof HTMLElement)) {
      return;
    }
    if (list.dataset.timedMultiplierReady === "true") {
      return;
    }
    list.dataset.timedMultiplierReady = "true";
    const form = list.closest("form");
    const modal = document.getElementById("timed-multiplier-editor");
    const editor = modal?.querySelector("[data-timed-multiplier-editor]");
    if (!(form instanceof HTMLFormElement) || !(modal instanceof HTMLElement) || !(editor instanceof HTMLElement)) {
      return;
    }
    const inputs = {
      id: editor.querySelector('[data-rule-input="id"]'),
      mode: editor.querySelector('[data-rule-input="mode"]'),
      name: editor.querySelector('[data-rule-input="name"]'),
      value: editor.querySelector('[data-rule-input="value"]'),
      startDate: editor.querySelector('[data-rule-input="startDate"]'),
      endDate: editor.querySelector('[data-rule-input="endDate"]'),
      startTime: editor.querySelector('[data-rule-input="startTime"]'),
      endTime: editor.querySelector('[data-rule-input="endTime"]'),
      priority: editor.querySelector('[data-rule-input="priority"]'),
      enabled: editor.querySelector('[data-rule-input="enabled"]'),
    };
    const weekdayInputs = Array.from(editor.querySelectorAll('[data-rule-input="weekday"]')).filter(
      (input) => input instanceof HTMLInputElement
    );
    if (
      !(inputs.id instanceof HTMLInputElement) ||
      !(inputs.mode instanceof HTMLInputElement) ||
      !(inputs.name instanceof HTMLInputElement) ||
      !(inputs.value instanceof HTMLInputElement) ||
      !(inputs.startDate instanceof HTMLInputElement) ||
      !(inputs.endDate instanceof HTMLInputElement) ||
      !(inputs.startTime instanceof HTMLInputElement) ||
      !(inputs.endTime instanceof HTMLInputElement) ||
      !(inputs.priority instanceof HTMLInputElement) ||
      !(inputs.enabled instanceof HTMLInputElement)
    ) {
      return;
    }
    const title = modal.querySelector("[data-timed-multiplier-title]");
    const saveButton = modal.querySelector("[data-timed-multiplier-save]");
    const labels = list.dataset;
    let editingRow = null;
    let previousFocus = null;
    let pointerStartedOnOverlay = false;

    const confirmRemoveRule = async () => {
      const message = (labels.confirmDeleteLabel || "").trim();
      if (!message) {
        return true;
      }
      return Boolean(await confirmUI.open(message));
    };

    const fieldName = (id, field) => {
      switch (field) {
        case "name":
          return `timed_multiplier_name_${id}`;
        case "value":
          return `timed_multiplier_value_${id}`;
        case "startDate":
          return `timed_multiplier_start_date_${id}`;
        case "endDate":
          return `timed_multiplier_end_date_${id}`;
        case "startTime":
          return `timed_multiplier_start_time_${id}`;
        case "endTime":
          return `timed_multiplier_end_time_${id}`;
        case "priority":
          return `timed_multiplier_priority_${id}`;
        case "weekday":
          return `timed_multiplier_weekday_${id}`;
        default:
          return "";
      }
    };

    const readRow = (row) => {
      const id = row.dataset.ruleId || "";
      const hidden = (field) => row.querySelector(`[data-rule-hidden="${field}"]`)?.value || "";
      return {
        id,
        name: hidden("name"),
        value: hidden("value") || "1",
        startDate: hidden("startDate"),
        endDate: hidden("endDate"),
        startTime: hidden("startTime"),
        endTime: hidden("endTime"),
        priority: hidden("priority") || "0",
        enabled: row.querySelector('[data-rule-hidden="enabled"]') instanceof HTMLInputElement,
        weekdays: Array.from(row.querySelectorAll('[data-rule-hidden="weekday"]')).map((input) => input.value),
      };
    };

    const writeInput = (row, name, value, marker) => {
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = name;
      input.value = value;
      input.dataset.ruleHidden = marker;
      row.appendChild(input);
    };

    const dateSummary = (rule) => {
      if (!rule.startDate && !rule.endDate) {
        return labels.anyDateLabel || "Any date";
      }
      return `${rule.startDate || labels.startLabel || "Start"} - ${rule.endDate || labels.endLabel || "End"}`;
    };

    const timeSummary = (rule) => {
      if (!rule.startTime && !rule.endTime) {
        return labels.allDayLabel || "All day";
      }
      return `${rule.startTime || "--:--"} - ${rule.endTime || "--:--"}`;
    };

    const weekdaySummary = (rule) => {
      if (!rule.weekdays.length) {
        return labels.everydayLabel || "Every day";
      }
      const weekdayLabels = Array.from(editor.querySelectorAll('[data-rule-input="weekday"]')).reduce((acc, input) => {
        if (input instanceof HTMLInputElement) {
          const label = input.closest("label")?.querySelector("span")?.textContent?.trim() || input.value;
          acc[input.value] = label;
        }
        return acc;
      }, {});
      return rule.weekdays.map((weekday) => weekdayLabels[weekday] || weekday).join(" ");
    };

    const ruleExpired = (rule) => {
      if (!rule.endDate) {
        return false;
      }
      const now = new Date();
      const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
      const current = `${String(now.getHours()).padStart(2, "0")}:${String(now.getMinutes()).padStart(2, "0")}`;
      if (today > rule.endDate) {
        return true;
      }
      if (today < rule.endDate) {
        return false;
      }
      if (!rule.endTime) {
        return false;
      }
      if (rule.startTime && rule.startTime > rule.endTime) {
        return false;
      }
      return current >= rule.endTime;
    };

    const syncEmptyState = () => {
      const hasRows = list.querySelector("[data-timed-multiplier-rule]") instanceof HTMLElement;
      let empty = list.querySelector("[data-timed-multiplier-empty]");
      if (hasRows) {
        empty?.remove();
        return;
      }
      if (!(empty instanceof HTMLElement)) {
        empty = document.createElement("div");
        empty.className = "timed-multiplier-empty";
        empty.dataset.timedMultiplierEmpty = "true";
        list.appendChild(empty);
      }
      empty.textContent = labels.emptyLabel || "No timed multiplier rules yet.";
    };

    const updateRowDisplay = (row, rule) => {
      const expired = ruleExpired(rule);
      row.classList.toggle("is-expired", expired);
      row.querySelector('[data-rule-display="name"]').textContent = rule.name || labels.unnamedLabel || "Unnamed Rule";
      row.querySelector('[data-rule-display="multiplier"]').textContent = `${rule.value || "1"}x`;
      row.querySelector('[data-rule-display="weekdays"]').textContent = weekdaySummary(rule);
      row.querySelector('[data-rule-display="dates"]').textContent = dateSummary(rule);
      row.querySelector('[data-rule-display="times"]').textContent = timeSummary(rule);
      row.querySelector('[data-rule-display="priority"]').textContent = `${editor.querySelector('[data-rule-input="priority"]')?.closest("label")?.querySelector("span")?.textContent?.trim() || "Priority"} ${rule.priority || "0"}`;
      const status = row.querySelector('[data-rule-display="status"]');
      if (status instanceof HTMLElement) {
        status.className = `pill ${expired ? "tone-bad" : rule.enabled ? "tone-good" : "tone-muted"}`;
        status.textContent = expired
          ? labels.expiredLabel || "Expired"
          : rule.enabled
            ? labels.enabledLabel || "Enabled"
            : labels.disabledLabel || "Disabled";
      }
    };

    const writeRow = (row, rule) => {
      row.dataset.ruleId = rule.id;
      row.querySelectorAll('input[type="hidden"]').forEach((input) => input.remove());
      writeInput(row, "timed_multiplier_id", rule.id, "id");
      writeInput(row, fieldName(rule.id, "name"), rule.name, "name");
      writeInput(row, fieldName(rule.id, "value"), rule.value || "1", "value");
      writeInput(row, fieldName(rule.id, "startDate"), rule.startDate, "startDate");
      writeInput(row, fieldName(rule.id, "endDate"), rule.endDate, "endDate");
      writeInput(row, fieldName(rule.id, "startTime"), rule.startTime, "startTime");
      writeInput(row, fieldName(rule.id, "endTime"), rule.endTime, "endTime");
      writeInput(row, fieldName(rule.id, "priority"), rule.priority || "0", "priority");
      if (rule.enabled && !ruleExpired(rule)) {
        writeInput(row, "timed_multiplier_enabled", rule.id, "enabled");
      }
      rule.weekdays.forEach((weekday) => writeInput(row, fieldName(rule.id, "weekday"), weekday, "weekday"));
      updateRowDisplay(row, rule);
    };

    const createRow = (rule) => {
      const row = document.createElement("div");
      row.className = "timed-multiplier-item";
      row.dataset.timedMultiplierRule = "true";
      const main = document.createElement("div");
      main.className = "timed-multiplier-item-main";
      const head = document.createElement("div");
      head.className = "timed-multiplier-item-head";
      const name = document.createElement("strong");
      name.dataset.ruleDisplay = "name";
      const status = document.createElement("span");
      status.className = "pill";
      status.dataset.ruleDisplay = "status";
      head.append(name, status);
      const meta = document.createElement("div");
      meta.className = "timed-multiplier-meta";
      ["multiplier", "weekdays", "dates", "times", "priority"].forEach((field) => {
        const value = document.createElement("span");
        value.dataset.ruleDisplay = field;
        meta.appendChild(value);
      });
      main.append(head, meta);
      const actions = document.createElement("div");
      actions.className = "timed-multiplier-item-actions";
      const edit = document.createElement("button");
      edit.type = "button";
      edit.className = "ghost";
      edit.dataset.timedMultiplierOpen = "edit";
      edit.textContent = labels.editLabel || "Edit";
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "ghost danger";
      remove.dataset.timedMultiplierRemove = "";
      remove.textContent = labels.deleteLabel || "Delete";
      actions.append(edit, remove);
      row.append(main, actions);
      writeRow(row, rule);
      attachRowEvents(row);
      return row;
    };

    const setEditor = (rule, mode) => {
      inputs.mode.value = mode;
      inputs.id.value = rule.id || "";
      inputs.name.value = rule.name || "";
      inputs.value.value = rule.value || "";
      inputs.startDate.value = rule.startDate || "";
      inputs.endDate.value = rule.endDate || "";
      inputs.startTime.value = rule.startTime || "";
      inputs.endTime.value = rule.endTime || "";
      inputs.priority.value = rule.priority || "";
      inputs.enabled.checked = Boolean(rule.enabled);
      weekdayInputs.forEach((input) => {
        input.checked = (rule.weekdays || []).includes(input.value);
      });
      if (title instanceof HTMLElement) {
        title.textContent =
          mode === "edit" ? inputs.mode.dataset.editTitle || labels.editLabel || "Edit Rule" : inputs.mode.dataset.newTitle || "New Rule";
      }
    };

    const readEditor = () => ({
      id: inputs.id.value || `tm_${Date.now()}_${Math.random().toString(16).slice(2, 8)}`,
      name: inputs.name.value.trim(),
      value: inputs.value.value.trim() || "1",
      startDate: inputs.startDate.value,
      endDate: inputs.endDate.value,
      startTime: inputs.startTime.value,
      endTime: inputs.endTime.value,
      priority: inputs.priority.value.trim() || "0",
      enabled: inputs.enabled.checked,
      weekdays: weekdayInputs.filter((input) => input.checked).map((input) => input.value),
    });

    const focusableElements = () =>
      Array.from(
        modal.querySelectorAll(
          'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href]'
        )
      ).filter((element) => element instanceof HTMLElement && element.offsetParent !== null);

    const closeModal = () => {
      modal.classList.remove("is-visible");
      modal.hidden = true;
      if (!document.querySelector(".settings-overlay.is-visible")) {
        document.body.classList.remove("settings-open");
      }
      if (previousFocus instanceof HTMLElement) {
        previousFocus.focus();
      }
      previousFocus = null;
      editingRow = null;
    };

    const openModal = (mode, row = null) => {
      previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      editingRow = row;
      setEditor(row instanceof HTMLElement ? readRow(row) : { enabled: true, weekdays: [] }, mode);
      modal.hidden = false;
      modal.classList.add("is-visible");
      document.body.classList.add("settings-open");
      window.setTimeout(() => {
        const focusable = focusableElements();
        (modal.querySelector("[data-modal-autofocus]") || focusable[0] || modal).focus();
      }, 0);
    };

    const attachRowEvents = (row) => {
      row.querySelector("[data-timed-multiplier-open]")?.addEventListener("click", () => openModal("edit", row));
      row.querySelector("[data-timed-multiplier-remove]")?.addEventListener("click", async () => {
        if (!(await confirmRemoveRule())) {
          return;
        }
        row.remove();
        syncEmptyState();
      });
    };

    form.querySelectorAll("[data-timed-multiplier-open]").forEach((button) => {
      button.addEventListener("click", () => {
        const row = button.closest("[data-timed-multiplier-rule]");
        openModal(row instanceof HTMLElement ? "edit" : "new", row instanceof HTMLElement ? row : null);
      });
    });
    list.querySelectorAll("[data-timed-multiplier-rule]").forEach((row) => {
      if (row instanceof HTMLElement) {
        updateRowDisplay(row, readRow(row));
      }
    });
    list.querySelectorAll("[data-timed-multiplier-remove]").forEach((button) => {
      button.addEventListener("click", async () => {
        if (!(await confirmRemoveRule())) {
          return;
        }
        button.closest("[data-timed-multiplier-rule]")?.remove();
        syncEmptyState();
      });
    });
    saveButton?.addEventListener("click", () => {
      const rule = readEditor();
      const row = editingRow instanceof HTMLElement ? editingRow : createRow(rule);
      if (!(editingRow instanceof HTMLElement)) {
        list.appendChild(row);
      } else {
        writeRow(row, rule);
      }
      syncEmptyState();
      closeModal();
    });
    modal.querySelectorAll("[data-timed-multiplier-close]").forEach((button) => {
      button.addEventListener("click", (event) => {
        event.preventDefault();
        event.stopPropagation();
        closeModal();
      });
    });
    modal.addEventListener("pointerdown", (event) => {
      event.stopPropagation();
      pointerStartedOnOverlay = event.target === modal;
    });
    modal.addEventListener("click", (event) => {
      event.stopPropagation();
      if (event.target === modal && pointerStartedOnOverlay) {
        closeModal();
      }
      pointerStartedOnOverlay = false;
    });
    modal.addEventListener("keydown", (event) => {
      event.stopPropagation();
      if (event.key === "Escape") {
        event.preventDefault();
        closeModal();
        return;
      }
      if (event.key !== "Tab") {
        return;
      }
      const focusable = focusableElements();
      if (focusable.length === 0) {
        event.preventDefault();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    });
    syncEmptyState();
  });
  };

  initTimedMultiplierEditors();
  initGroupSettingModals();
  initUserDetailsLazyLoad();

  const requestedModal = new URLSearchParams(window.location.search).get("open_modal") || "";
  if (requestedModal) {
    const opener = Array.from(document.querySelectorAll("[data-group-settings-open]")).find(
      (element) => element instanceof HTMLElement && element.dataset.groupSettingsOpen === requestedModal
    );
    if (opener instanceof HTMLButtonElement) {
      opener.click();
      if (window.history && typeof window.history.replaceState === "function") {
        const url = new URL(window.location.href);
        url.searchParams.delete("open_modal");
        window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
      }
    }
  }

  document.querySelectorAll("[data-payment-form]").forEach((form) => {
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    const providerInputs = form.querySelectorAll('input[name="provider"]');
    const error = form.querySelector("[data-payment-error]");
    const loading = form.querySelector("[data-payment-loading]");
    const result = form.querySelector("[data-payment-result]");
    const createFields = form.querySelector("[data-payment-create-fields]");
    const summary = form.querySelector("[data-payment-summary]");
    const summaryProvider = form.querySelector("[data-payment-summary-provider]");
    const summaryPaid = form.querySelector("[data-payment-summary-paid]");
    const summaryReceived = form.querySelector("[data-payment-summary-received]");
    const urlNode = form.querySelector("[data-payment-url]");
    const codeWrap = form.querySelector("[data-payment-code-wrap]");
    const qrNode = form.querySelector("[data-payment-qr]");
    const statusText = form.querySelector("[data-payment-status-text]");
    const submitButton = form.querySelector("[data-payment-submit]");
    let paymentPollTimer = null;
    let activePaymentOrderID = "";
    let activePaymentCompleted = false;
    let activePaymentQRVersion = "";

    const stopPaymentPolling = () => {
      if (paymentPollTimer !== null) {
        window.clearInterval(paymentPollTimer);
        paymentPollTimer = null;
      }
    };

    const cancelActivePaymentOrder = async () => {
      if (!activePaymentOrderID) {
        return;
      }
      const orderID = activePaymentOrderID;
      activePaymentOrderID = "";
      activePaymentCompleted = false;
      activePaymentQRVersion = "";
      stopPaymentPolling();
      const csrfToken = String(new FormData(form).get("csrf_token") || "");
      try {
        await fetch("/payments/cancel", {
          method: "POST",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/json",
            "X-CSRF-Token": csrfToken,
          },
          body: JSON.stringify({ id: orderID }),
          keepalive: true,
        });
      } catch {
        // The server will expire unfinished PersonalPay orders on its own timer.
      }
    };

    const setError = (message) => {
      if (!(error instanceof HTMLElement)) {
        return;
      }
      error.textContent = message || "";
      error.hidden = !message;
    };

    const setPaymentLoading = (isLoading) => {
      form.setAttribute("aria-busy", isLoading ? "true" : "false");
      if (loading instanceof HTMLElement) {
        loading.hidden = !isLoading;
      }
      if (submitButton instanceof HTMLButtonElement) {
        submitButton.disabled = isLoading;
        submitButton.classList.toggle("is-loading", isLoading);
      }
    };

    const resetResult = () => {
      stopPaymentPolling();
      setPaymentLoading(false);
      activePaymentOrderID = "";
      activePaymentCompleted = false;
      activePaymentQRVersion = "";
      if (result instanceof HTMLElement) {
        result.hidden = true;
      }
      if (createFields instanceof HTMLElement) {
        createFields.hidden = false;
      }
      if (urlNode instanceof HTMLAnchorElement) {
        urlNode.hidden = true;
        urlNode.href = "#";
      }
      if (summary instanceof HTMLElement) {
        summary.hidden = true;
      }
      if (summaryProvider instanceof HTMLElement) {
        summaryProvider.textContent = "";
      }
      if (summaryPaid instanceof HTMLElement) {
        summaryPaid.textContent = "";
      }
      if (summaryReceived instanceof HTMLElement) {
        summaryReceived.textContent = "";
      }
      if (codeWrap instanceof HTMLElement) {
        codeWrap.hidden = true;
      }
      if (qrNode instanceof HTMLImageElement) {
        qrNode.hidden = true;
        qrNode.removeAttribute("src");
      }
      if (statusText instanceof HTMLElement) {
        statusText.textContent = statusText.dataset.waitingText || statusText.textContent || "";
        statusText.hidden = statusText.textContent.trim() === "";
      }
      if (submitButton instanceof HTMLButtonElement) {
        submitButton.hidden = false;
        submitButton.disabled = false;
      }
    };

    const resetPaymentFormState = () => {
      resetResult();
      setError("");
    };

    const selectedPaymentLabel = () => {
      const checked = Array.from(providerInputs).find((input) => input instanceof HTMLInputElement && input.checked);
      if (!(checked instanceof HTMLInputElement)) {
        return "";
      }
      const card = checked.closest("label")?.querySelector(".payment-method-card");
      const image = card?.querySelector("img");
      if (image instanceof HTMLImageElement && image.alt.trim()) {
        return image.alt.trim();
      }
      return (card?.textContent || checked.value || "").trim();
    };

    const setPaymentSummary = (providerText, paidText, receivedText) => {
      if (summaryProvider instanceof HTMLElement) {
        summaryProvider.textContent = providerText;
      }
      if (summaryPaid instanceof HTMLElement) {
        summaryPaid.textContent = paidText;
      }
      if (summaryReceived instanceof HTMLElement) {
        summaryReceived.textContent = receivedText;
      }
      if (summary instanceof HTMLElement) {
        summary.hidden = !(providerText || paidText || receivedText);
      }
    };

    const formatSubmittedUSDAmount = (value) => {
      const amount = Number(value);
      if (!Number.isFinite(amount) || amount <= 0) {
        return "";
      }
      return `$${amount.toFixed(2)}`;
    };

    const formatSubmittedCNYAmount = (value) => {
      const amount = Number(value);
      if (!Number.isFinite(amount) || amount <= 0) {
        return "";
      }
      return `¥${amount.toFixed(2)}`;
    };

    const formatOrderReceivedAmount = (order) => {
      const amountNanoUSD = order.AmountNanoUSD ?? order.amount_nano_usd ?? order.amountNanoUSD;
      if (amountNanoUSD === undefined || amountNanoUSD === null || amountNanoUSD === "") {
        return "";
      }
      const amount = Number(amountNanoUSD);
      if (!Number.isFinite(amount) || amount <= 0) {
        return "";
      }
      return `$${(amount / 1000000000).toFixed(2)}`;
    };

    const formatOrderPaymentAmount = (order) => {
      const amountCents = order.ProviderAmountCents ?? order.provider_amount_cents ?? order.providerAmountCents;
      if (amountCents === undefined || amountCents === null || amountCents === "") {
        return "";
      }
      const amount = Number(amountCents);
      if (!Number.isFinite(amount) || amount <= 0) {
        return "";
      }
      return `¥${(amount / 100).toFixed(2)}`;
    };

    providerInputs.forEach((input) => {
      input.addEventListener("change", () => {
        resetResult();
        setError("");
      });
    });
    form.closest(".settings-overlay")?.addEventListener("modal:open", resetPaymentFormState);
    form.closest(".settings-overlay")?.addEventListener("modal:close", () => {
      cancelActivePaymentOrder();
      resetPaymentFormState();
    });

    const paymentCSRFToken = () => String(new FormData(form).get("csrf_token") || "");

    const pollPaymentStatus = (orderID) => {
      stopPaymentPolling();
      let attempts = 0;
      const tick = async () => {
        attempts += 1;
        try {
          const response = await fetch("/payments/refresh", {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "Content-Type": "application/json",
              "X-CSRF-Token": paymentCSRFToken(),
              "X-Requested-With": "fetch",
            },
            body: JSON.stringify({ id: orderID }),
          });
          const payload = await readResponsePayload(response);
          if (!response.ok || payload.status !== "ok") {
            return;
          }
          const providerStatus = String(payload.provider_status || "");
          if (payload.paid === true || payload.order_status === "paid") {
            stopPaymentPolling();
            activePaymentCompleted = true;
            activePaymentOrderID = "";
            activePaymentQRVersion = "";
            if (statusText instanceof HTMLElement) {
              statusText.textContent = statusText.dataset.successText || "Payment successful";
              statusText.hidden = false;
            }
            window.setTimeout(() => window.location.reload(), 900);
            return;
          }
          if (payload.has_code_url === true && qrNode instanceof HTMLImageElement) {
            const codeVersion = String(payload.code_version || "");
            if (!qrNode.getAttribute("src") || codeVersion !== activePaymentQRVersion) {
              const versionQuery = codeVersion ? `&v=${encodeURIComponent(codeVersion)}` : "";
              qrNode.src = `/payments/qr?id=${encodeURIComponent(orderID)}${versionQuery}`;
              activePaymentQRVersion = codeVersion;
            }
            qrNode.hidden = false;
            if (statusText instanceof HTMLElement) {
              statusText.textContent = statusText.dataset.waitingText || statusText.textContent || "";
              statusText.hidden = statusText.textContent.trim() === "";
            }
          }
          if (providerStatus === "paying" && statusText instanceof HTMLElement) {
            statusText.textContent = statusText.dataset.payingText || "Payment in progress";
            statusText.hidden = false;
          }
          if (payload.order_status === "closed" || payload.order_status === "failed") {
            stopPaymentPolling();
            activePaymentOrderID = "";
            activePaymentQRVersion = "";
            if (statusText instanceof HTMLElement) {
              statusText.textContent = providerStatus === "canceled"
                ? statusText.dataset.canceledText || "Payment canceled"
                : statusText.dataset.expiredText || "QR code expired";
              statusText.hidden = false;
            }
            if (qrNode instanceof HTMLImageElement) {
              qrNode.hidden = true;
              qrNode.removeAttribute("src");
            }
            return;
          }
          if (attempts >= 2400) {
            stopPaymentPolling();
          }
        } catch {
          if (attempts >= 2400) {
            stopPaymentPolling();
          }
        }
      };
      paymentPollTimer = window.setInterval(tick, 3000);
      window.setTimeout(tick, 1200);
    };

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      setError("");
      await cancelActivePaymentOrder();
      resetResult();
      setPaymentLoading(true);
      try {
        const formData = new FormData(form);
        const csrfToken = String(formData.get("csrf_token") || "");
        const paymentLabel = selectedPaymentLabel();
        const submittedAmount = String(formData.get("amount_usd") || "").trim();
        const response = await fetch(form.action, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/json",
            "X-CSRF-Token": csrfToken,
          },
          body: JSON.stringify({
            provider: String(formData.get("provider") || ""),
            amount_usd: String(formData.get("amount_usd") || ""),
          }),
        });
        const payload = await readResponsePayload(response);
        if (!response.ok || payload.status !== "ok") {
          throw new Error(payload.message || response.statusText);
        }
        setPaymentLoading(false);
        const order = payload.order || {};
        const providerPayload = payload.payload || {};
        const orderID = order.ID || order.Id || order.id || "";
        const channel = order.Channel || order.channel || "";
        const provider = order.Provider || order.provider || String(formData.get("provider") || "");
        const paymentInputMode = String(payload.mode || form.dataset.paymentMode || "balance_usd");
        const paidText = formatOrderPaymentAmount(order) || (paymentInputMode === "payment_cny" ? formatSubmittedCNYAmount(submittedAmount) : "");
        const receivedText = formatOrderReceivedAmount(order) || (paymentInputMode === "payment_cny" ? "" : formatSubmittedUSDAmount(submittedAmount));
        const payURL =
          order.PayURL ||
          order.pay_url ||
          order.payUrl ||
          providerPayload.pay_url ||
          providerPayload.payUrl ||
          providerPayload.payurl ||
          providerPayload.urlscheme ||
          providerPayload.h5_url ||
          providerPayload.h5Url ||
          "";
        const codeURL =
          order.CodeURL ||
          order.code_url ||
          order.codeUrl ||
          providerPayload.code_url ||
          providerPayload.codeUrl ||
          providerPayload.qrcode ||
          "";

        const waitsForAsyncQRCode = provider === "personalpay";
        if (orderID && (payURL || codeURL || channel === "native" || waitsForAsyncQRCode)) {
          activePaymentOrderID = orderID;
          activePaymentCompleted = false;
          activePaymentQRVersion = "";
        }
        if (urlNode instanceof HTMLAnchorElement && payURL) {
          urlNode.href = payURL;
          urlNode.hidden = false;
        }
        if (codeWrap instanceof HTMLElement && orderID && (codeURL || channel === "native" || waitsForAsyncQRCode)) {
          if (createFields instanceof HTMLElement) {
            createFields.hidden = true;
          }
          codeWrap.hidden = false;
          if (qrNode instanceof HTMLImageElement) {
            if (codeURL || channel === "native") {
              qrNode.src = `/payments/qr?id=${encodeURIComponent(orderID)}`;
              qrNode.hidden = false;
            } else {
              qrNode.hidden = true;
              qrNode.removeAttribute("src");
            }
          }
          if (statusText instanceof HTMLElement && waitsForAsyncQRCode) {
            statusText.textContent = statusText.dataset.waitingText || statusText.textContent || "Waiting for payment";
            statusText.hidden = false;
          }
          if (submitButton instanceof HTMLButtonElement) {
            submitButton.hidden = true;
          }
          pollPaymentStatus(orderID);
        }
        setPaymentSummary(paymentLabel, paidText, receivedText);
        if (result instanceof HTMLElement && (payURL || (orderID && (codeURL || channel === "native" || waitsForAsyncQRCode)))) {
          result.hidden = false;
        }
        if (payURL) {
          window.open(payURL, "_blank", "noopener");
        }
      } catch (error) {
        setPaymentLoading(false);
        setError(error instanceof Error ? error.message : "Request failed");
      } finally {
        if (submitButton instanceof HTMLButtonElement) {
          submitButton.disabled = false;
          submitButton.classList.remove("is-loading");
        }
      }
    });
  });

  initPaymentCancelOrders();
  initPaymentOrderQRModals();

  document.querySelectorAll("[data-password-form]").forEach((form) => {
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    const modal = form.closest(".settings-overlay");
    const error = modal?.querySelector("[data-password-error]");
    const setError = (message) => {
      if (!(error instanceof HTMLElement)) {
        return;
      }
      error.textContent = message || "";
      error.hidden = !message;
    };

    modal?.addEventListener("modal:close", () => {
      setError("");
      form.reset();
    });

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      setError("");
      const submitButton = form.querySelector('button[type="submit"]');
      if (submitButton instanceof HTMLButtonElement) {
        submitButton.disabled = true;
      }
      try {
        const csrfInput = form.querySelector('input[name="csrf_token"]');
        const csrfToken = csrfInput instanceof HTMLInputElement ? csrfInput.value : "";
        const response = await fetch(form.action, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "X-Requested-With": "fetch",
            "X-CSRF-Token": csrfToken,
          },
          body: new FormData(form),
        });
        const payload = await response.json().catch(() => ({}));
        if (!response.ok || payload.status !== "ok") {
          setError(payload.message || response.statusText);
          return;
        }
        if (modal instanceof HTMLElement) {
          const closeButton = modal.querySelector("[data-group-settings-close]");
          if (closeButton instanceof HTMLButtonElement) {
            closeButton.click();
          } else {
            modal.classList.remove("is-visible");
            modal.hidden = true;
            document.body.classList.remove("settings-open");
            modal.dispatchEvent(new CustomEvent("modal:close"));
          }
        }
        toastUI.show({ message: payload.message || "", tone: "ok" });
      } catch (error) {
        setError(error instanceof Error ? error.message : "Request failed");
      } finally {
        if (submitButton instanceof HTMLButtonElement) {
          submitButton.disabled = false;
        }
      }
    });
  });

  initProxyTestButtons();
  initEmailTestButtons();
  initPlanRatioForms();
  initPlanPurchaseForms();

  initImageLab(toastUI, confirmUI);
  initCopySecrets(toastUI);
  initEnhancedSelects();
});
