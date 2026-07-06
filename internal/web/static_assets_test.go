package web

import (
	"os"
	"strings"
	"testing"
)

func TestStaticAssetEntryPointsReferenceEmbeddedChildren(t *testing.T) {
	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	if !strings.Contains(string(css), `./css/foundation.css`) ||
		!strings.Contains(string(css), `./css/components.css`) ||
		!strings.Contains(string(css), `./css/theme.css`) ||
		!strings.Contains(string(css), `./css/maintenance.css`) {
		t.Fatalf("app.css does not import the expected layered stylesheets")
	}
	cssBody := string(css)
	foundationIndex := strings.Index(cssBody, `./css/foundation.css`)
	componentsIndex := strings.Index(cssBody, `./css/components.css`)
	themeIndex := strings.Index(cssBody, `./css/theme.css`)
	maintenanceIndex := strings.Index(cssBody, `./css/maintenance.css`)
	if foundationIndex < 0 || componentsIndex < 0 || themeIndex < 0 || maintenanceIndex < 0 ||
		!(foundationIndex < componentsIndex && componentsIndex < themeIndex && themeIndex < maintenanceIndex) {
		t.Fatalf("app.css must import stylesheets as foundation, components, theme, maintenance")
	}
	if !strings.Contains(cssBody, `./css/components.css?v=2026062501`) {
		t.Fatalf("app.css must cache-bust component styles")
	}
	if !strings.Contains(cssBody, `./css/maintenance.css?v=2026062402`) {
		t.Fatalf("app.css must cache-bust table stability styles")
	}

	for _, path := range []string{
		"static/css/foundation.css",
		"static/css/components.css",
		"static/css/theme.css",
		"static/css/maintenance.css",
		"static/js/app.bundle.js",
		"static/js/clipboard.js",
		"static/js/dialogs.js",
		"static/js/forms.js",
		"static/js/image_lab.js",
		"static/js/model_groups.js",
		"static/js/pricing.js",
		"static/js/selects.js",
		"static/js/support_notifications.js",
		"static/js/toast.js",
		"static/claude.svg",
		"static/codex.svg",
	} {
		if _, err := assets.ReadFile(path); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if !strings.Contains(string(js), `./js/app.bundle.js`) {
		t.Fatalf("app.js does not import the bundled application script")
	}
	if !strings.Contains(string(js), `./js/app.bundle.js?v=2026070601`) {
		t.Fatalf("app.js must cache-bust the site message popup date script")
	}
	if !strings.Contains(string(js), `./js/events.js?v=2026061512`) {
		t.Fatalf("app.js must cache-bust the console event script")
	}
	if !strings.Contains(string(js), `./js/support.js?v=2026061516`) {
		t.Fatalf("app.js must cache-bust the support chat script")
	}
	bundle, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	if !strings.Contains(string(bundle), `./selects.js?v=2026061702`) {
		t.Fatalf("app.bundle.js must cache-bust enhanced select descriptions")
	}
	if !strings.Contains(string(bundle), `./toast.js?v=2026070401`) {
		t.Fatalf("app.bundle.js must cache-bust account detection toast behavior")
	}
	selects, err := assets.ReadFile("static/js/selects.js")
	if err != nil {
		t.Fatalf("read selects.js: %v", err)
	}
	if !strings.Contains(string(selects), `selectDescription`) ||
		!strings.Contains(string(selects), `select-option-description`) ||
		!strings.Contains(string(selects), `select-trigger-description`) {
		t.Fatalf("selects.js must render option descriptions")
	}

	layout, err := assets.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	layoutBody := string(layout)
	if !strings.Contains(layoutBody, `/static/app.css?v=2026062501`) {
		t.Fatalf("layout.html must cache-bust the app stylesheet entrypoint")
	}
	if !strings.Contains(layoutBody, `/static/app.js?v=2026070601`) {
		t.Fatalf("layout.html must cache-bust the app module entrypoint")
	}
}

func TestFinancePageIgnoresHighFrequencyBillingRefreshEvents(t *testing.T) {
	events, err := assets.ReadFile("static/js/events.js")
	if err != nil {
		t.Fatalf("read events.js: %v", err)
	}
	body := string(events)
	for _, want := range []string{
		`const financeAutoRefreshIgnoredReasons = new Set(["usage_settled", "usage_released", "usage_account_updated"]);`,
		`source.addEventListener("finance.changed", (event) => {`,
		`if (financeAutoRefreshIgnoredReasons.has(String(payload.reason || "").trim()))`,
		`refreshCurrentPartials(["finance-page"]);`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("events.js missing finance refresh guard %q", want)
		}
	}
}

