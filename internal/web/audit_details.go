package web

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
)

type auditDetailLine struct {
	Label string
	Value string
}

type auditMessageField struct {
	Key   string
	Value string
}

var auditMessageFieldPattern = regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z0-9_]*)=`)

func auditDetailLines(locale string, event core.AuditEvent) []auditDetailLine {
	lines := make([]auditDetailLine, 0, 6)
	lines = append(lines, auditResourceDetailLines(locale, event)...)
	lines = append(lines, auditMessageDetailLines(locale, event)...)
	if attempts := auditAttemptPreview(event); attempts != "" {
		lines = append(lines, auditDetailLine{Label: translate(locale, "attempts"), Value: attempts})
	}
	return lines
}

func auditResourceDetailLines(locale string, event core.AuditEvent) []auditDetailLine {
	if event.EffectiveKind() == core.AuditKindGateway {
		return auditGatewayResourceDetailLines(locale, event)
	}
	switch strings.TrimSpace(event.ResourceType) {
	case "billing_request":
		lines := make([]auditDetailLine, 0, 2)
		if value := strings.TrimSpace(event.ResourceName); value != "" {
			lines = append(lines, auditDetailLine{Label: translate(locale, "client"), Value: value})
		}
		if value := strings.TrimSpace(event.ResourceID); value != "" {
			lines = append(lines, auditDetailLine{Label: translate(locale, "request_id"), Value: value})
		}
		return lines
	case "account":
		return auditNamedResourceDetailLines(locale, event, auditStaticLabel(locale, "账号", "Account"), auditStaticLabel(locale, "账号 ID", "Account ID"))
	case "account_group":
		return auditNamedResourceDetailLines(locale, event, auditStaticLabel(locale, "账号分组", "Account group"), auditStaticLabel(locale, "分组 ID", "Group ID"))
	case "client":
		return auditNamedResourceDetailLines(locale, event, translate(locale, "client"), auditStaticLabel(locale, "密钥 ID", "Client ID"))
	case "model":
		return auditNamedResourceDetailLines(locale, event, translate(locale, "model"), auditStaticLabel(locale, "模型 ID", "Model ID"))
	case "payment_order":
		return auditNamedResourceDetailLines(locale, event, translate(locale, "payment_order"), auditStaticLabel(locale, "订单 ID", "Order ID"))
	case "site_message":
		return auditNamedResourceDetailLines(locale, event, auditStaticLabel(locale, "站内消息", "Message"), auditStaticLabel(locale, "消息 ID", "Message ID"))
	case "document":
		return auditNamedResourceDetailLines(locale, event, auditStaticLabel(locale, "文档", "Document"), auditStaticLabel(locale, "文档 ID", "Document ID"))
	case "mcp_token":
		return auditNamedResourceDetailLines(locale, event, auditStaticLabel(locale, "MCP Token", "MCP Token"), auditStaticLabel(locale, "Token ID", "Token ID"))
	case "system_settings":
		if strings.TrimSpace(event.ResourceID) == "" {
			return nil
		}
		return []auditDetailLine{{Label: auditStaticLabel(locale, "设置项", "Settings"), Value: auditSystemSettingsResourceText(locale, event.ResourceID)}}
	case "user":
		return auditUserResourceDetailLines(locale, event)
	default:
		value := auditResourceText(event)
		if value == "-" {
			return nil
		}
		return []auditDetailLine{{Label: translate(locale, "resource"), Value: value}}
	}
}

func auditUserResourceDetailLines(locale string, event core.AuditEvent) []auditDetailLine {
	if name := strings.TrimSpace(event.ResourceName); name != "" {
		return []auditDetailLine{{Label: auditStaticLabel(locale, "用户", "User"), Value: name}}
	}
	if id := strings.TrimSpace(event.ResourceID); id != "" {
		return []auditDetailLine{{Label: auditStaticLabel(locale, "用户", "User"), Value: id}}
	}
	return nil
}

func auditGatewayResourceDetailLines(locale string, event core.AuditEvent) []auditDetailLine {
	lines := make([]auditDetailLine, 0, 4)
	if event.ClientName != "" && event.ClientID != "" && event.ClientName != event.ClientID {
		lines = append(lines, auditDetailLine{Label: translate(locale, "client"), Value: event.ClientName})
		lines = append(lines, auditDetailLine{Label: auditStaticLabel(locale, "密钥 ID", "Client ID"), Value: event.ClientID})
	} else if event.ClientName != "" {
		lines = append(lines, auditDetailLine{Label: translate(locale, "client"), Value: event.ClientName})
	} else if event.ClientID != "" {
		lines = append(lines, auditDetailLine{Label: auditStaticLabel(locale, "密钥 ID", "Client ID"), Value: event.ClientID})
	}
	if event.Provider != "" {
		lines = append(lines, auditDetailLine{Label: translate(locale, "provider"), Value: string(event.Provider)})
	}
	if event.AccountID != "" {
		lines = append(lines, auditDetailLine{Label: auditStaticLabel(locale, "账号 ID", "Account ID"), Value: event.AccountID})
	}
	if event.Model != "" {
		lines = append(lines, auditDetailLine{Label: translate(locale, "model"), Value: event.Model})
	}
	return lines
}

func auditNamedResourceDetailLines(locale string, event core.AuditEvent, nameLabel, idLabel string) []auditDetailLine {
	name := strings.TrimSpace(event.ResourceName)
	id := strings.TrimSpace(event.ResourceID)
	switch {
	case name != "" && id != "" && name != id:
		return []auditDetailLine{{Label: nameLabel, Value: name}, {Label: idLabel, Value: id}}
	case name != "":
		return []auditDetailLine{{Label: nameLabel, Value: name}}
	case id != "":
		return []auditDetailLine{{Label: idLabel, Value: id}}
	default:
		return nil
	}
}

func auditMessageDetailLines(locale string, event core.AuditEvent) []auditDetailLine {
	message := strings.TrimSpace(event.Message)
	if message == "" {
		return nil
	}
	fields := auditMessageFields(message)
	if len(fields) == 0 {
		label := translate(locale, "message_body")
		if event.Status == "error" {
			label = translate(locale, "error")
		}
		return []auditDetailLine{{Label: label, Value: auditHumanMessageText(locale, message)}}
	}
	if lines := auditActionSummaryLines(locale, event, fields); len(lines) > 0 {
		return lines
	}
	if lines := auditChangeDetailLines(locale, fields); len(lines) > 0 {
		return lines
	}
	lines := make([]auditDetailLine, 0, len(fields))
	for _, field := range fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		lines = append(lines, auditDetailLine{
			Label: auditMessageFieldLabel(locale, field.Key),
			Value: auditMessageFieldValue(locale, field.Key, field.Value),
		})
	}
	if len(lines) == 0 {
		return []auditDetailLine{{Label: translate(locale, "message_body"), Value: truncateRunes(message, 140)}}
	}
	return lines
}

func auditActionSummaryLines(locale string, event core.AuditEvent, fields []auditMessageField) []auditDetailLine {
	values := auditFieldMap(fields)
	switch {
	case strings.HasPrefix(strings.TrimSpace(event.Action), "account.batch."):
		return auditAccountBatchSummaryLines(locale, event, values)
	case event.Action == "system_settings.update":
		parts := make([]string, 0, 2)
		if auditLimit := strings.TrimSpace(values["audit_limit"]); auditLimit != "" {
			parts = append(parts, auditStaticLabel(locale, "审计保留上限：", "Audit retention: ")+auditLimit)
		}
		if usageDays := strings.TrimSpace(values["usage_log_max_age_days"]); usageDays != "" {
			parts = append(parts, auditStaticLabel(locale, "使用日志保留天数：", "Usage log days: ")+usageDays)
		}
		if ledgerDays := strings.TrimSpace(values["billing_ledger_retention_days"]); ledgerDays != "" {
			parts = append(parts, auditStaticLabel(locale, "财务流水保留天数：", "Billing ledger days: ")+ledgerDays)
		}
		if limit := strings.TrimSpace(values["user_concurrent_request_limit"]); limit != "" {
			parts = append(parts, auditStaticLabel(locale, "用户并发请求上限：", "Per-user concurrent request limit: ")+limit)
		}
		if limit := strings.TrimSpace(values["plan_concurrent_request_limit"]); limit != "" {
			parts = append(parts, auditStaticLabel(locale, "套餐并发请求上限：", "Plan concurrent request limit: ")+limit)
		}
		if limit := strings.TrimSpace(values["user_ip_concurrent_request_limit"]); limit != "" {
			parts = append(parts, auditStaticLabel(locale, "单用户同 IP 并发请求上限：", "Per-user same-IP concurrent request limit: ")+limit)
		}
		if len(parts) > 0 {
			return []auditDetailLine{{Label: auditChangeLabel(locale), Value: strings.Join(parts, auditStaticLabel(locale, "；", "; "))}}
		}
	case event.Action == "billing.release_reserved":
		amountText := auditAmountFieldText(values["amount"])
		if amountText == "" {
			return nil
		}
		lines := []auditDetailLine{
			{Label: auditResultLabel(locale), Value: auditStaticLabel(locale, "已取消待结算请求", "Pending billing request cancelled")},
			{Label: translate(locale, "amount"), Value: amountText},
		}
		if userID := strings.TrimSpace(values["user"]); userID != "" {
			lines = append(lines, auditDetailLine{Label: auditStaticLabel(locale, "用户", "User"), Value: userID})
		}
		if model := strings.TrimSpace(values["model"]); model != "" {
			lines = append(lines, auditDetailLine{Label: translate(locale, "model"), Value: model})
		}
		return lines
	case event.Action == "account.toggle":
		if from, hasFrom := values["control_disabled_from"]; hasFrom {
			if to, hasTo := values["control_disabled_to"]; hasTo {
				result := auditStaticLabel(locale, "手动禁用：", "Manually disabled: ") + auditChangeValueText(locale, "control_disabled", from) + " -> " + auditChangeValueText(locale, "control_disabled", to)
				if status := strings.TrimSpace(values["status"]); status != "" {
					result += auditStaticLabel(locale, "，当前状态：", ", status: ") + auditStatusValueText(locale, status)
				}
				return []auditDetailLine{{Label: auditChangeLabel(locale), Value: result}}
			}
		}
		if disabled := strings.TrimSpace(values["control_disabled"]); disabled != "" {
			result := auditStaticLabel(locale, "账号已启用", "Account enabled")
			if strings.EqualFold(disabled, "true") {
				result = auditStaticLabel(locale, "账号已禁用", "Account disabled")
			}
			if status := strings.TrimSpace(values["status"]); status != "" {
				result += auditStaticLabel(locale, "，当前状态：", ", status: ") + auditStatusValueText(locale, status)
			}
			return []auditDetailLine{{Label: auditChangeLabel(locale), Value: result}}
		}
	case event.Action == "user.balance":
		return auditUserBalanceDetailLines(locale, values)
	}
	return nil
}

func auditChangeDetailLines(locale string, fields []auditMessageField) []auditDetailLine {
	values := auditRawFieldMap(fields)
	if strings.EqualFold(strings.TrimSpace(values["no_changes"]), "true") {
		return []auditDetailLine{{Label: auditChangeLabel(locale), Value: auditStaticLabel(locale, "无实际变更", "No effective changes")}}
	}
	order := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		base, ok := auditChangeFieldBase(field.Key)
		if !ok {
			continue
		}
		if _, exists := seen[base]; exists {
			continue
		}
		seen[base] = struct{}{}
		order = append(order, base)
	}
	if len(order) == 0 {
		return nil
	}
	lines := make([]auditDetailLine, 0, len(order))
	for _, base := range order {
		from, hasFrom := values[base+"_from"]
		to, hasTo := values[base+"_to"]
		if !hasFrom || !hasTo {
			continue
		}
		lines = append(lines, auditDetailLine{
			Label: auditMessageFieldLabel(locale, base),
			Value: auditChangeValueText(locale, base, from) + " -> " + auditChangeValueText(locale, base, to),
		})
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

func auditChangeFieldBase(key string) (string, bool) {
	key = strings.TrimSpace(key)
	switch {
	case strings.HasSuffix(key, "_from"):
		return strings.TrimSuffix(key, "_from"), true
	case strings.HasSuffix(key, "_to"):
		return strings.TrimSuffix(key, "_to"), true
	default:
		return "", false
	}
}

func auditChangeValueText(locale, key, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return auditStaticLabel(locale, "空", "empty")
	}
	return auditMessageFieldValue(locale, key, value)
}

func auditAccountBatchSummaryLines(locale string, event core.AuditEvent, values map[string]string) []auditDetailLine {
	action := strings.TrimSpace(values["action"])
	if action == "" {
		action = strings.TrimPrefix(strings.TrimSpace(event.Action), "account.batch.")
	}
	actionText := auditAccountBatchActionValue(locale, action)
	total := auditCountText(locale, values["total"])
	succeeded := auditCountText(locale, values["succeeded"])
	failed := auditCountText(locale, values["failed"])
	skipped := auditCountText(locale, values["skipped"])
	value := fmt.Sprintf(
		auditStaticLabel(locale, "%s：共 %s，成功 %s，失败 %s，跳过 %s", "%s: total %s, succeeded %s, failed %s, skipped %s"),
		actionText,
		total,
		succeeded,
		failed,
		skipped,
	)
	lines := []auditDetailLine{{Label: auditResultLabel(locale), Value: value}}
	if targetGroup := strings.Trim(strings.TrimSpace(values["target_group"]), `"`); targetGroup != "" {
		lines = append(lines, auditDetailLine{Label: auditStaticLabel(locale, "目标分组", "Target group"), Value: accountGroupLabelText(locale, targetGroup)})
	}
	return lines
}

