export function initCopySecrets(toastUI, root = document) {
  const scope = root && typeof root.querySelectorAll === "function" ? root : document;
  scope.querySelectorAll("[data-copy-secret]").forEach((button) => {
    if (button.dataset.copySecretReady === "true") {
      return;
    }
    button.dataset.copySecretReady = "true";
    button.addEventListener("click", async () => {
      const value = button.dataset.copyValue || "";
      const label = button.dataset.copyLabel || button.getAttribute("aria-label") || "";
      const success = button.dataset.copySuccess || "Copied";
      const failed = button.dataset.copyFailed || "Copy failed";
      let resetTimer = Number(button.dataset.copyResetTimer || 0);
      if (!value) {
        return;
      }

      if (resetTimer) {
        window.clearTimeout(resetTimer);
      }

      try {
        await copyText(value);
        button.classList.add("is-copied");
        button.setAttribute("aria-label", success);
        button.title = success;
        toastUI.show({ message: success, tone: "ok" });
        resetTimer = window.setTimeout(() => {
          button.classList.remove("is-copied");
          button.setAttribute("aria-label", label);
          button.title = label;
          button.dataset.copyResetTimer = "";
        }, 1200);
        button.dataset.copyResetTimer = String(resetTimer);
      } catch (_error) {
        button.setAttribute("aria-label", failed);
        button.title = failed;
        toastUI.show({ message: failed, tone: "error" });
        resetTimer = window.setTimeout(() => {
          button.setAttribute("aria-label", label);
          button.title = label;
          button.dataset.copyResetTimer = "";
        }, 1600);
        button.dataset.copyResetTimer = String(resetTimer);
      }
    });
  });

}

async function copyText(value) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(value);
    return;
  }

  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.top = "-1000px";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) {
    throw new Error("copy failed");
  }
}

