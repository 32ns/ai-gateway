package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
	personalpay "personalpay/sdk-go"
)

const (
	localeEN = "en"
	localeZH = "zh-CN"
)

func resolveLocale(w http.ResponseWriter, r *http.Request) string {
	if lang := normalizeLocale(r.URL.Query().Get("lang")); lang != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "lang",
			Value:    lang,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   requestIsHTTPS(r),
		})
		return lang
	}
	if cookie, err := r.Cookie("lang"); err == nil {
		if lang := normalizeLocale(cookie.Value); lang != "" {
			return lang
		}
	}
	return detectLocaleFromHeaders(r.Header.Get("Accept-Language"))
}

func detectLocaleFromHeaders(acceptLanguage string) string {
	for _, part := range strings.Split(strings.ToLower(acceptLanguage), ",") {
		token := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.HasPrefix(token, "zh") {
			return localeZH
		}
	}
	return localeEN
}

func normalizeLocale(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "zh", "zh-cn", "zh-hans":
		return localeZH
	case "en", "en-us":
		return localeEN
	default:
		return ""
	}
}

func translate(locale, key string) string {
	if dict, ok := translations[locale]; ok {
		if value, ok := dict[key]; ok {
			return value
		}
	}
	if value, ok := translations[localeEN][key]; ok {
		return value
	}
	return key
}

func translatef(locale, key string, args ...any) string {
	return fmt.Sprintf(translate(locale, key), args...)
}

func providerListText(locale string, providers []core.ProviderKind) string {
	if len(providers) == 0 {
		return translate(locale, "none")
	}
	return joinProviders(providers)
}

func accountRoleText(locale string, account core.Account) string {
	if containsTag(account, "backup") {
		return translate(locale, "role_backup")
	}
	if containsTag(account, "primary") {
		return translate(locale, "role_primary")
	}
	return translate(locale, "role_manual")
}

func accountStatusText(locale string, status core.AccountStatus) string {
	switch status {
	case core.AccountStatusActive:
		if locale == localeZH {
			return "活跃"
		}
		return "active"
	case core.AccountStatusCooling:
		if locale == localeZH {
			return "冷却中"
		}
		return "cooldown"
	case core.AccountStatusExpired:
		if locale == localeZH {
			return "已过期"
		}
		return "expired"
	case core.AccountStatusBlocked:
		if locale == localeZH {
			return "已阻断"
		}
		return "blocked"
	case core.AccountStatusProviderBanned:
		if locale == localeZH {
			return "已封禁"
		}
		return "provider banned"
	case core.AccountStatusRefreshing:
		if locale == localeZH {
			return "刷新中"
		}
		return "refreshing"
	default:
		return string(status)
	}
}

func accountRuntimeStatusText(locale, status string) string {
	switch strings.TrimSpace(status) {
	case core.AccountRuntimeStatusTimeLimit:
		if locale == localeZH {
			return "5小时限额"
		}
		return "5h limit"
	case core.AccountRuntimeStatusWeekLimit:
		if locale == localeZH {
			return "周限额"
		}
		return "weekly limit"
	default:
		return accountStatusText(locale, core.AccountStatus(status))
	}
}

func healthStatusText(locale, status string) string {
	switch status {
	case "ok":
		return translate(locale, "health_status_healthy")
	case "degraded":
		return translate(locale, "health_status_degraded")
	case "setup":
		return translate(locale, "health_status_setup")
	case "error":
		return translate(locale, "health_status_error")
	default:
		return status
	}
}

func monitorStatusText(locale string, status core.MonitorStatus) string {
	switch status {
	case core.MonitorStatusOK:
		return translate(locale, "monitor_status_ok")
	case core.MonitorStatusDegraded:
		return translate(locale, "monitor_status_degraded")
	case core.MonitorStatusFailed:
		return translate(locale, "monitor_status_failed")
	default:
		return translate(locale, "monitor_status_unknown")
	}
}