func auditUserBalanceDetailLines(locale string, values map[string]string) []auditDetailLine {
	delta, hasDelta := auditParseAmountField(values["delta"])
	previous := auditAmountFieldText(values["previous"])
	current := auditAmountFieldText(values["current"])
	amount := auditAmountFieldText(values["amount"])
	if paymentID := strings.TrimSpace(values["payment"]); paymentID != "" {
		lines := []auditDetailLine{{Label: translate(locale, "payment_order"), Value: paymentID}}
		if provider := strings.TrimSpace(values["provider"]); provider != "" {
			lines = append(lines, auditDetailLine{Label: translate(locale, "provider"), Value: auditProviderText(locale, provider)})
		}
		if amount != "" {
			lines = append(lines, auditDetailLine{Label: translate(locale, "amount"), Value: amount})
		}
		return lines
	}
	if !hasDelta && previous == "" && current == "" {
		return nil
	}
	action := auditStaticLabel(locale, "余额调整", "Balance adjusted")
	if hasDelta {
		if delta > 0 {
			action = translate(locale, "recharge")
		} else if delta < 0 {
			action = translate(locale, "deduct")
		}
		action += " " + signedAuditExactUSDDisplay(delta)
	}
	if previous != "" && current != "" {
		action += auditStaticLabel(locale, "，余额 ", ", balance ") + previous + " -> " + current
	}
	lines := []auditDetailLine{{Label: auditChangeLabel(locale), Value: action}}
	if reason := strings.TrimSpace(values["reason"]); reason != "" {
		lines = append(lines, auditDetailLine{Label: translate(locale, "reason"), Value: auditHumanMessageText(locale, reason)})
	}
	return lines
}

