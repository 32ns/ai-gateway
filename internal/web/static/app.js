import "./js/app.bundle.js?v=2026070802";
import { initConsoleEvents } from "./js/events.js?v=2026061512";
import { initMCPScopeForms } from "./js/mcp_tokens.js?v=2026052701";
import { initSupportChat } from "./js/support.js?v=2026070803";
import { initBalanceMigration } from "./js/balance_migration.js?v=2026072201";

document.addEventListener("DOMContentLoaded", () => {
  initConsoleEvents();
  initMCPScopeForms();
  initSupportChat();
  initBalanceMigration();
});
