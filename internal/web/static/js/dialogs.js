export { createConfirmUI, createPromptUI };

function createConfirmUI() {
  const body = document.body;
  const defaultTitle = body.dataset.confirmTitle || "Confirm Action";
  const defaultCaption = body.dataset.confirmCaption || "This action cannot be undone.";
  const confirmLabel = body.dataset.confirmAccept || "Confirm";
  const cancelLabel = body.dataset.confirmCancel || "Cancel";

  const overlay = document.createElement("div");
  overlay.className = "confirm-overlay";

  const dialog = document.createElement("div");
  dialog.className = "confirm-dialog";
  dialog.setAttribute("role", "dialog");
  dialog.setAttribute("aria-modal", "true");
  dialog.setAttribute("aria-labelledby", "confirm-title");
  dialog.setAttribute("aria-describedby", "confirm-message");

  const header = document.createElement("div");
  header.className = "confirm-header";

  const headerText = document.createElement("div");
  headerText.className = "confirm-header-text";

  const title = document.createElement("h3");
  title.className = "confirm-title";
  title.id = "confirm-title";
  title.textContent = defaultTitle;

  const caption = document.createElement("p");
  caption.className = "confirm-caption";
  caption.textContent = defaultCaption;

  headerText.appendChild(title);
  headerText.appendChild(caption);
  header.appendChild(headerText);

  const message = document.createElement("div");
  message.className = "confirm-message";
  message.id = "confirm-message";

  const messageText = document.createElement("span");
  messageText.className = "confirm-message-text";

  message.appendChild(messageText);

  const optionField = document.createElement("label");
  optionField.className = "confirm-option checkbox-row";
  optionField.hidden = true;

  const optionInput = document.createElement("input");
  optionInput.type = "checkbox";

  const optionText = document.createElement("span");

  optionField.appendChild(optionInput);
  optionField.appendChild(optionText);

  const actions = document.createElement("div");
  actions.className = "confirm-actions";

  const cancelButton = document.createElement("button");
  cancelButton.type = "button";
  cancelButton.className = "ghost";
  cancelButton.textContent = cancelLabel;

  const confirmButton = document.createElement("button");
  confirmButton.type = "button";
  confirmButton.className = "danger";
  confirmButton.textContent = confirmLabel;

  actions.appendChild(cancelButton);
  actions.appendChild(confirmButton);

  dialog.appendChild(header);
  dialog.appendChild(message);
  dialog.appendChild(optionField);
  dialog.appendChild(actions);
  overlay.appendChild(dialog);
  body.appendChild(overlay);

  let resolver = null;
  let previousFocus = null;

  const close = (accepted) => {
    const optionChecked = !optionField.hidden && optionInput.checked;
    overlay.classList.remove("is-visible");
    body.classList.remove("confirm-open");
    const currentResolver = resolver;
    resolver = null;
    if (previousFocus instanceof HTMLElement) {
      previousFocus.focus();
    }
    previousFocus = null;
    if (currentResolver) {
      currentResolver(accepted ? { accepted: true, optionChecked } : false);
    }
  };

  cancelButton.addEventListener("click", () => close(false));
  confirmButton.addEventListener("click", () => close(true));

  overlay.addEventListener("click", (event) => {
    if (event.target === overlay) {
      close(false);
    }
  });

  overlay.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      close(false);
      return;
    }
    if (event.key !== "Tab") {
      return;
    }

    const focusable = optionField.hidden ? [cancelButton, confirmButton] : [optionInput, cancelButton, confirmButton];
    const currentIndex = focusable.indexOf(document.activeElement);
    if (event.shiftKey) {
      if (currentIndex <= 0) {
        event.preventDefault();
        focusable[focusable.length - 1].focus();
      }
      return;
    }
    if (currentIndex === focusable.length - 1) {
      event.preventDefault();
      focusable[0].focus();
    }
  });

  return {
    open(text, customTitle, options = {}) {
      messageText.textContent = text;
      title.textContent = customTitle || defaultTitle;
      caption.textContent = defaultCaption;
      const tone = String(options.tone || "").trim();
      confirmButton.className = tone === "primary" ? "confirm-primary" : "danger";
      optionInput.checked = false;
      optionText.textContent = String(options.optionLabel || "").trim();
      optionField.hidden = optionText.textContent === "";
      overlay.classList.add("is-visible");
      body.classList.add("confirm-open");
      previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      cancelButton.focus();
      return new Promise((resolve) => {
        resolver = resolve;
      });
    },
  };
}