func auditFieldMap(fields []auditMessageField) map[string]string {
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		out[strings.TrimSpace(field.Key)] = auditDecodeFieldValue(field.Value)
	}
	return out
}

func auditRawFieldMap(fields []auditMessageField) map[string]string {
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		out[strings.TrimSpace(field.Key)] = strings.TrimSpace(field.Value)
	}
	return out
}

func auditMessageFields(message string) []auditMessageField {
	message = strings.TrimSpace(message)
	matches := auditMessageFieldPattern.FindAllStringSubmatchIndex(message, -1)
	if len(matches) == 0 {
		return nil
	}
	fields := make([]auditMessageField, 0, len(matches))
	for i, match := range matches {
		keyStart, keyEnd := match[2], match[3]
		valueStart := match[1]
		valueEnd := len(message)
		if i+1 < len(matches) {
			valueEnd = matches[i+1][0]
		}
		key := strings.TrimSpace(message[keyStart:keyEnd])
		value := strings.TrimSpace(message[valueStart:valueEnd])
		if key != "" {
			fields = append(fields, auditMessageField{Key: key, Value: value})
		}
	}
	return fields
}

func auditMessageFieldLabel(locale, key string) string {
	key = strings.TrimSpace(key)
	if label := auditExtendedMessageFieldLabel(locale, key); label != "" {
		return label
	}
	switch key {
	case "action":
		return translate(locale, "action")
	case "amount":
		return translate(locale, "amount")
	case "audit_limit":
		return auditStaticLabel(locale, "审计保留上限", "Audit retention")
	case "usage_log_max_age_days":
		return auditStaticLabel(locale, "使用日志保留天数", "Usage log days")
	case "billing_ledger_retention_days":
		return auditStaticLabel(locale, "财务流水保留天数", "Billing ledger days")
	case "cached_input":
		return auditStaticLabel(locale, "缓存输入价", "Cached input price")
	case "billing_source":
		return auditStaticLabel(locale, "扣费方式", "Billing source")
	case "control_disabled":
		return auditStaticLabel(locale, "手动禁用", "Manually disabled")
	case "current":
		return auditStaticLabel(locale, "调整后", "After")
	case "delta":
		return auditStaticLabel(locale, "变动", "Change")
	case "enabled":
		return auditStaticLabel(locale, "启用", "Enabled")
	case "fixed":
		return auditStaticLabel(locale, "固定", "Fixed")
	case "groups":
		return auditStaticLabel(locale, "可见分组", "Visible groups")
	case "visible_users":
		return auditStaticLabel(locale, "可见用户", "Visible users")
	case "input":
		return auditStaticLabel(locale, "输入价", "Input price")
	case "kind":
		return translate(locale, "type")
	case "message":
		return translate(locale, "message_body")
	case "model":
		return translate(locale, "model")
	case "mode":
		return auditStaticLabel(locale, "计费模式", "Billing mode")
	case "multiplier":
		return auditStaticLabel(locale, "倍率", "Multiplier")
	case "plan_multiplier":
		return auditStaticLabel(locale, "套餐倍率", "Plan multiplier")
	case "output":
		return auditStaticLabel(locale, "输出价", "Output price")
	case "package":
		return auditStaticLabel(locale, "包", "Package")
	case "payment":
		return translate(locale, "payment_order")
	case "payment_recharge_input_mode":
		return translate(locale, "payment_recharge_input_mode")
	case "payment_min_recharge":
		return translate(locale, "payment_min_recharge_usd")
	case "payment_max_recharge":
		return translate(locale, "payment_max_recharge_usd")
	case "plan":
		return auditStaticLabel(locale, "套餐", "Plan")
	case "plan_billing_enabled":
		return auditStaticLabel(locale, "允许套餐计费", "Plan billing enabled")
	case "priority":
		return auditStaticLabel(locale, "优先级", "Priority")
	case "provider":
		return translate(locale, "provider")
	case "proxy_url":
		return auditStaticLabel(locale, "代理", "Proxy")
	case "reason":
		return translate(locale, "reason")
	case "remark":
		return translate(locale, "note")
	case "request":
		return auditStaticLabel(locale, "请求价", "Request price")
	case "role":
		return translate(locale, "role")
	case "route":
		return auditStaticLabel(locale, "路由", "Route")
	case "scope":
		return auditStaticLabel(locale, "范围", "Scope")
	case "show_in_client_editor":
		return auditStaticLabel(locale, "创建密钥时显示", "Shown in client editor")
	case "source":
		return translate(locale, "source")
	case "status":
		return translate(locale, "status")
	case "total_fails":
		return auditStaticLabel(locale, "累计失败", "Total failures")
	case "user":
		return auditStaticLabel(locale, "用户", "User")
	case "version":
		return auditStaticLabel(locale, "版本", "Version")
	case "weight":
		return auditStaticLabel(locale, "权重", "Weight")
	case "previous":
		return auditStaticLabel(locale, "调整前", "Before")
	default:
		return auditHumanKey(locale, key)
	}
}

