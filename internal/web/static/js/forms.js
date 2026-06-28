export {
  initAccountLoginForms,
  initEmailCodeSenders,
  initEmailProviderFields,
  initPasswordToggles,
  initRegistrationEmailDomains,
  initSettingsTabs,
};

const emailCodeCooldownStoragePrefix = "ag:email-code-cooldown:";

function initPasswordToggles() {
  document.querySelectorAll("[data-password-toggle]").forEach((button) => {
    button.addEventListener("click", () => {
      const field = button.closest(".password-field");
      const input = field?.querySelector("[data-password-toggle-input]");
      if (!(input instanceof HTMLInputElement)) {
        return;
      }
      const showPassword = input.type === "password";
      input.type = showPassword ? "text" : "password";
      const label = showPassword
        ? button.dataset.hideLabel || "Hide password"
        : button.dataset.showLabel || "Show password";
      const showIcon = button.querySelector("[data-password-icon-show]");
      const hideIcon = button.querySelector("[data-password-icon-hide]");
      if (showIcon instanceof SVGElement) {
        showIcon.toggleAttribute("hidden", showPassword);
      }
      if (hideIcon instanceof SVGElement) {
        hideIcon.toggleAttribute("hidden", !showPassword);
      }
      button.setAttribute("aria-label", label);
      button.title = label;
      button.classList.toggle("is-active", showPassword);
    });
  });

}

function initAccountLoginForms() {
  document.querySelectorAll("[data-account-login-form]").forEach((form) => {
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    const select = form.querySelector("[data-account-login-method]");
    if (!(select instanceof HTMLSelectElement)) {
      return;
    }
    const syncAccountLoginFields = () => {
      const method = select.value || "api_key";
      form.querySelectorAll("[data-account-login-panel]").forEach((panel) => {
        if (!(panel instanceof HTMLElement)) {
          return;
        }
        const active = (panel.dataset.accountLoginPanel || "") === method;
        panel.hidden = !active;
        panel.querySelectorAll("input, select, textarea").forEach((control) => {
          if (
            control instanceof HTMLInputElement ||
            control instanceof HTMLSelectElement ||
            control instanceof HTMLTextAreaElement
          ) {
            control.disabled = !active;
          }
        });
      });
    };
    syncAccountLoginFields();
    select.addEventListener("change", syncAccountLoginFields);
  });

}

function initEmailProviderFields() {
  document.querySelectorAll("[data-email-provider-select]").forEach((select) => {
    if (!(select instanceof HTMLSelectElement)) {
      return;
    }
    const form = select.closest("form");
    const syncEmailProviderFields = () => {
      const provider = select.value || "smtp";
      const scope = form || document;
      scope.querySelectorAll("[data-email-provider-field]").forEach((field) => {
        if (!(field instanceof HTMLElement)) {
          return;
        }
        const active = (field.dataset.emailProviderField || "") === provider;
        field.hidden = !active;
        field.querySelectorAll("input, select, textarea, button").forEach((control) => {
          if (
            control instanceof HTMLInputElement ||
            control instanceof HTMLSelectElement ||
            control instanceof HTMLTextAreaElement ||
            control instanceof HTMLButtonElement
          ) {
            control.disabled = !active;
          }
        });
      });
    };
    syncEmailProviderFields();
    select.addEventListener("change", syncEmailProviderFields);
  });

}

function initRegistrationEmailDomains() {
  document.querySelectorAll("[data-registration-email-domain-field]").forEach((field) => {
    if (!(field instanceof HTMLElement)) {
      return;
    }
    const form = field.closest("form");
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    const syncEmail = () => {
      syncRegistrationEmailDomainField(form, field);
    };
    syncEmail();
    const localInput = field.querySelector("[data-registration-email-local]");
    const domainSelect = field.querySelector("[data-registration-email-domain]");
    if (!(localInput instanceof HTMLInputElement) || !(domainSelect instanceof HTMLSelectElement)) {
      return;
    }
    localInput.addEventListener("input", syncEmail);
    domainSelect.addEventListener("change", syncEmail);
    form.addEventListener("submit", syncEmail);
  });

}

function syncRegistrationEmailDomainField(form, field) {
  const localInput = field.querySelector("[data-registration-email-local]");
  const domainSelect = field.querySelector("[data-registration-email-domain]");
  const emailInput = form.querySelector("[data-registration-email]");
  if (
    !(localInput instanceof HTMLInputElement) ||
    !(domainSelect instanceof HTMLSelectElement) ||
    !(emailInput instanceof HTMLInputElement)
  ) {
    return;
  }
  const local = localInput.value.trim();
  const domain = domainSelect.value.trim();
  emailInput.value = local && domain ? `${local}${domain}` : "";
}