func TestSupportNotificationsUseConsoleEventsAndSharedDedupe(t *testing.T) {
	events, err := assets.ReadFile("static/js/events.js")
	if err != nil {
		t.Fatalf("read events.js: %v", err)
	}
	eventsBody := string(events)
	for _, want := range []string{
		`./support_notifications.js?v=2026061511`,
		`source.addEventListener("support.message.created"`,
		`source.addEventListener("support.unread_count"`,
		`if (!document.querySelector("[data-support-root]"))`,
		`maybeNotifySupportMessage(payload, admin);`,
		`setSupportUnread(payload.unread_support_messages);`,
	} {
		if !strings.Contains(eventsBody, want) {
			t.Fatalf("events.js missing support notification behavior %q", want)
		}
	}

	support, err := assets.ReadFile("static/js/support.js")
	if err != nil {
		t.Fatalf("read support.js: %v", err)
	}
	supportBody := string(support)
	for _, want := range []string{
		`./support_notifications.js?v=2026061511`,
		`let connectOnOpen = Boolean(!widget);`,
		`widgetToggle?.addEventListener("click", () => {`,
		`ensureConnected();`,
		`const disconnect = () =>`,
		`widgetClose?.addEventListener("click", () => {`,
	} {
		if !strings.Contains(supportBody, want) {
			t.Fatalf("support.js missing lazy chat websocket behavior %q", want)
		}
	}
	for _, unwanted := range []string{
		`new WebSocket(websocketURL("/admin/support/ws"))`,
		`initSupportGlobalNotificationClient`,
	} {
		if strings.Contains(supportBody, unwanted) {
			t.Fatalf("support.js must not keep global admin support websocket behavior %q", unwanted)
		}
	}

	notifications, err := assets.ReadFile("static/js/support_notifications.js")
	if err != nil {
		t.Fatalf("read support_notifications.js: %v", err)
	}
	notificationsBody := string(notifications)
	for _, want := range []string{
		`const supportNotificationChannelName = "ag:support-notifications";`,
		`const supportNotificationStoreKey = "ag:support-notifications-seen-v1";`,
		`const supportNotificationServiceWorkerURL = "/support-notification-sw.js?v=2026061503";`,
		`playSupportNotificationSound();`,
		`.register(supportNotificationServiceWorkerURL, { scope: "/" })`,
		`navigator.serviceWorker.ready`,
		`registration.showNotification(title, options)`,
		`tag: ` + "`support-chat-${encodeURIComponent(String(messageID || Date.now()))}`" + `,`,
		`renotify: true,`,
		`requireInteraction: true,`,
		`icon: "/favicon.ico",`,
		`registration.getNotifications({ tag: options.tag })`,
		`window.__agSupportNotificationLastState`,
		`playSupportNotificationSound({ force: true });`,
		`rememberSupportNotification(message.id);`,
		`releaseSupportNotificationClaim(message.id);`,
		`window.__agSupportNotificationDebug`,
		`window.__agSupportNotificationStatus`,
		`window.__agTestSupportNotification`,
		`new AudioContextClass()`,
		`storage.setItem(claimKey`,
		`new BroadcastChannel(supportNotificationChannelName)`,
		`supportNotificationPermissionPromise`,
		`supportNotificationChannel.postMessage`,
		`setSupportWidgetUnread(count)`,
	} {
		if !strings.Contains(notificationsBody, want) {
			t.Fatalf("support_notifications.js missing shared dedupe behavior %q", want)
		}
	}
	for _, unwanted := range []string{
		`showSupportInPageNotification`,
		`support-in-page-notification`,
		`data-support-notification-test`,
		`data-support-notification-status`,
	} {
		if strings.Contains(notificationsBody, unwanted) {
			t.Fatalf("support_notifications.js must not render in-page notification UI %q", unwanted)
		}
	}
}

func TestSupportChatHasMobileLayoutGuards(t *testing.T) {
	css, err := assets.ReadFile("static/css/components.css")
	if err != nil {
		t.Fatalf("read components.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`@media (max-width: 760px) {`,
		`.workspace:has(.support-workspace) {`,
		`height: calc(100dvh - 58px);`,
		`.support-workspace.admin {`,
		`grid-template-rows: minmax(132px, 32%) minmax(0, 1fr);`,
		`.support-rail {`,
		`display: none;`,
		`.support-widget[data-support-single="true"] .support-widget-panel {`,
		`@media (max-width: 460px) {`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("components.css missing support mobile layout guard %q", want)
		}
	}

	support, err := assets.ReadFile("static/js/support.js")
	if err != nil {
		t.Fatalf("read support.js: %v", err)
	}
	supportBody := string(support)
	for _, want := range []string{
		`supportWidgetUsesMobilePanel()`,
		`window.matchMedia?.("(max-width: 760px)").matches`,
		`left: "",`,
		`height: "",`,
	} {
		if !strings.Contains(supportBody, want) {
			t.Fatalf("support.js missing mobile widget sizing guard %q", want)
		}
	}
}

func TestSupportNotificationDiagnosticsAreNotRendered(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/support_panel.html")
	if err != nil {
		t.Fatalf("read support panel template: %v", err)
	}
	for _, unwanted := range []string{
		`class="support-notification-tools"`,
		`data-support-notification-test`,
		`data-support-notification-status`,
		`测试通知`,
	} {
		if strings.Contains(string(templateBody), unwanted) {
			t.Fatalf("support panel must not render notification diagnostic marker %q", unwanted)
		}
	}

	css, err := assets.ReadFile("static/css/components.css")
	if err != nil {
		t.Fatalf("read components.css: %v", err)
	}
	for _, unwanted := range []string{
		`.support-notification-tools {`,
		`.support-notification-tools button {`,
		`.support-notification-tools span {`,
		`.support-in-page-notifications {`,
		`.support-in-page-notification {`,
	} {
		if strings.Contains(string(css), unwanted) {
			t.Fatalf("components.css must not keep notification diagnostic style %q", unwanted)
		}
	}
}

