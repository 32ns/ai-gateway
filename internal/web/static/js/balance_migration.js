const messageFor = (response, fallback) => response?.message || fallback;

export const initBalanceMigration = () => {
  const form = document.querySelector("[data-balance-migration-form]");
  if (!form) return;
  const message = form.querySelector("[data-balance-migration-message]");
  const result = form.querySelector("[data-balance-migration-result]");
  const codeInput = form.querySelector("[data-balance-migration-code]");
  const expiry = form.querySelector("[data-balance-migration-expiry]");
  const submit = form.querySelector("[data-balance-migration-submit]");
  const copy = form.querySelector("[data-balance-migration-copy]");

  const showMessage = (value) => {
    if (!message) return;
    message.textContent = value || "";
    message.hidden = !value;
  };

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (submit) submit.disabled = true;
    showMessage("");
    try {
      const response = await fetch(form.action, {
        method: "POST",
        headers: { Accept: "application/json", "X-Requested-With": "fetch" },
        body: new URLSearchParams(new FormData(form)),
      });
      const payload = await response.json().catch(() => null);
      if (!response.ok) throw new Error(messageFor(payload, "Unable to generate a migration code."));
      if (codeInput) codeInput.value = payload.code || "";
      if (expiry) {
        const expiresAt = new Date(payload.expires_at || "");
        expiry.textContent = Number.isNaN(expiresAt.getTime()) ? "" : `Expires: ${expiresAt.toLocaleString()}`;
      }
      if (result) result.hidden = false;
    } catch (error) {
      showMessage(error?.message || "Unable to generate a migration code.");
    } finally {
      if (submit) submit.disabled = false;
    }
  });

  copy?.addEventListener("click", async () => {
    const value = codeInput?.value || "";
    if (!value) return;
    try {
      await navigator.clipboard.writeText(value);
      showMessage("Migration code copied.");
    } catch {
      codeInput?.select();
      showMessage("Select and copy the migration code.");
    }
  });
};