function initSettingsTabs() {
  document.querySelectorAll(".settings-tab-layout").forEach((layout) => {
    if (!(layout instanceof HTMLElement)) {
      return;
    }
    const tabs = Array.from(layout.querySelectorAll(".settings-tabs a")).filter(
      (tab) => tab instanceof HTMLAnchorElement && (tab.getAttribute("href") || "").startsWith("#")
    );
    const panels = Array.from(layout.querySelectorAll(".settings-tab-content > .panel")).filter(
      (panel) => panel instanceof HTMLElement
    );
    if (tabs.length === 0 || panels.length === 0) {
      return;
    }
    let activeID = "";
    const activate = (id, updateHash = true) => {
      const target = panels.find((panel) => panel.id === id) || panels[0];
      activeID = target.id;
      panels.forEach((panel) => {
        panel.hidden = panel !== target;
      });
      tabs.forEach((tab) => {
        const active = tab.getAttribute("href") === `#${target.id}`;
        tab.classList.toggle("is-active", active);
        tab.setAttribute("aria-current", active ? "page" : "false");
      });
      if (updateHash && window.location.hash !== `#${target.id}`) {
        history.replaceState(null, "", `#${target.id}`);
      }
    };
    tabs.forEach((tab) => {
      tab.addEventListener("click", (event) => {
        event.preventDefault();
        activate((tab.getAttribute("href") || "").slice(1));
      });
    });
    const form = layout.closest(".system-settings-page")?.querySelector("#system-settings-form");
    if (form instanceof HTMLFormElement) {
      form.addEventListener("submit", () => {
        if (!activeID) {
          return;
        }
        const action = form.getAttribute("action") || window.location.pathname;
        const url = new URL(action, window.location.href);
        url.hash = activeID;
        form.setAttribute("action", `${url.pathname}${url.search}${url.hash}`);
      });
    }
    const initialID = window.location.hash ? window.location.hash.slice(1) : panels[0].id;
    activate(initialID, false);
  });

}

function initEmailCodeSenders(toastUI) {
  document.querySelectorAll("[data-email-code-send]").forEach((button) => {
    if (!(button instanceof HTMLButtonElement) || button.dataset.emailCodeReady === "true") {
      return;
    }
    button.dataset.emailCodeReady = "true";
    const originalText = button.textContent || "";
    restoreEmailCodeCooldown(button, originalText);
    button.addEventListener("click", async (event) => {
      event.preventDefault();
      const form = button.closest("form");
      if (!(form instanceof HTMLFormElement)) {
        return;
      }
      const domainField = form.querySelector("[data-registration-email-domain-field]");
      if (domainField instanceof HTMLElement) {
        syncRegistrationEmailDomainField(form, domainField);
      }
      const emailInput = form.querySelector('input[name="email"]');
      const csrfInput = form.querySelector('input[name="csrf_token"]');
      if (!(emailInput instanceof HTMLInputElement) || !(csrfInput instanceof HTMLInputElement)) {
        return;
      }
      const endpoint = button.dataset.endpoint || "/register/email-code/send";
      const runningText = button.dataset.running || originalText;
      resetEmailCodeButton(button, originalText);
      button.disabled = true;
      setEmailCodeButtonState(button, "sending", runningText);
      const body = new URLSearchParams();
      body.set("email", emailInput.value.trim());
      body.set("csrf_token", csrfInput.value);
      const inviteInput = form.querySelector('input[name="invite_code"]');
      if (inviteInput instanceof HTMLInputElement) {
        body.set("invite_code", inviteInput.value);
      }
      try {
        const response = await fetch(endpoint, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            Accept: "application/json",
            "Content-Type": "application/x-www-form-urlencoded",
            "X-CSRF-Token": csrfInput.value,
            "X-Requested-With": "fetch",
          },
          body,
        });
        const payload = await response.json().catch(() => ({}));
        if (!response.ok || payload.status !== "ok") {
          throw new Error(payload.message || response.statusText);
        }
        const cooldownUntil = storeEmailCodeCooldown(button);
        setEmailCodeButtonState(button, "sent", button.dataset.sent || originalText);
        toastUI.show({ message: payload.message || "", tone: "ok" });
        window.setTimeout(() => {
          startEmailCodeCooldown(button, originalText, cooldownUntil);
        }, 850);
      } catch (error) {
        resetEmailCodeButton(button, originalText);
        toastUI.show({ message: error instanceof Error ? error.message : "Request failed", tone: "error" });
      }
    });
  });

}

function setEmailCodeButtonState(button, state, label) {
  button.classList.remove("is-sending", "is-sent", "is-counting");
  button.classList.add(`is-${state}`);
  button.setAttribute("aria-live", "polite");
  button.setAttribute("aria-busy", state === "sending" ? "true" : "false");
  const text = document.createElement("span");
  text.className = "email-code-button-label";
  text.textContent = label;
  if (state === "sending" || state === "sent") {
    const icon = state === "sending" ? emailCodeSpinnerIcon() : emailCodeCheckIcon();
    button.replaceChildren(icon, text);
    return;
  }
  button.replaceChildren(text);
}