func auditExtendedMessageFieldLabel(locale, key string) string {
	switch key {
	case "access_token_changed":
		return auditStaticLabel(locale, "访问令牌已更新", "Access token changed")
	case "account_group":
		return auditStaticLabel(locale, "账号分组", "Account group")
	case "alipay_app_id":
		return "Alipay AppID"
	case "alipay_enabled":
		return auditStaticLabel(locale, "支付宝支付", "Alipay")
	case "alipay_gateway_url":
		return auditStaticLabel(locale, "支付宝网关", "Alipay gateway")
	case "alipay_notify_url":
		return auditStaticLabel(locale, "支付宝通知地址", "Alipay notify URL")
	case "alipay_private_key_changed":
		return auditStaticLabel(locale, "支付宝私钥已更新", "Alipay private key changed")
	case "alipay_public_key_changed":
		return auditStaticLabel(locale, "支付宝公钥已更新", "Alipay public key changed")
	case "alipay_return_url":
		return auditStaticLabel(locale, "支付宝返回地址", "Alipay return URL")
	case "alipay_sign_type":
		return auditStaticLabel(locale, "支付宝签名类型", "Alipay sign type")
	case "allow_public_registration":
		return auditStaticLabel(locale, "公开注册", "Public registration")
	case "base_url":
		return auditStaticLabel(locale, "基础地址", "Base URL")
	case "body":
		return auditStaticLabel(locale, "正文", "Body")
	case "brand_subtitle":
		return auditStaticLabel(locale, "品牌副标题", "Brand subtitle")
	case "brand_title":
		return auditStaticLabel(locale, "品牌标题", "Brand title")
	case "claude_oauth_enabled":
		return "Claude OAuth"
	case "cloudmail_account_id":
		return auditStaticLabel(locale, "CloudMail 账号 ID", "CloudMail account ID")
	case "cloudmail_base_url":
		return auditStaticLabel(locale, "CloudMail 地址", "CloudMail URL")
	case "cloudmail_email":
		return auditStaticLabel(locale, "CloudMail 邮箱", "CloudMail email")
	case "cloudmail_password_changed":
		return auditStaticLabel(locale, "CloudMail 密码已更新", "CloudMail password changed")
	case "config_changed":
		return auditStaticLabel(locale, "配置已更新", "Config changed")
	case "email_code_ip_hourly_limit":
		return auditStaticLabel(locale, "验证码 IP 每小时限制", "Email code IP hourly limit")
	case "email_code_ttl_seconds":
		return auditStaticLabel(locale, "验证码有效期秒数", "Email code TTL seconds")
	case "email_hourly_send_limit":
		return auditStaticLabel(locale, "邮箱每小时发送限制", "Email hourly send limit")
	case "email_max_attempts":
		return auditStaticLabel(locale, "验证码最大尝试次数", "Email max attempts")
	case "email_provider":
		return auditStaticLabel(locale, "邮件服务商", "Email provider")
	case "email_send_cooldown_seconds":
		return auditStaticLabel(locale, "验证码发送冷却秒数", "Email send cooldown seconds")
	case "email_template_html":
		return auditStaticLabel(locale, "验证邮件 HTML 模版", "Verification email HTML template")
	case "email_template_subject":
		return auditStaticLabel(locale, "验证邮件主题模版", "Verification email subject template")
	case "email_template_text":
		return auditStaticLabel(locale, "验证邮件文本模版", "Verification email text template")
	case "expires_at":
		return auditStaticLabel(locale, "过期时间", "Expires at")
	case "from_email":
		return auditStaticLabel(locale, "发件邮箱", "From email")
	case "from_name":
		return auditStaticLabel(locale, "发件名称", "From name")
	case "github_login_client_id":
		return "GitHub Client ID"
	case "github_login_enabled":
		return auditStaticLabel(locale, "GitHub 登录", "GitHub login")
	case "github_login_secret_changed":
		return auditStaticLabel(locale, "GitHub Secret 已更新", "GitHub secret changed")
	case "google_login_client_id":
		return "Google Client ID"
	case "google_login_enabled":
		return auditStaticLabel(locale, "Google 登录", "Google login")
	case "google_login_secret_changed":
		return auditStaticLabel(locale, "Google Secret 已更新", "Google secret changed")
	case "home_availability":
		return auditStaticLabel(locale, "首页可用性文案", "Home availability text")
	case "home_capability":
		return auditStaticLabel(locale, "首页能力文案", "Home capability text")
	case "home_cost_multiplier":
		return auditStaticLabel(locale, "首页成本文案", "Home cost text")
	case "home_heading":
		return auditStaticLabel(locale, "首页标题", "Home heading")
	case "home_latency":
		return auditStaticLabel(locale, "首页延迟文案", "Home latency text")
	case "home_summary":
		return auditStaticLabel(locale, "首页摘要", "Home summary")
	case "invitation_enabled":
		return auditStaticLabel(locale, "邀请功能", "Invitation")
	case "invitee_reward":
		return auditStaticLabel(locale, "被邀请人奖励", "Invitee reward")
	case "inviter_reward":
		return auditStaticLabel(locale, "邀请人奖励", "Inviter reward")
	case "inviter_recharge_reward_percent":
		return auditStaticLabel(locale, "Inviter recharge reward", "Inviter recharge reward")
	case "label":
		return auditStaticLabel(locale, "名称", "Label")
	case "login_auto_create_user":
		return auditStaticLabel(locale, "登录自动创建用户", "Login auto-create user")
	case "name":
		return auditStaticLabel(locale, "名称", "Name")
	case "new_user_reward":
		return auditStaticLabel(locale, "新用户奖励", "New user reward")
	case "new_user_reward_enabled":
		return auditStaticLabel(locale, "新用户奖励", "New user reward")
	case "openai_oauth_enabled":
		return "OpenAI OAuth"
	case "order":
		return auditStaticLabel(locale, "排序", "Order")
	case "password_changed":
		return auditStaticLabel(locale, "密码已更新", "Password changed")
	case "public_base_url":
		return auditStaticLabel(locale, "公开访问地址", "Public base URL")
	case "refresh_token_changed":
		return auditStaticLabel(locale, "刷新令牌已更新", "Refresh token changed")
	case "register_ip_hourly_limit":
		return auditStaticLabel(locale, "注册 IP 每小时限制", "Register IP hourly limit")
	case "registration_username_min_length":
		return auditStaticLabel(locale, "用户名最小长度", "Minimum username length")
	case "registration_email_allowlist":
		return auditStaticLabel(locale, "注册邮箱白名单", "Registration email allowlist")
	case "registration_email_allowlist_enabled":
		return auditStaticLabel(locale, "注册邮箱白名单", "Registration email allowlist")
	case "registration_verification_enabled":
		return auditStaticLabel(locale, "注册邮箱验证", "Registration email verification")
	case "require_invitation_code":
		return auditStaticLabel(locale, "强制邀请码", "Require invitation code")
	case "session_token_changed":
		return auditStaticLabel(locale, "会话令牌已更新", "Session token changed")
	case "smtp_host":
		return auditStaticLabel(locale, "SMTP 主机", "SMTP host")
	case "smtp_password_changed":
		return auditStaticLabel(locale, "SMTP 密码已更新", "SMTP password changed")
	case "smtp_port":
		return auditStaticLabel(locale, "SMTP 端口", "SMTP port")
	case "smtp_username":
		return auditStaticLabel(locale, "SMTP 用户名", "SMTP username")
	case "spend_limit":
		return auditStaticLabel(locale, "消费限额", "Spend limit")
	case "system_proxy_url":
		return auditStaticLabel(locale, "系统代理", "System proxy")
	case "target_groups":
		return auditStaticLabel(locale, "目标分组", "Target groups")
	case "target_users":
		return auditStaticLabel(locale, "目标用户", "Target users")
	case "tiers":
		return auditStaticLabel(locale, "价格阶梯数", "Pricing tiers")
	case "timed_rules":
		return auditStaticLabel(locale, "定时倍率规则数", "Timed multiplier rules")
	case "title":
		return auditStaticLabel(locale, "标题", "Title")
	case "turnstile_enabled":
		return "Turnstile"
	case "turnstile_secret_changed":
		return auditStaticLabel(locale, "Turnstile Secret 已更新", "Turnstile secret changed")
	case "turnstile_site_key":
		return "Turnstile Site Key"
	case "user_dashboard_custom_panel_enabled":
		return auditStaticLabel(locale, "用户控制台自定义卡片", "User console custom panel")
	case "user_dashboard_custom_panel_html":
		return auditStaticLabel(locale, "用户控制台自定义卡片 HTML", "User console custom panel HTML")
	case "user_concurrent_request_limit":
		return auditStaticLabel(locale, "用户并发请求上限", "Per-user concurrent request limit")
	case "plan_concurrent_request_limit":
		return auditStaticLabel(locale, "套餐并发请求上限", "Plan concurrent request limit")
	case "user_ip_concurrent_request_limit":
		return auditStaticLabel(locale, "单用户同 IP 并发请求上限", "Per-user same-IP concurrent request limit")
	case "user_request_rate_limit_per_minute":
		return auditStaticLabel(locale, "用户请求速率上限（次/分钟）", "Per-user request rate per minute")
	case "user_concurrent_request_limit_override":
		return auditStaticLabel(locale, "单用户并发请求上限", "User concurrent request limit")
	case "user_ip_concurrent_request_limit_override":
		return auditStaticLabel(locale, "\u5355\u7528\u6237\u540c IP \u5e76\u53d1\u8bf7\u6c42\u4e0a\u9650", "User same-IP concurrent request limit")
	case "user_request_rate_limit_override":
		return auditStaticLabel(locale, "单用户请求速率上限（次/分钟）", "User request rate per minute")
	case "username":
		return auditStaticLabel(locale, "用户名", "Username")
	case "wechatpay_api_v3_key_changed":
		return auditStaticLabel(locale, "微信支付 APIv3 密钥已更新", "WeChat Pay APIv3 key changed")
	case "wechatpay_app_id":
		return auditStaticLabel(locale, "微信支付 AppID", "WeChat Pay AppID")
	case "wechatpay_enabled":
		return auditStaticLabel(locale, "微信支付", "WeChat Pay")
	case "wechatpay_mch_id":
		return auditStaticLabel(locale, "微信支付商户号", "WeChat Pay merchant ID")
	case "wechatpay_merchant_serial_no":
		return auditStaticLabel(locale, "微信支付商户证书序列号", "WeChat Pay merchant serial no.")
	case "wechatpay_notify_url":
		return auditStaticLabel(locale, "微信支付通知地址", "WeChat Pay notify URL")
	case "wechatpay_private_key_changed":
		return auditStaticLabel(locale, "微信支付商户私钥已更新", "WeChat Pay private key changed")
	case "wechatpay_public_key_changed":
		return auditStaticLabel(locale, "微信支付公钥已更新", "WeChat Pay public key changed")
	case "wechatpay_public_key_id":
		return auditStaticLabel(locale, "微信支付公钥 ID", "WeChat Pay public key ID")
	default:
		return ""
	}
}