func monitorStatusClass(status core.MonitorStatus) string {
	switch status {
	case core.MonitorStatusOK:
		return "tone-good"
	case core.MonitorStatusDegraded:
		return "tone-warn"
	case core.MonitorStatusFailed:
		return "tone-bad"
	default:
		return "tone-muted"
	}
}

func monitorNoticeText(locale, notice string) string {
	switch strings.TrimSpace(notice) {
	case "status_target_saved":
		return translate(locale, "status_target_saved")
	case "status_target_deleted":
		return translate(locale, "status_target_deleted")
	case "status_probe_done":
		return translate(locale, "status_probe_done")
	default:
		return ""
	}
}

func healthReasonText(locale, reason string) string {
	switch reason {
	case "no accounts configured":
		return translate(locale, "health_reason_no_accounts_configured")
	case "no available accounts":
		return translate(locale, "health_reason_no_available_accounts")
	case "one or more providers have no available accounts":
		return translate(locale, "health_reason_provider_degraded")
	case "one or more accounts expire soon":
		return translate(locale, "health_reason_expiring_soon")
	case "accounts available":
		return translate(locale, "health_reason_accounts_available")
	default:
		return reason
	}
}

func personalPayAccountStatusText(locale string, status personalpay.AccountStatus) string {
	switch status {
	case personalpay.AccountIdle:
		return translate(locale, "personalpay_account_idle")
	case personalpay.AccountOccupied:
		return translate(locale, "personalpay_account_occupied")
	case personalpay.AccountOffline:
		return translate(locale, "personalpay_account_offline")
	default:
		return string(status)
	}
}

func personalPayChannelText(locale string, channel personalpay.PaymentChannel) string {
	switch channel {
	case personalpay.ChannelWeChat:
		return translate(locale, "personalpay_channel_wechat")
	case personalpay.ChannelAlipay:
		return translate(locale, "personalpay_channel_alipay")
	default:
		return string(channel)
	}
}

func boolStateText(locale string, enabled bool) string {
	if enabled {
		return translate(locale, "enabled")
	}
	return translate(locale, "disabled")
}

func userRoleText(locale string, role core.UserRole) string {
	switch role {
	case core.UserRoleAdmin:
		return translate(locale, "user_role_admin")
	default:
		return translate(locale, "user_role_user")
	}
}

func clientOwnerText(locale string, client core.APIClient, users []core.User) string {
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		return translate(locale, "unassigned")
	}
	for _, user := range users {
		if user.ID == ownerID {
			return user.Username
		}
	}
	return ownerID
}

func auditStatusText(locale, status string) string {
	switch status {
	case "ok":
		return translate(locale, "success")
	case "error":
		return translate(locale, "error")
	default:
		return status
	}
}