function emailCodeSpinnerIcon() {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "email-code-status-icon email-code-spinner-svg");
  svg.setAttribute("viewBox", "0 0 20 20");
  svg.setAttribute("aria-hidden", "true");
  const track = document.createElementNS("http://www.w3.org/2000/svg", "circle");
  track.setAttribute("class", "email-code-spinner-track");
  track.setAttribute("cx", "10");
  track.setAttribute("cy", "10");
  track.setAttribute("r", "7");
  const arc = document.createElementNS("http://www.w3.org/2000/svg", "circle");
  arc.setAttribute("class", "email-code-spinner-arc");
  arc.setAttribute("cx", "10");
  arc.setAttribute("cy", "10");
  arc.setAttribute("r", "7");
  svg.append(track, arc);
  return svg;
}

function emailCodeCheckIcon() {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "email-code-status-icon email-code-check-svg");
  svg.setAttribute("viewBox", "0 0 20 20");
  svg.setAttribute("aria-hidden", "true");
  const ring = document.createElementNS("http://www.w3.org/2000/svg", "circle");
  ring.setAttribute("class", "email-code-check-ring");
  ring.setAttribute("cx", "10");
  ring.setAttribute("cy", "10");
  ring.setAttribute("r", "7");
  const mark = document.createElementNS("http://www.w3.org/2000/svg", "path");
  mark.setAttribute("class", "email-code-check-mark");
  mark.setAttribute("d", "M6 10.4 8.7 13 14.2 7");
  svg.append(ring, mark);
  return svg;
}

function resetEmailCodeButton(button, label) {
  stopEmailCodeTimer(button);
  clearEmailCodeCooldown(button);
  button.disabled = false;
  button.classList.remove("is-sending", "is-sent", "is-counting");
  button.removeAttribute("aria-busy");
  button.textContent = label;
}

function startEmailCodeCooldown(button, originalText, cooldownUntil = 0) {
  stopEmailCodeTimer(button);
  let expiresAt = Number(cooldownUntil || 0);
  if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) {
    expiresAt = storeEmailCodeCooldown(button);
  }
  button.disabled = true;
  button.classList.remove("is-sending", "is-sent");
  const suffix = button.dataset.countdownSuffix || "";
  const render = () => {
    const remaining = Math.ceil((expiresAt - Date.now()) / 1000);
    if (remaining <= 0) {
      resetEmailCodeButton(button, originalText);
      return false;
    }
    setEmailCodeButtonState(button, "counting", suffix ? `${remaining}s ${suffix}` : `${remaining}s`);
    return true;
  };
  if (!render()) {
    return;
  }
  const timer = window.setInterval(() => {
    render();
  }, 1000);
  button.dataset.emailCodeTimer = String(timer);
}

function stopEmailCodeTimer(button) {
  const existingTimer = Number(button.dataset.emailCodeTimer || 0);
  if (existingTimer) {
    window.clearInterval(existingTimer);
    delete button.dataset.emailCodeTimer;
  }
}

function restoreEmailCodeCooldown(button, originalText) {
  const cooldownUntil = readEmailCodeCooldown(button);
  if (cooldownUntil <= Date.now()) {
    clearEmailCodeCooldown(button);
    return;
  }
  startEmailCodeCooldown(button, originalText, cooldownUntil);
}

function storeEmailCodeCooldown(button) {
  const cooldownUntil = Date.now() + emailCodeCooldownSeconds(button) * 1000;
  try {
    window.localStorage?.setItem(emailCodeCooldownStorageKey(button), String(Math.floor(cooldownUntil)));
  } catch {
    // Storage can be unavailable in private or restricted browser contexts.
  }
  return cooldownUntil;
}

function readEmailCodeCooldown(button) {
  try {
    const raw = window.localStorage?.getItem(emailCodeCooldownStorageKey(button)) || "";
    const cooldownUntil = Number(raw);
    return Number.isFinite(cooldownUntil) ? cooldownUntil : 0;
  } catch {
    return 0;
  }
}

function clearEmailCodeCooldown(button) {
  try {
    window.localStorage?.removeItem(emailCodeCooldownStorageKey(button));
  } catch {
    // Ignore storage failures; the button can still fall back to in-memory state.
  }
}

function emailCodeCooldownStorageKey(button) {
  const endpoint = button.dataset.endpoint || "/register/email-code/send";
  const form = button.closest("form");
  const action = form instanceof HTMLFormElement ? form.getAttribute("action") || window.location.pathname : window.location.pathname;
  return `${emailCodeCooldownStoragePrefix}${emailCodeURLPath(action)}:${emailCodeURLPath(endpoint)}`;
}

function emailCodeURLPath(value) {
  try {
    return new URL(value, window.location.href).pathname;
  } catch {
    return String(value || "");
  }
}

function emailCodeCooldownSeconds(button) {
  const value = Number(button.dataset.cooldownSeconds || 60);
  if (!Number.isFinite(value) || value <= 0) {
    return 60;
  }
  return Math.min(3600, Math.max(1, Math.floor(value)));
}