func auditMessageFieldValue(locale, key, value string) string {
	value = auditDecodeFieldValue(value)
	switch strings.TrimSpace(key) {
	case "action":
		return auditAccountBatchActionValue(locale, value)
	case "amount", "current", "delta", "input", "cached_input", "output", "previous", "request", "spend_limit", "new_user_reward", "inviter_reward", "invitee_reward", "payment_min_recharge", "payment_max_recharge":
		if amount, err := core.ParseNanoUSDDecimal(value); err == nil {
			if key == "delta" {
				return signedAuditExactUSDDisplay(amount)
			}
			return auditExactUSDDisplay(amount)
		}
	case "account_group":
		return accountGroupLabelText(locale, value)
	case "allow_public_registration", "alipay_enabled", "alipay_private_key_changed", "alipay_public_key_changed", "claude_oauth_enabled", "cloudmail_password_changed", "config_changed", "control_disabled", "enabled", "fixed", "github_login_enabled", "github_login_secret_changed", "google_login_enabled", "google_login_secret_changed", "invitation_enabled", "login_auto_create_user", "new_user_reward_enabled", "openai_oauth_enabled", "password_changed", "plan_billing_enabled", "registration_email_allowlist_enabled", "registration_verification_enabled", "require_invitation_code", "show_in_client_editor", "smtp_password_changed", "turnstile_enabled", "turnstile_secret_changed", "wechatpay_api_v3_key_changed", "wechatpay_enabled", "wechatpay_private_key_changed", "wechatpay_public_key_changed", "access_token_changed", "refresh_token_changed", "session_token_changed":
		return auditBoolText(locale, value)
	case "email_provider":
		return auditEmailProviderText(locale, value)
	case "groups", "target_groups", "visible_users":
		return auditGroupListText(locale, value)
	case "provider":
		return auditProviderText(locale, value)
	case "payment_recharge_input_mode":
		return auditRechargeInputModeText(locale, value)
	case "role":
		return auditRoleValueText(locale, value)
	case "scope":
		return auditScopeText(locale, value)
	case "source":
		return auditSourceText(locale, value)
	case "status":
		return auditStatusValueText(locale, value)
	case "user_concurrent_request_limit_override", "user_ip_concurrent_request_limit_override", "user_request_rate_limit_override":
		return auditUserConcurrentRequestLimitOverrideText(locale, value)
	}
	return auditHumanMessageText(locale, value)
}