function createPromptUI() {
  const body = document.body;
  const defaultTitle = "Input Required";
  const confirmLabel = body.dataset.confirmAccept || "Confirm";
  const cancelLabel = body.dataset.confirmCancel || "Cancel";

  const overlay = document.createElement("div");
  overlay.className = "confirm-overlay prompt-overlay";

  const dialog = document.createElement("div");
  dialog.className = "confirm-dialog prompt-dialog";
  dialog.setAttribute("role", "dialog");
  dialog.setAttribute("aria-modal", "true");
  dialog.setAttribute("aria-labelledby", "prompt-title");
  dialog.setAttribute("aria-describedby", "prompt-message");

  const header = document.createElement("div");
  header.className = "confirm-header";

  const headerText = document.createElement("div");
  headerText.className = "confirm-header-text";

  const title = document.createElement("h3");
  title.className = "confirm-title";
  title.id = "prompt-title";
  title.textContent = defaultTitle;

  const caption = document.createElement("p");
  caption.className = "confirm-caption";
  caption.id = "prompt-message";

  headerText.appendChild(title);
  headerText.appendChild(caption);
  header.appendChild(headerText);

  const field = document.createElement("label");
  field.className = "prompt-field";

  const input = document.createElement("input");
  input.className = "prompt-input";
  input.type = "text";
  input.autocomplete = "off";

  field.appendChild(input);

  const actions = document.createElement("div");
  actions.className = "confirm-actions";

  const cancelButton = document.createElement("button");
  cancelButton.type = "button";
  cancelButton.className = "ghost";
  cancelButton.textContent = cancelLabel;

  const submitButton = document.createElement("button");
  submitButton.type = "button";
  submitButton.textContent = confirmLabel;

  actions.appendChild(cancelButton);
  actions.appendChild(submitButton);

  dialog.appendChild(header);
  dialog.appendChild(field);
  dialog.appendChild(actions);
  overlay.appendChild(dialog);
  body.appendChild(overlay);

  let resolver = null;
  let previousFocus = null;

  const close = (value) => {
    overlay.classList.remove("is-visible");
    body.classList.remove("confirm-open");
    const currentResolver = resolver;
    resolver = null;
    if (previousFocus instanceof HTMLElement) {
      previousFocus.focus();
    }
    previousFocus = null;
    if (currentResolver) {
      currentResolver(value);
    }
  };

  cancelButton.addEventListener("click", () => close(null));
  submitButton.addEventListener("click", () => close(input.value));

  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      close(input.value);
    }
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
      return;
    }
    if (event.key !== "Tab") {
      return;
    }

    const focusable = [input, cancelButton, submitButton];
    const currentIndex = focusable.indexOf(document.activeElement);
    if (event.shiftKey) {
      if (currentIndex <= 0) {
        event.preventDefault();
        focusable[focusable.length - 1].focus();
      }
      return;
    }
    if (currentIndex === focusable.length - 1) {
      event.preventDefault();
      focusable[0].focus();
    }
  });

  return {
    open(options) {
      title.textContent = options.title || defaultTitle;
      caption.textContent = options.message || "";
      submitButton.textContent = options.submitLabel || confirmLabel;
      input.type = options.type || "text";
      input.autocomplete = options.autocomplete || "off";
      input.value = "";
      input.placeholder = options.placeholder || "";
      overlay.classList.add("is-visible");
      body.classList.add("confirm-open");
      previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      window.setTimeout(() => input.focus(), 0);
      return new Promise((resolve) => {
        resolver = resolve;
      });
    },
  };
}