func auditActionText(locale, action string) string {
	switch action {
	case "account.connect":
		if locale == localeZH {
			return "创建账号"
		}
		return "create account"
	case "account.update":
		if locale == localeZH {
			return "更新账号"
		}
		return "update account"
	case "account.toggle":
		if locale == localeZH {
			return "切换账号状态"
		}
		return "toggle account"
	case "account.delete":
		if locale == localeZH {
			return "删除账号"
		}
		return "delete account"
	case "account.recover":
		if locale == localeZH {
			return "恢复账号"
		}
		return "recover account"
	case "account.refresh_quota":
		if locale == localeZH {
			return "刷新账号配额"
		}
		return "refresh account quota"
	case "account.oauth_token_update":
		if locale == localeZH {
			return "更新账号 Token"
		}
		return "update account token"
	case "account.test", "account.detect":
		if locale == localeZH {
			return "检测账号"
		}
		return "detect account"
	case "account.import_codex":
		if locale == localeZH {
			return "从 Codex 导入账号"
		}
		return "import account from codex"
	case "account.import_codex_upload":
		if locale == localeZH {
			return "上传 Codex 账号"
		}
		return "upload codex accounts"
	case "account.batch.disable":
		if locale == localeZH {
			return "批量禁用账号"
		}
		return "batch disable accounts"
	case "account.batch.enable":
		if locale == localeZH {
			return "批量启用账号"
		}
		return "batch enable accounts"
	case "account.batch.delete":
		if locale == localeZH {
			return "批量删除账号"
		}
		return "batch delete accounts"
	case "account.batch.refresh_quota":
		if locale == localeZH {
			return "批量刷新账号配额"
		}
		return "batch refresh account quota"
	case "account.batch.refresh_quota.start":
		if locale == localeZH {
			return "开始批量刷新账号配额"
		}
		return "start batch refresh account quota"
	case "account.batch.refresh_quota.cancel":
		if locale == localeZH {
			return "取消批量刷新账号配额"
		}
		return "cancel batch refresh account quota"
	case "account.batch.test":
		if locale == localeZH {
			return "批量检测账号"
		}
		return "batch detect accounts"
	case "account.batch.test.start":
		if locale == localeZH {
			return "开始批量检测账号"
		}
		return "start batch detect accounts"
	case "account.batch.test.cancel":
		if locale == localeZH {
			return "取消批量检测账号"
		}
		return "cancel batch detect accounts"
	case "account.batch.move_group":
		if locale == localeZH {
			return "批量移动账号分组"
		}
		return "batch move account group"
	case "client.create":
		if locale == localeZH {
			return "创建 API 密钥"
		}
		return "create client"
	case "client.update":
		if locale == localeZH {
			return "更新 API 密钥"
		}
		return "update client"
	case "client.toggle":
		if locale == localeZH {
			return "切换 API 密钥状态"
		}
		return "toggle client"
	case "client.delete":
		if locale == localeZH {
			return "删除 API 密钥"
		}
		return "delete client"
	case "user.create":
		if locale == localeZH {
			return "创建用户"
		}
		return "create user"
	case "user.update":
		if locale == localeZH {
			return "更新用户"
		}
		return "update user"
	case "user.delete":
		if locale == localeZH {
			return "删除用户"
		}
		return "delete user"
	case "user.balance":
		if locale == localeZH {
			return "调整余额"
		}
		return "adjust balance"
	case "image_review.update":
		if locale == localeZH {
			return "\u66f4\u65b0\u751f\u56fe\u5ba1\u67e5\u72b6\u6001"
		}
		return "update image review status"
	case "support.delete":
		if locale == localeZH {
			return "删除客服会话"
		}
		return "delete support conversation"
	case "model.create":
		if locale == localeZH {
			return "创建模型"
		}
		return "create model"
	case "model.sync":
		if locale == localeZH {
			return "同步模型"
		}
		return "sync models"
	case "model.toggle":
		if locale == localeZH {
			return "切换模型状态"
		}
		return "toggle model"
	case "model.delete":
		if locale == localeZH {
			return "删除模型"
		}
		return "delete model"
	case "model.price":
		if locale == localeZH {
			return "更新模型价格"
		}
		return "update model pricing"
	case "model.groups":
		if locale == localeZH {
			return "更新模型可见分组"
		}
		return "update model groups"
	case "monitor.create":
		if locale == localeZH {
			return "\u521b\u5efa\u72b6\u6001\u76d1\u63a7"
		}
		return "create status monitor"
	case "monitor.update":
		if locale == localeZH {
			return "\u66f4\u65b0\u72b6\u6001\u76d1\u63a7"
		}
		return "update status monitor"
	case "monitor.delete":
		if locale == localeZH {
			return "\u5220\u9664\u72b6\u6001\u76d1\u63a7"
		}
		return "delete status monitor"
	case "monitor.run":
		if locale == localeZH {
			return "\u8fd0\u884c\u72b6\u6001\u63a2\u6d4b"
		}
		return "run status probe"
	case "account_group.create":
		if locale == localeZH {
			return "创建账号分组"
		}
		return "create account group"
	case "account_group.delete":
		if locale == localeZH {
			return "删除账号分组"
		}
		return "delete account group"
	case "account_group.update_proxy":
		if locale == localeZH {
			return "更新分组代理"
		}
		return "update group proxy"
	case "account_group.update_name":
		if locale == localeZH {
			return "\u66f4\u65b0\u5206\u7ec4\u540d\u79f0"
		}
		return "update group name"
	case "account_group.update_profile":
		if locale == localeZH {
			return "\u66f4\u65b0\u5206\u7ec4\u57fa\u7840\u4fe1\u606f"
		}
		return "update group profile"
	case "account_group.update_type":
		if locale == localeZH {
			return "更新分组类型"
		}
		return "update group type"
	case "account_group.update_remark":
		if locale == localeZH {
			return "更新分组备注"
		}
		return "update group remark"
	case "account_group.update_billing":
		if locale == localeZH {
			return "更新分组计费"
		}
		return "update group billing"
	case "account_group.update_visibility":
		if locale == localeZH {
			return "更新分组可见性"
		}
		return "update group visibility"
	case "account.runtime.reconcile":
		if locale == localeZH {
			return "账号运行态整理"
		}
		return "reconcile account runtime"
	case "account.import":
		if locale == localeZH {
			return "导入账号"
		}
		return "import accounts"
	case "account.export":
		if locale == localeZH {
			return "导出账号"
		}
		return "export accounts"
	case "system_settings.update":
		if locale == localeZH {
			return "更新系统设置"
		}
		return "update system settings"
	case "payment.create":
		if locale == localeZH {
			return "创建支付订单"
		}
		return "create payment order"
	case "payment.order.confirm_paid":
		if locale == localeZH {
			return "确认支付成功"
		}
		return "confirm payment paid"
	case "payment.refund":
		if locale == localeZH {
			return "退款"
		}
		return "refund payment"
	case "payment.refund.create":
		if locale == localeZH {
			return "创建退款任务"
		}
		return "create refund task"
	case "payment.refund.complete":
		if locale == localeZH {
			return "确认手动退款"
		}
		return "confirm manual refund"
	case "payment.refund.fail":
		if locale == localeZH {
			return "标记退款失败"
		}
		return "mark refund failed"
	case "plan.purchase":
		if locale == localeZH {
			return "购买套餐"
		}
		return "purchase plan"
	case "plan.grant":
		if locale == localeZH {
			return "赠送套餐"
		}
		return "grant plan"
	case "plan.entitlement.cancel":
		if locale == localeZH {
			return "失效用户套餐"
		}
		return "cancel user plan"
	case "plan.upsert":
		if locale == localeZH {
			return "保存套餐"
		}
		return "save plan"
	case "plan.delete":
		if locale == localeZH {
			return "删除套餐"
		}
		return "delete plan"
	case "plan_group.create":
		if locale == localeZH {
			return "添加套餐分组"
		}
		return "create plan group"
	case "plan_group.update":
		if locale == localeZH {
			return "更新套餐分组"
		}
		return "update plan group"
	case "plan_group.delete":
		if locale == localeZH {
			return "删除套餐分组"
		}
		return "delete plan group"
	case "billing.release_reserved":
		if locale == localeZH {
			return "取消待结算"
		}
		return "cancel pending billing"
	case "personalpay.device.delete":
		if locale == localeZH {
			return "删除 PersonalPay 设备"
		}
		return "delete PersonalPay device"
	case "personalpay.account.release":
		if locale == localeZH {
			return "释放 PersonalPay 订单"
		}
		return "release PersonalPay order"
	case "site_message.create":
		if locale == localeZH {
			return "创建站内消息"
		}
		return "create site message"
	case "site_message.update":
		if locale == localeZH {
			return "更新站内消息"
		}
		return "update site message"
	case "site_message.delete":
		if locale == localeZH {
			return "删除站内消息"
		}
		return "delete site message"
	case "document.create":
		if locale == localeZH {
			return "创建文档"
		}
		return "create document"
	case "document.update":
		if locale == localeZH {
			return "更新文档"
		}
		return "update document"
	case "document.delete":
		if locale == localeZH {
			return "删除文档"
		}
		return "delete document"
	case "mcp_token.create":
		if locale == localeZH {
			return "创建 MCP Token"
		}
		return "create MCP token"
	case "mcp_token.update":
		if locale == localeZH {
			return "更新 MCP Token"
		}
		return "update MCP token"
	case "mcp_token.delete":
		if locale == localeZH {
			return "删除 MCP Token"
		}
		return "delete MCP token"
	case "mcp_token.revoke":
		if locale == localeZH {
			return "吊销 MCP Token"
		}
		return "revoke MCP token"
	case "docs.list":
		if locale == localeZH {
			return "MCP 列出文档"
		}
		return "MCP list docs"
	case "docs.search":
		if locale == localeZH {
			return "MCP 搜索文档"
		}
		return "MCP search docs"
	case "docs.read":
		if locale == localeZH {
			return "MCP 读取文档"
		}
		return "MCP read doc"
	case "docs.create_draft":
		if locale == localeZH {
			return "MCP 创建文档草稿"
		}
		return "MCP create doc draft"
	case "docs.update":
		if locale == localeZH {
			return "MCP 更新文档"
		}
		return "MCP update doc"
	case "docs.publish":
		if locale == localeZH {
			return "MCP 发布文档"
		}
		return "MCP publish doc"
	case "docs.archive":
		if locale == localeZH {
			return "MCP 归档文档"
		}
		return "MCP archive doc"
	case "docs.set_pinned":
		if locale == localeZH {
			return "MCP 设置文档置顶"
		}
		return "MCP set doc pinned"
	case "mcp.initialize":
		if locale == localeZH {
			return "MCP 初始化"
		}
		return "MCP initialize"
	case "mcp.tools.list":
		if locale == localeZH {
			return "MCP 列出工具"
		}
		return "MCP list tools"
	case "mcp.resources.list":
		if locale == localeZH {
			return "MCP 列出资源"
		}
		return "MCP list resources"
	case "mcp.resources.read":
		if locale == localeZH {
			return "MCP 读取资源"
		}
		return "MCP read resource"
	default:
		return action
	}
}