func auditRechargeInputModeText(locale, value string) string {
	switch strings.TrimSpace(value) {
	case core.RechargeInputModePaymentCNY:
		return translate(locale, "payment_recharge_input_mode_payment_cny")
	default:
		return translate(locale, "payment_recharge_input_mode_balance_usd")
	}
}

func auditUserConcurrentRequestLimitOverrideText(locale, value string) string {
	switch strings.TrimSpace(value) {
	case "inherit":
		return translate(locale, "inherit_system_default")
	case "0":
		return translate(locale, "unlimited")
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditHumanKey(locale, key string) string {
	key = strings.TrimSpace(strings.ReplaceAll(key, "_", " "))
	if key == "" {
		return translate(locale, "details")
	}
	return key
}

func auditHumanMessageText(locale, value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, providers.ErrorCodeCredentialExpired) {
		if locale == localeZH {
			return "凭据已过期或被上游拒绝，请重新授权账号"
		}
		return "credential expired or rejected by upstream; reconnect the account"
	}
	switch value {
	case "true", "false":
		return auditBoolText(locale, value)
	case "active":
		if locale == localeZH {
			return "正常"
		}
		return "active"
	case "disabled":
		return translate(locale, "disabled")
	case "openai_chatgpt_usage":
		if locale == localeZH {
			return "OpenAI ChatGPT 用量"
		}
		return "OpenAI ChatGPT usage"
	case "oauth":
		return "OAuth"
	}
	return truncateRunes(value, 140)
}

func auditAccountBatchActionValue(locale, value string) string {
	switch strings.TrimSpace(value) {
	case "disable":
		return translate(locale, "account_batch_action_disable")
	case "enable":
		return translate(locale, "account_batch_action_enable")
	case "delete":
		return translate(locale, "account_batch_action_delete")
	case "move_group":
		return translate(locale, "account_batch_action_move_group")
	case "refresh_quota":
		return translate(locale, "account_batch_action_refresh_quota")
	case "test":
		return translate(locale, "account_batch_action_test")
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditSystemSettingsResourceText(locale, value string) string {
	if strings.TrimSpace(value) == "global" {
		return auditStaticLabel(locale, "全局设置", "Global settings")
	}
	return auditHumanMessageText(locale, value)
}

func auditCountText(locale, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "0"
	}
	if locale == localeZH {
		return value + " 个"
	}
	if value == "1" {
		return value + " item"
	}
	return value + " items"
}

func auditParseAmountField(value string) (int64, bool) {
	amount, err := core.ParseNanoUSDDecimal(strings.TrimSpace(value))
	if err != nil {
		return 0, false
	}
	return amount, strings.TrimSpace(value) != ""
}

func auditAmountFieldText(value string) string {
	amount, ok := auditParseAmountField(value)
	if !ok {
		return ""
	}
	return auditExactUSDDisplay(amount)
}

func auditResultLabel(locale string) string {
	return auditStaticLabel(locale, "结果", "Result")
}

func auditChangeLabel(locale string) string {
	return auditStaticLabel(locale, "变更", "Change")
}

func auditStatusValueText(locale, value string) string {
	switch strings.TrimSpace(value) {
	case "ok":
		return translate(locale, "success")
	case "error":
		return translate(locale, "error")
	case "active":
		if locale == localeZH {
			return "正常"
		}
		return "active"
	case "disabled":
		return translate(locale, "disabled")
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditProviderText(locale, value string) string {
	value = strings.TrimSpace(value)
	switch core.PaymentProvider(value) {
	case core.PaymentProviderAlipay, core.PaymentProviderWeChatPay, core.PaymentProviderPersonalPay:
		return paymentProviderText(locale, core.PaymentProvider(value))
	default:
		return value
	}
}

func auditSourceText(locale, value string) string {
	switch strings.TrimSpace(value) {
	case "openai_chatgpt_usage":
		if locale == localeZH {
			return "OpenAI ChatGPT 用量"
		}
		return "OpenAI ChatGPT usage"
	case "oauth":
		return "OAuth"
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditEmailProviderText(locale, value string) string {
	switch strings.TrimSpace(value) {
	case core.EmailProviderCloudMail:
		return "CloudMail"
	case core.EmailProviderSMTP:
		return "SMTP"
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditGroupListText(locale, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return auditStaticLabel(locale, "无", "none")
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		group := strings.TrimSpace(part)
		if group != "" {
			out = append(out, accountGroupLabelText(locale, group))
		}
	}
	if len(out) == 0 {
		return auditStaticLabel(locale, "无", "none")
	}
	return strings.Join(out, ", ")
}

func auditRoleValueText(locale, value string) string {
	switch core.UserRole(strings.TrimSpace(value)) {
	case core.UserRoleAdmin:
		return auditStaticLabel(locale, "管理员", "Admin")
	case core.UserRoleUser:
		return auditStaticLabel(locale, "用户", "User")
	default:
		return auditHumanMessageText(locale, value)
	}
}

func auditScopeText(locale, value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "all") {
		return auditStaticLabel(locale, "全部账号分组", "All account groups")
	}
	if group := strings.TrimPrefix(value, "group:"); group != value {
		return accountGroupLabelText(locale, group)
	}
	return auditHumanMessageText(locale, value)
}

func auditBoolText(locale, value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		if locale == localeZH {
			return "是"
		}
		return "yes"
	case "false":
		if locale == localeZH {
			return "否"
		}
		return "no"
	default:
		return value
	}
}

func auditExactUSDDisplay(nanoUSD int64) string {
	sign := ""
	if nanoUSD < 0 {
		sign = "-"
		nanoUSD = -nanoUSD
	}
	return sign + "$" + core.FormatNanoUSD(nanoUSD)
}

func signedAuditExactUSDDisplay(nanoUSD int64) string {
	if nanoUSD > 0 {
		return "+" + auditExactUSDDisplay(nanoUSD)
	}
	return auditExactUSDDisplay(nanoUSD)
}

func auditStaticLabel(locale, zh, en string) string {
	if locale == localeZH {
		return zh
	}
	return en
}