func TestDefaultDataTablesUseNaturalLayout(t *testing.T) {
	maintenance, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(maintenance)
	dataTableIndex := strings.LastIndex(cssBody, `.data-table {`)
	if dataTableIndex < 0 {
		t.Fatal("maintenance.css missing shared data-table guardrail")
	}
	dataTableRuleEnd := strings.Index(cssBody[dataTableIndex:], `}`)
	if dataTableRuleEnd < 0 {
		t.Fatal("maintenance.css shared data-table guardrail is not closed")
	}
	dataTableRule := cssBody[dataTableIndex : dataTableIndex+dataTableRuleEnd]
	if strings.Contains(dataTableRule, `min-width: max-content;`) {
		t.Fatal("default data tables must not force content-width horizontal scrolling")
	}
	if strings.Contains(dataTableRule, `table-layout: fixed;`) {
		t.Fatal("default data tables must not use fixed layout")
	}
	for _, want := range []string{
		`.table-wrap {`,
		`overflow-x: auto;`,
		`.data-table {`,
		`min-width: 0;`,
		`table-layout: auto;`,
		`.data-table code {`,
		`overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing default table layout guard %q", want)
		}
	}

	payments, err := assets.ReadFile("templates/payments.html")
	if err != nil {
		t.Fatalf("read payments.html: %v", err)
	}
	if !strings.Contains(string(payments), `class="table-wrap payment-orders-table-wrap"`) ||
		!strings.Contains(string(payments), `class="data-table payment-orders-table"`) {
		t.Fatal("payments table must use dedicated order table classes")
	}

	finance, err := assets.ReadFile("templates/finance.html")
	if err != nil {
		t.Fatalf("read finance.html: %v", err)
	}
	financeBody := string(finance)
	ordersTabIndex := strings.Index(financeBody, `{{if eq .FinanceTab "orders"}}`)
	ordersTableIndex := strings.Index(financeBody, `class="table-wrap finance-orders-table-wrap"`)
	if ordersTabIndex < 0 || ordersTableIndex < 0 || ordersTableIndex < ordersTabIndex ||
		!strings.Contains(financeBody, `class="data-table finance-orders-table"`) {
		t.Fatal("finance order table must use dedicated order table classes")
	}
	if !strings.Contains(financeBody, `{{$canConfirmPaid := or (eq (printf "%s" .Status) "closed") (eq (printf "%s" .Status) "failed")}}`) ||
		!strings.Contains(financeBody, `{{if $canConfirmPaid}}`) {
		t.Fatal("finance payment confirmation button must only be shown for closed or failed orders")
	}
	if strings.Contains(financeBody, `{{if not $paidOrder}}`) {
		t.Fatal("finance payment confirmation button must not be shown for every non-paid order")
	}
}

func TestPlanHistoryTableUsesStableColumns(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/plans.html")
	if err != nil {
		t.Fatalf("read plans.html: %v", err)
	}
	if !strings.Contains(string(templateBody), `class="table-wrap plan-history-table-wrap"`) ||
		!strings.Contains(string(templateBody), `class="data-table plan-table plan-history-table"`) {
		t.Fatalf("plans template must use dedicated plan history table classes")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.plan-history-table {`,
		`min-width: 820px;`,
		`table-layout: fixed;`,
		`.plan-history-table-wrap {`,
		`overflow-x: auto;`,
		`.plan-history-table th:nth-child(5),`,
		`width: 200px;`,
		`text-overflow: ellipsis;`,
		`white-space: nowrap;`,
		`.data-table {`,
		`.plan-history-panel .plan-history-table {`,
		`height: auto;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing stable plan history table rule %q", want)
		}
	}
	dataTableIndex := strings.LastIndex(cssBody, `.data-table {`)
	planHistoryIndex := strings.LastIndex(cssBody, `.plan-history-panel .plan-history-table {`)
	if dataTableIndex < 0 || planHistoryIndex < 0 || planHistoryIndex < dataTableIndex {
		t.Fatal("plan history table final sizing rules must appear after shared data-table guardrails")
	}
}

func TestModelTableUsesStableColumns(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/models.html")
	if err != nil {
		t.Fatalf("read models.html: %v", err)
	}
	if !strings.Contains(string(templateBody), `class="table-wrap model-table-wrap"`) ||
		!strings.Contains(string(templateBody), `class="data-table model-table"`) {
		t.Fatal("models template must use dedicated model table classes")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.model-table-wrap {`,
		`overflow-x: auto;`,
		`.model-table {`,
		`min-width: 1080px;`,
		`table-layout: fixed;`,
		`.model-table .cell-stack {`,
		`flex-direction: column;`,
		`.model-table td:nth-child(3) code {`,
		`white-space: normal;`,
		`.model-table td:nth-child(4) .model-group-popover summary {`,
		`width: 100%;`,
		`.model-table tbody tr:not(.model-config-row) td {`,
		`height: auto;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing model table rule %q", want)
		}
	}
	modelTableIndex := strings.LastIndex(cssBody, `.model-table {`)
	dataTableIndex := strings.LastIndex(cssBody, `.data-table {`)
	if modelTableIndex < 0 || dataTableIndex < 0 || modelTableIndex < dataTableIndex {
		t.Fatal("model table final sizing rules must appear after shared data-table guardrails")
	}
	if strings.Contains(string(templateBody), `<th>{{t "source"}}</th>`) {
		t.Fatal("models template must not render source column")
	}
}

func TestAdminPlanSubscriptionTableUsesStableColumns(t *testing.T) {
	templateBodyBytes, err := assets.ReadFile("templates/admin_plans.html")
	if err != nil {
		t.Fatalf("read admin_plans.html: %v", err)
	}
	templateBody := string(templateBodyBytes)
	for _, want := range []string{
		`class="table-wrap plan-admin-table-wrap plan-subscription-detail-table-wrap"`,
		`class="data-table plan-table plan-subscription-detail-table"`,
		`class="settings-actions plan-usage-actions"`,
		`action="/admin/plans/entitlements/cancel"`,
		`data-confirm="{{t "confirm_cancel_plan_entitlement"}}"`,
		`data-confirm-tone="danger"`,
	} {
		if !strings.Contains(templateBody, want) {
			t.Fatalf("admin plans template missing stable subscription table marker %q", want)
		}
	}
	detailTableIndex := strings.Index(templateBody, `class="data-table plan-table plan-subscription-detail-table"`)
	usageActionsIndex := strings.Index(templateBody, `class="settings-actions plan-usage-actions"`)
	cancelFormIndex := strings.Index(templateBody, `action="/admin/plans/entitlements/cancel"`)
	if detailTableIndex < 0 || usageActionsIndex < 0 || cancelFormIndex < 0 || usageActionsIndex < detailTableIndex || cancelFormIndex < usageActionsIndex {
		t.Fatal("plan cancel form must live in the plan usage details actions area")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.plan-subscription-detail-table-wrap {`,
		`.plan-subscription-detail-table {`,
		`table-layout: fixed;`,
		`.plan-usage-actions {`,
		`justify-content: flex-end;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing stable plan subscription table rule %q", want)
		}
	}
	dataTableIndex := strings.LastIndex(cssBody, `.data-table {`)
	planSubscriptionIndex := strings.LastIndex(cssBody, `.plan-subscription-detail-table {`)
	if dataTableIndex < 0 || planSubscriptionIndex < 0 || planSubscriptionIndex < dataTableIndex {
		t.Fatal("plan subscription table final sizing rules must appear after shared data-table guardrails")
	}
}

func TestAuditTableUsesStableColumns(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/audit.html")
	if err != nil {
		t.Fatalf("read audit.html: %v", err)
	}
	if !strings.Contains(string(templateBody), `class="table-wrap audit-table-wrap"`) ||
		!strings.Contains(string(templateBody), `class="data-table audit-table"`) {
		t.Fatal("audit.html must mark audit table with stable table classes")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.audit-table-wrap {`,
		`overflow-x: auto;`,
		`.audit-table {`,
		`min-width: 1120px;`,
		`table-layout: fixed;`,
		`.audit-table th,`,
		`height: 46px;`,
		`.audit-col-details,`,
		`.audit-detail-cell {`,
		`max-height: 26px;`,
		`text-overflow: ellipsis;`,
		`white-space: nowrap;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing stable audit table rule %q", want)
		}
	}
	dataTableIndex := strings.LastIndex(cssBody, `.data-table {`)
	auditTableIndex := strings.LastIndex(cssBody, `.audit-table {`)
	if dataTableIndex < 0 || auditTableIndex < 0 || auditTableIndex < dataTableIndex {
		t.Fatal("audit table final sizing rules must appear after shared data-table guardrails")
	}
}

func TestUsageAccountWarningHasStableStyle(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/usage_logs.html")
	if err != nil {
		t.Fatalf("read usage_logs.html: %v", err)
	}
	if !strings.Contains(string(templateBody), `class="usage-account-warning"`) {
		t.Fatal("usage logs template must render the account warning marker class")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.usage-account-warning {`,
		`display: inline-grid;`,
		`border-radius: 50%;`,
		`color: var(--warn);`,
		`cursor: help;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing usage account warning rule %q", want)
		}
	}
}

func TestUsageLogsUseMobileCardTableLayout(t *testing.T) {
	templateBody, err := assets.ReadFile("templates/usage_logs.html")
	if err != nil {
		t.Fatalf("read usage_logs.html: %v", err)
	}
	body := string(templateBody)
	for _, want := range []string{
		`class="usage-logs-page" data-partial="usage-logs-page"`,
		`class="table-wrap usage-log-table-wrap"`,
		`class="data-table usage-log-table"`,
		`class="usage-col-time"`,
		`class="usage-col-user"`,
		`class="usage-col-account"`,
		`class="usage-col-input"`,
		`data-label="{{t "at"}}"`,
		`data-label="{{t "client"}}"`,
		`data-label="{{t "model"}}"`,
		`data-label="{{t "status"}}"`,
		`data-label="{{t "cost"}}"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("usage logs template missing mobile table marker %q", want)
		}
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.usage-log-table-wrap {`,
		`.usage-log-table {`,
		`width: max-content;`,
		`min-width: 100%;`,
		`table-layout: auto;`,
		`.usage-log-table .usage-col-time {`,
		`min-width: 150px;`,
		`.usage-log-table .usage-col-user {`,
		`min-width: 140px;`,
		`.usage-log-table .usage-col-input {`,
		`min-width: 154px;`,
		`.usage-log-table .usage-col-cost {`,
		`min-width: 112px;`,
		`.usage-log-table .usage-col-input .cell-stack {`,
		`display: inline-flex;`,
		`white-space: nowrap;`,
		`@media (max-width: 640px) {`,
		`.usage-log-table thead {`,
		`display: none;`,
		`.usage-log-table td::before {`,
		`content: attr(data-label);`,
		`grid-template-columns: minmax(86px, 32%) minmax(0, 1fr);`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing usage logs mobile table rule %q", want)
		}
	}
}

func TestAccountCardsHaveMobileLayoutGuards(t *testing.T) {
	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`/* Account pool controls need a dedicated phone layout;`,
		`.resource-card.account-card {`,
		`grid-template-columns: 24px minmax(0, 1fr);`,
		`"select head"`,
		`.account-pool-grid {`,
		`grid-template-columns: repeat(3, minmax(84px, 1fr));`,
		`.account-pool-card span {`,
		`font-variant-numeric: tabular-nums;`,
		`.resource-card.account-card .cell-stack {`,
		`grid-template-columns: minmax(0, 1fr) auto;`,
		`.resource-card.account-card .quota-window-row {`,
		`grid-template-columns: minmax(0, 1fr);`,
		`row-gap: 4px;`,
		`.resource-card.account-card .quota-progress > :last-child {`,
		`padding-bottom: 10px;`,
		`.resource-card.account-card .action-row {`,
		`grid-template-columns: repeat(auto-fit, minmax(84px, 1fr));`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing account card mobile layout guard %q", want)
		}
	}
}

func TestClientsTemplateReferencesServerAddressIcons(t *testing.T) {
	templateBytes, err := assets.ReadFile("templates/clients.html")
	if err != nil {
		t.Fatalf("read clients.html: %v", err)
	}
	templateBody := string(templateBytes)
	for _, want := range []string{
		`class="server-address-label"`,
		`class="server-address-icon-frame"`,
		`src="/static/claude.svg?v=2026053101"`,
		`src="/static/codex.svg?v=2026053101"`,
	} {
		if !strings.Contains(templateBody, want) {
			t.Fatalf("clients template missing %q", want)
		}
	}
}

func TestAppBundleInitializesDeferredPartials(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, `./image_lab.js?v=2026070601`) {
		t.Fatalf("app.bundle.js must cache-bust image_lab.js")
	}
	if !strings.Contains(body, `const initDeferredPartials =`) ||
		!strings.Contains(body, `initDeferredPartials();`) ||
		!strings.Contains(body, `initAjaxLinks();`) {
		t.Fatalf("app.bundle.js must initialize deferred partials and ajax links on first page load")
	}
}

func TestStatusPagesUseAutoRefreshPartials(t *testing.T) {
	translations, err := os.ReadFile("i18n_translations.go")
	if err != nil {
		t.Fatalf("read i18n_translations.go: %v", err)
	}
	translationsBody := string(translations)
	if strings.Contains(translationsBody, `"monitor_due_now":                             "\u7acb\u5373\u68c0\u6d4b"`) {
		t.Fatal("monitor_due_now must not reuse the manual run button text")
	}

	statusTemplate, err := assets.ReadFile("templates/status.html")
	if err != nil {
		t.Fatalf("read status.html: %v", err)
	}
	statusBody := string(statusTemplate)
	for _, want := range []string{
		`{{define "status_page_fragment"}}`,
		`class="status-page" data-partial="status-page" data-status-auto-refresh`,
		`class="status-monitor-title"`,
		`class="status-monitor-subtitle"`,
		`class="monitor-availability-value {{monitorAvailabilityClass .}}"`,
		`class="monitor-next-check"`,
		`class="monitor-countdown-spinner"`,
		`data-monitor-countdown data-next-check-at="{{monitorNextCheckUnix .}}"`,
		`{{t "monitor_next_check"}}`,
	} {
		if !strings.Contains(statusBody, want) {
			t.Fatalf("status.html missing auto-refresh layout marker %q", want)
		}
	}
	if strings.Contains(statusBody, `<p class="panel-note">{{$target.AccountGroup}} / {{$target.Model}}</p>`) {
		t.Fatal("status card must not render group/model as a second-line panel note")
	}

	adminTemplate, err := assets.ReadFile("templates/admin_status.html")
	if err != nil {
		t.Fatalf("read admin_status.html: %v", err)
	}
	adminBody := string(adminTemplate)
	for _, want := range []string{
		`{{define "admin_status_summary_fragment"}}`,
		`{{define "admin_status_targets_fragment"}}`,
		`data-partial="admin-status-summary" data-status-auto-refresh`,
		`data-partial="admin-status-targets" data-status-auto-refresh`,
		`class="status-monitor-title status-monitor-title-table"`,
		`class="monitor-details monitor-details-expanded"`,
		`class="monitor-run-indicator"`,
		`class="monitor-run-spinner"`,
		`class="monitor-availability-value {{monitorAvailabilityClass .}}"`,
		`class="monitor-next-check"`,
		`class="monitor-countdown-spinner"`,
		`data-running-text="{{t "monitor_checking"}}"`,
		`data-monitor-countdown data-next-check-at="{{monitorNextCheckUnix .}}"`,
		`{{t "monitor_next_check"}}`,
	} {
		if !strings.Contains(adminBody, want) {
			t.Fatalf("admin_status.html missing auto-refresh layout marker %q", want)
		}
	}
	for _, unwanted := range []string{
		`<details class="monitor-details">`,
		`<summary>{{t "monitor_history"}}`,
	} {
		if strings.Contains(adminBody, unwanted) {
			t.Fatalf("admin_status.html should render monitor history expanded, found %q", unwanted)
		}
	}

	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	jsBody := string(js)
	for _, want := range []string{
		`const initStatusAutoRefresh =`,
		`[data-status-auto-refresh][data-partial]`,
		`refreshStatusAutoPartial(target)`,
		`"X-Ajax-Partial": partial`,
		`initStatusAutoRefresh();`,
		`const initMonitorCountdowns =`,
		`[data-monitor-countdown]`,
		`const monitorCountdownSpinnerHTML =`,
		`initMonitorCountdowns();`,
		`const showMonitorRunPending =`,
		`monitor-run-button`,
		`is-monitor-running`,
	} {
		if !strings.Contains(jsBody, want) {
			t.Fatalf("app.bundle.js missing status auto-refresh behavior %q", want)
		}
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`.dashboard-status-panel {`,
		`padding-block: 14px;`,
		`.dashboard-status-list {`,
		`grid-template-columns: minmax(0, 1fr);`,
		`.model-status-card {`,
		`grid-template-columns: minmax(0, 1fr);`,
		`.dashboard-status-card {`,
		`padding: 8px 0 10px;`,
		`.model-status-card-head {`,
		`justify-content: space-between;`,
		`.model-status-card-state {`,
		`.model-status-metrics {`,
		`display: flex;`,
		`justify-content: space-between;`,
		`.model-status-timeline {`,
		`justify-content: flex-end;`,
		`.status-monitor-title {`,
		`align-items: baseline;`,
		`.status-monitor-subtitle {`,
		`.status-card .panel-head > .pill {`,
		`.status-monitor-grid .status-card .model-status-card-head {`,
		`.status-monitor-grid .status-card-metrics {`,
		`grid-template-columns: repeat(4, minmax(0, 1fr));`,
		`.status-monitor-grid .monitor-timeline {`,
		`justify-content: flex-end;`,
		`gap: 3px;`,
		`overflow: hidden;`,
		`.status-monitor-grid .monitor-timeline-bar {`,
		`flex: 0 0 3px;`,
		`height: 20px;`,
		`border-radius: 999px;`,
		`background: currentColor;`,
		`.status-monitor-grid .monitor-timeline-bar:hover {`,
		`opacity: 1;`,
		`.status-monitor-grid .monitor-timeline-bar.tone-warn {`,
		`height: 15px;`,
		`color: #F59E0B;`,
		`.status-monitor-grid .monitor-timeline-bar.tone-bad {`,
		`height: 9px;`,
		`.hero.compact {`,
		`align-items: stretch;`,
		`.hero.compact > * {`,
		`text-align: left;`,
		`.status-admin-page .monitor-table-wrap {`,
		`overflow: hidden;`,
		`.status-admin-page .monitor-table {`,
		`table-layout: fixed;`,
		`.status-admin-page .monitor-table th:nth-child(1),`,
		`border-collapse: collapse;`,
		`border-bottom: 1px solid var(--line);`,
		`padding: 8px 10px 12px;`,
		`.status-admin-page .monitor-table tbody tr:not(.monitor-detail-row) {`,
		`min-width: 0;`,
		`grid-template-columns: minmax(0, 1fr);`,
		`width: 100%;`,
		`content: attr(data-label);`,
		`.status-admin-page .monitor-table .monitor-next-check {`,
		`grid-template-columns: repeat(2, minmax(0, 1fr));`,
		`grid-column: 1 / -1;`,
		`.status-admin-page .monitor-attempt-list {`,
		`.status-admin-page .monitor-table .monitor-detail-row td:nth-child(1) {`,
		`.status-admin-page .monitor-detail-row .monitor-timeline {`,
		`grid-template-columns: max-content minmax(64px, 0.8fr) minmax(0, 1fr);`,
		`.status-admin-page .monitor-attempt > :nth-child(3) {`,
		`padding: 10px 12px 12px;`,
		`border-radius: 0 0 8px 8px;`,
		`.status-admin-page .monitor-details-expanded {`,
		`overflow: hidden;`,
		`.monitor-availability-good {`,
		`.monitor-availability-warn {`,
		`.monitor-availability-bad {`,
		`.status-monitor-grid .status-card-metrics .kv strong.monitor-availability-good,`,
		`.monitor-countdown-spinner {`,
		`display: inline-block;`,
		`flex: 0 0 auto;`,
		`transform-origin: 50% 50%;`,
		`.status-admin-page .monitor-run-spinner {`,
		`animation: monitor-run-spin 1200ms linear infinite !important;`,
		`@keyframes monitor-run-spin`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing status title layout rule %q", want)
		}
	}

	dashboard, err := assets.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	dashboardBody := string(dashboard)
	for _, want := range []string{
		`{{if .DashboardStatus.Targets}}`,
		`<section class="panel dashboard-status-panel"`,
		`{{t "dashboard_model_status"}}`,
		`{{$dashboardStatus.Summary.OK}}`,
		`model-status-card dashboard-status-card`,
		`model-status-card-head`,
		`model-status-metrics`,
		`href="/status"`,
		`class="monitor-timeline model-status-timeline"`,
	} {
		if !strings.Contains(dashboardBody, want) {
			t.Fatalf("dashboard.html missing dashboard status panel markup %q", want)
		}
	}
	if strings.Contains(dashboardBody, `dashboard_model_status_hint`) {
		t.Fatalf("dashboard.html should not render the dashboard status helper text")
	}

}

func TestAppBundleStoresPublicPopupReadsInBrowser(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	for _, want := range []string{
		`const browserReadPrefix = "ag:site-message-popup-read:";`,
		`window.localStorage?.getItem(browserReadPrefix + key) === "1"`,
		`window.localStorage?.setItem(browserReadPrefix + key, "1")`,
		`readMode === "browser"`,
		`const popupRow = form?.querySelector("[data-message-popup-row]");`,
		`const isWebsiteMessage = mode === "website";`,
		`popupRow.hidden = isWebsiteMessage;`,
		`popup.disabled = isWebsiteMessage;`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.bundle.js missing browser popup read behavior %q", want)
		}
	}
}

func TestAppBundleShowsLoadingOnAjaxAndSubmitControls(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	for _, want := range []string{
		`const controlLoadingState = new WeakMap();`,
		`const startControlLoading = (control, options = {}) =>`,
		`const stopControlLoading = (control) =>`,
		`const autoSubmitLoadingControl = (control) =>`,
		`autoSubmitControls.set(form, autoSubmitLoadingControl(control))`,
		`const loadingControl = autoSubmitLoadingControl(control);`,
		`if (form.hasAttribute("data-long-running-form"))`,
		`beginLongRunningSubmit(form, null, loadingControl);`,
		`startControlLoading(loadingControl, { disable: false })`,
		`const submitAjaxForm = async (form, submitter, trigger = submitter) =>`,
		`return loadAjaxTargets(formGETURL(form, submitter), targets, partial, trigger)`,
		`if (controlLoadingState.has(link))`,
		`loadAjaxTargets(new URL(link.href, window.location.href), targets, partial, link)`,
		`startControlLoading(loadingControl, { disable: false })`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.bundle.js missing loading behavior %q", want)
		}
	}

	css, err := assets.ReadFile("static/css/components.css")
	if err != nil {
		t.Fatalf("read components.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{
		`a.is-loading,`,
		`a.is-loading::before,`,
		`animation: payment-loading-spin 780ms linear infinite;`,
	} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("components.css missing loading style %q", want)
		}
	}
}

func TestAppBundlePlanPriceSyncIsManual(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	for _, want := range []string{
		`const syncButton = form.querySelector("[data-plan-price-sync-button]");`,
		`syncButton.addEventListener("click", syncPrice);`,
		`priceInput.value = formatPlanRatioAmount((quota * periodCount * ratio.price) / ratio.quota);`,
		`const rounded = Math.round((amount + Number.EPSILON) * 100) / 100;`,
		`return rounded.toFixed(2);`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.bundle.js missing manual plan price sync behavior %q", want)
		}
	}
	for _, unwanted := range []string{
		`quotaInput.addEventListener("input", syncPrice);`,
		`periodCountInput.addEventListener("input", syncPrice);`,
		`groupSelect.addEventListener("change", syncPrice);`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("app.bundle.js must not auto-sync plan price with %q", unwanted)
		}
	}
}

func TestAppBundleKeepsCompletedAccountBatchResultVisible(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, `const terminal = payload.state === "completed" || payload.state === "cancelled";`) ||
		!strings.Contains(body, `showAccountBatchTerminalToast(payload);`) ||
		!strings.Contains(body, `const refreshIDs = accountBatchTerminalRefreshIDs(payload);`) ||
		!strings.Contains(body, `refreshAccountBatchPartials(refreshIDs).finally(() => clearAccountBatchRefreshState(payload));`) {
		t.Fatalf("app.bundle.js must keep terminal account batch results visible while refreshing rows")
	}
	if strings.Contains(body, `refreshAccountBatchPartials().finally(() => {
            accountBatchRefreshedItems.delete(String(payload.id || ""));`) ||
		strings.Contains(body, `refreshAccountBatchPartials().finally(() => {
        accountBatchRefreshedItems.delete(String(payload.id || ""));`) {
		t.Fatalf("app.bundle.js must not replace the full accounts panel after terminal account batch updates")
	}
	if strings.Contains(body, `message: panel.dataset.refreshingMessage || ""`) {
		t.Fatalf("app.bundle.js must not replace completed account batch result with refreshing text")
	}
}

func TestAccountDetectionToastsUseRequestedLifetimes(t *testing.T) {
	toastJS, err := assets.ReadFile("static/js/toast.js")
	if err != nil {
		t.Fatalf("read toast.js: %v", err)
	}
	toastBody := string(toastJS)
	for _, want := range []string{
		`const duration = Number(options?.duration ?? options?.durationMs ?? 5000);`,
		`const sticky = Boolean(options?.sticky) || duration <= 0;`,
		`notice.dataset.toastSticky === "true"`,
		`options.duration = duration;`,
	} {
		if !strings.Contains(toastBody, want) {
			t.Fatalf("toast.js missing lifetime behavior %q", want)
		}
	}

	accountsTemplate, err := assets.ReadFile("templates/accounts.html")
	if err != nil {
		t.Fatalf("read accounts.html: %v", err)
	}
	accountsBody := string(accountsTemplate)
	for _, want := range []string{
		`data-toast-duration="{{if eq .AccountTestTone "good"}}3000{{else}}0{{end}}"`,
		`data-toast-duration="{{if eq .AccountBatchTone "good"}}3000{{else}}0{{end}}"`,
		`data-toast-sticky="true"`,
	} {
		if !strings.Contains(accountsBody, want) {
			t.Fatalf("accounts.html missing account detection toast option %q", want)
		}
	}

	bundle, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	bundleBody := string(bundle)
	for _, want := range []string{
		`if (String(payload?.action || "") === "test") {`,
		`options.sticky = true;`,
		`options.duration = 3000;`,
	} {
		if !strings.Contains(bundleBody, want) {
			t.Fatalf("app.bundle.js missing account batch detection toast option %q", want)
		}
	}
}

func TestPaymentPollingUsesStateChangingRefreshEndpoint(t *testing.T) {
	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	if strings.Contains(body, `payments/status?id=${encodeURIComponent(orderID)}&refresh=1`) ||
		strings.Contains(body, `/payments/status?id=`+`${encodeURIComponent(orderID)}&refresh=1`) {
		t.Fatalf("payment polling must not refresh orders through GET /payments/status")
	}
	if strings.Count(body, `fetch("/payments/refresh"`) < 2 ||
		!strings.Contains(body, `"X-CSRF-Token": modal.dataset.paymentCancelCsrf || ""`) ||
		!strings.Contains(body, `"X-CSRF-Token": paymentCSRFToken()`) {
		t.Fatalf("payment polling must POST to /payments/refresh with CSRF protection")
	}
}

func TestHomeTemplateReferencesSupportedToolIcons(t *testing.T) {
	templateBytes, err := assets.ReadFile("templates/home.html")
	if err != nil {
		t.Fatalf("read home.html: %v", err)
	}
	templateBody := string(templateBytes)
	for _, want := range []string{
		`class="home-tool-rail`,
		`src="/static/claude.svg?v=2026053101"`,
		`src="/static/codex.svg?v=2026053101"`,
		`class="home-tool-chip home-tool-chip-link" href="/images"`,
		`{{t "home_tool_image"}}`,
		`{{t "home_tool_signin_required"}}`,
		`{{t "home_tools_caption"}}`,
		`{{t "home_tool_fast"}}`,
	} {
		if !strings.Contains(templateBody, want) {
			t.Fatalf("home template missing %q", want)
		}
	}
}

func TestImageLabScriptPreservesRunningTaskStatus(t *testing.T) {
	js, err := assets.ReadFile("static/js/image_lab.js")
	if err != nil {
		t.Fatalf("read image_lab.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, `case "running":`) || !strings.Contains(body, `startJobPoller(run.id)`) {
		t.Fatalf("image_lab.js must keep running server tasks polling")
	}
	if !strings.Contains(body, `/images/api/jobs`) || !strings.Contains(body, `正在加载后台任务`) {
		t.Fatalf("image_lab.js must show and fetch server tasks before bootstrap completes")
	}
	if !strings.Contains(body, `tasksLoading`) || !strings.Contains(body, `visibilitychange`) || !strings.Contains(body, `正在同步后台任务`) {
		t.Fatalf("image_lab.js must show task loading state when returning to the page")
	}
}

func TestImageLabScriptFiltersForeignJobSnapshots(t *testing.T) {
	js, err := assets.ReadFile("static/js/image_lab.js")
	if err != nil {
		t.Fatalf("read image_lab.js: %v", err)
	}
	body := string(js)
	for _, want := range []string{`currentUserID`, `snapshotBelongsToCurrentUser`, `snapshot.userId`, `snapshot.user_id`} {
		if !strings.Contains(body, want) {
			t.Fatalf("image_lab.js must ignore foreign job snapshots; missing %q", want)
		}
	}
}

func TestEmailCodeSenderDoesNotSubmitRegisterForm(t *testing.T) {
	js, err := assets.ReadFile("static/js/forms.js")
	if err != nil {
		t.Fatalf("read forms.js: %v", err)
	}
	body := string(js)
	for _, want := range []string{
		`event.preventDefault()`,
		`syncRegistrationEmailDomainField(form, domainField)`,
		`body.set("email", emailInput.value.trim())`,
		`setEmailCodeButtonState(button, "sending", runningText)`,
		`setEmailCodeButtonState(button, "sent", button.dataset.sent || originalText)`,
		`emailCodeSpinnerIcon()`,
		`emailCodeCheckIcon()`,
		`restoreEmailCodeCooldown(button, originalText)`,
		`const cooldownUntil = storeEmailCodeCooldown(button)`,
		`startEmailCodeCooldown(button, originalText, cooldownUntil)`,
		`window.localStorage?.setItem(emailCodeCooldownStorageKey(button), String(Math.floor(cooldownUntil)))`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("forms.js missing %q", want)
		}
	}

	registerTemplate, err := assets.ReadFile("templates/register.html")
	if err != nil {
		t.Fatalf("read register.html: %v", err)
	}
	templateBody := string(registerTemplate)
	if strings.Count(templateBody, `data-email-code-send`) == 0 {
		t.Fatal("register template missing email code sender buttons")
	}
	parts := strings.Split(templateBody, `data-email-code-send`)
	for i := 0; i < len(parts)-1; i++ {
		prefix := parts[i]
		if len(prefix) > 160 {
			prefix = prefix[len(prefix)-160:]
		}
		if !strings.Contains(prefix, `type="button"`) {
			t.Fatalf("email code sender button %d is missing type=\"button\"", i+1)
		}
	}
	if strings.Count(templateBody, `data-cooldown-seconds="{{.EmailSendCooldownSeconds}}"`) != strings.Count(templateBody, `data-email-code-send`) ||
		strings.Count(templateBody, `data-sent="{{t "email_code_sent_short"}}"`) != strings.Count(templateBody, `data-email-code-send`) ||
		strings.Count(templateBody, `data-countdown-suffix="{{t "email_code_countdown_suffix"}}"`) != strings.Count(templateBody, `data-email-code-send`) {
		t.Fatal("register email code buttons must include sent text and cooldown metadata")
	}

	css, err := assets.ReadFile("static/css/maintenance.css")
	if err != nil {
		t.Fatalf("read maintenance.css: %v", err)
	}
	cssBody := string(css)
	for _, want := range []string{`.email-code-spinner-svg`, `.email-code-check-mark`, `@keyframes email-code-spin`, `@keyframes email-code-check-pop`, `@keyframes email-code-check-draw`} {
		if !strings.Contains(cssBody, want) {
			t.Fatalf("maintenance.css missing %q", want)
		}
	}
}

func TestSettingsTestButtonsUseAssociatedForm(t *testing.T) {
	templateBodyBytes, err := assets.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	templateBody := string(templateBodyBytes)
	for _, marker := range []string{`data-proxy-test`, `data-email-test`} {
		index := strings.Index(templateBody, marker)
		if index < 0 {
			t.Fatalf("settings template missing %s", marker)
		}
		buttonStart := strings.LastIndex(templateBody[:index], `<button`)
		buttonEnd := strings.Index(templateBody[index:], `</button>`)
		if buttonStart < 0 || buttonEnd < 0 {
			t.Fatalf("settings template missing complete button for %s", marker)
		}
		buttonBlock := templateBody[buttonStart : index+buttonEnd]
		if !strings.Contains(buttonBlock, `type="button"`) || !strings.Contains(buttonBlock, `form="system-settings-form"`) {
			t.Fatalf("%s button must be a non-submit button associated with system-settings-form", marker)
		}
	}

	js, err := assets.ReadFile("static/js/app.bundle.js")
	if err != nil {
		t.Fatalf("read app.bundle.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, `const associatedForm = (control) =>`) ||
		strings.Count(body, `const form = associatedForm(button)`) < 2 {
		t.Fatalf("app.bundle.js must resolve settings test buttons through their associated form")
	}
}