func auditKindText(locale string, kind core.AuditKind) string {
	switch kind {
	case core.AuditKindAdmin:
		return translate(locale, "audit_kind_admin")
	case core.AuditKindGateway:
		return translate(locale, "audit_kind_gateway")
	default:
		if kind == "" {
			return translate(locale, "audit_kind_gateway")
		}
		return string(kind)
	}
}

func auditSubjectText(locale string, event core.AuditEvent) string {
	if event.EffectiveKind() == core.AuditKindAdmin {
		if event.Actor != "" {
			return event.Actor
		}
		return "-"
	}
	if event.ClientName != "" && event.ClientID != "" && event.ClientName != event.ClientID {
		return event.ClientName + " / " + event.ClientID
	}
	if event.ClientName != "" {
		return event.ClientName
	}
	if event.ClientID != "" {
		return event.ClientID
	}
	return "-"
}

func auditOperationText(locale string, event core.AuditEvent) string {
	if event.EffectiveKind() == core.AuditKindAdmin {
		if event.Action == "user.balance" {
			return auditUserBalanceOperationText(locale, event)
		}
		return auditActionText(locale, event.Action)
	}
	if event.Action != "" {
		return auditActionText(locale, event.Action)
	}
	if event.Model != "" {
		return event.Model
	}
	if locale == localeZH {
		return "协议请求"
	}
	return "protocol request"
}

func auditUserBalanceOperationText(locale string, event core.AuditEvent) string {
	values := auditMessageValues(event.Message)
	deltaText := strings.TrimSpace(values["delta"])
	delta, err := core.ParseNanoUSDDecimal(deltaText)
	if err != nil || delta == 0 {
		if locale == localeZH {
			return "调整余额"
		}
		return "adjust balance"
	}
	action := translate(locale, "recharge")
	if delta < 0 {
		action = translate(locale, "deduct")
	}
	parts := []string{action}
	if target := strings.TrimSpace(event.ResourceName); target != "" {
		parts = append(parts, target)
	}
	parts = append(parts, signedAuditUSDDisplay(delta))
	previous := auditUSDFromMessage(values["previous"])
	current := auditUSDFromMessage(values["current"])
	if previous != "" && current != "" {
		parts = append(parts, fmt.Sprintf("%s -> %s", previous, current))
	}
	if reason := strings.TrimSpace(values["reason"]); reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(parts, " · ")
}

func auditMessageValues(message string) map[string]string {
	out := map[string]string{}
	message = strings.TrimSpace(message)
	keys := []string{"previous", "current", "delta", "reason"}
	for i, key := range keys {
		marker := key + "="
		start := strings.Index(message, marker)
		if start < 0 {
			continue
		}
		valueStart := start + len(marker)
		valueEnd := len(message)
		for _, nextKey := range keys[i+1:] {
			nextMarker := " " + nextKey + "="
			if next := strings.Index(message[valueStart:], nextMarker); next >= 0 && valueStart+next < valueEnd {
				valueEnd = valueStart + next
			}
		}
		out[key] = strings.TrimSpace(message[valueStart:valueEnd])
	}
	return out
}

func auditUSDFromMessage(value string) string {
	amount, err := core.ParseNanoUSDDecimal(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return auditUSDDisplay(amount)
}

func auditUSDDisplay(nanoUSD int64) string {
	sign := ""
	if nanoUSD < 0 {
		sign = "-"
		nanoUSD = -nanoUSD
	}
	cents := nanoUSD / (core.NanoUSDPerUSD / 100)
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func signedAuditUSDDisplay(nanoUSD int64) string {
	if nanoUSD > 0 {
		return "+" + auditUSDDisplay(nanoUSD)
	}
	return auditUSDDisplay(nanoUSD)
}

func auditResourceText(event core.AuditEvent) string {
	if event.EffectiveKind() == core.AuditKindGateway {
		if event.ResourceName != "" && event.ResourceID != "" && event.ResourceName != event.ResourceID {
			return event.ResourceName + " / " + event.ResourceID
		}
		if event.ResourceName != "" {
			return event.ResourceName
		}
		if event.ResourceID != "" {
			return event.ResourceID
		}
		parts := make([]string, 0, 2)
		if event.Provider != "" {
			parts = append(parts, string(event.Provider))
		}
		if event.AccountID != "" {
			parts = append(parts, event.AccountID)
		}
		if len(parts) == 0 {
			return "-"
		}
		return strings.Join(parts, " / ")
	}
	switch {
	case event.ResourceName != "" && event.ResourceID != "" && event.ResourceName != event.ResourceID:
		return event.ResourceName + " / " + event.ResourceID
	case event.ResourceName != "":
		return event.ResourceName
	case event.ResourceID != "":
		return event.ResourceID
	default:
		return "-"
	}
}

func auditMessagePreview(event core.AuditEvent) string {
	message := strings.TrimSpace(event.Message)
	if message == "" {
		return "-"
	}
	return truncateRunes(message, 140)
}

func auditAttemptPreview(event core.AuditEvent) string {
	if len(event.Attempts) == 0 {
		return ""
	}

	shouldShow := false
	for _, attempt := range event.Attempts {
		if attempt.Status != "ok" {
			shouldShow = true
			break
		}
	}
	if !shouldShow {
		return ""
	}

	parts := make([]string, 0, len(event.Attempts))
	for i, attempt := range event.Attempts {
		if i == 4 {
			parts = append(parts, "...")
			break
		}
		label := string(attempt.Provider)
		if attempt.AccountLabel != "" {
			label += "/" + attempt.AccountLabel
		} else if attempt.AccountID != "" {
			label += "/" + attempt.AccountID
		}
		if label == "" {
			label = "-"
		}
		parts = append(parts, label+":"+attempt.Status)
	}
	return strings.Join(parts, " -> ")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
