package postgresrepo

func (r *PostgresRepository) initSchemaLocked() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS account_credentials (
			account_id TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE TABLE IF NOT EXISTS account_runtime (
			account_id TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE TABLE IF NOT EXISTS account_groups (
			id TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS models (
			id TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS clients (
			id TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			api_key_hash TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			owner_user_id TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE
		)`,
		`CREATE TABLE IF NOT EXISTS openai_response_bindings (
			response_id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			client_id TEXT NOT NULL DEFAULT '',
			prompt_cache_key TEXT NOT NULL DEFAULT '',
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_openai_response_bindings_account ON openai_response_bindings(account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_openai_response_bindings_client ON openai_response_bindings(client_id)`,
		`CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			updated_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username_key TEXT NOT NULL UNIQUE,
			username TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			inviter_user_id TEXT NOT NULL DEFAULT '',
			created_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			last_login_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_balances (
			user_id TEXT PRIMARY KEY,
			balance_nano_usd BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_balances_balance ON user_balances(balance_nano_usd)`,
		`CREATE TABLE IF NOT EXISTS user_oauth_identities (
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			subject TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			linked_at_ns BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY(user_id, provider),
			UNIQUE(provider, subject),
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE TABLE IF NOT EXISTS user_invitation_codes (
			user_id TEXT PRIMARY KEY,
			signature TEXT NOT NULL UNIQUE,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE TABLE IF NOT EXISTS user_sessions (
			token_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			expires_at_ns BIGINT NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_sessions_user_id ON user_sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_user_sessions_expires_at ON user_sessions(expires_at_ns)`,
		`CREATE TABLE IF NOT EXISTS mcp_tokens (
			id TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL UNIQUE,
			owner_user_id TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			expires_at_ns BIGINT NOT NULL DEFAULT 0,
			last_used_at_ns BIGINT NOT NULL DEFAULT 0,
			revoked_at_ns BIGINT NOT NULL DEFAULT 0,
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL,
			payload TEXT NOT NULL,
			FOREIGN KEY(owner_user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_tokens_owner_created ON mcp_tokens(owner_user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_tokens_hash ON mcp_tokens(token_hash)`,
		`CREATE TABLE IF NOT EXISTS email_verification_codes (
			id TEXT PRIMARY KEY,
			purpose TEXT NOT NULL,
			email_key TEXT NOT NULL,
			created_at_ns BIGINT NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_email_codes_lookup ON email_verification_codes(purpose, email_key, created_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS client_spend (
			client_id TEXT PRIMARY KEY,
			spend_limit_nano_usd BIGINT NOT NULL DEFAULT 0,
			spend_used_nano_usd BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			FOREIGN KEY(client_id) REFERENCES clients(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE TABLE IF NOT EXISTS finance_user_rollups (
			user_id TEXT PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
			balance_nano_usd BIGINT NOT NULL DEFAULT 0,
			recharge_nano_usd BIGINT NOT NULL DEFAULT 0,
			reward_nano_usd BIGINT NOT NULL DEFAULT 0,
			spend_nano_usd BIGINT NOT NULL DEFAULT 0,
			usage_spend_nano_usd BIGINT NOT NULL DEFAULT 0,
			plan_spend_nano_usd BIGINT NOT NULL DEFAULT 0,
			refund_nano_usd BIGINT NOT NULL DEFAULT 0,
			last_payment_at_ns BIGINT NOT NULL DEFAULT 0,
			last_spend_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_user_rollups_spend ON finance_user_rollups(spend_nano_usd DESC, user_id)`,
		`CREATE TABLE IF NOT EXISTS finance_client_rollups (
			client_id TEXT PRIMARY KEY,
			client_name TEXT NOT NULL DEFAULT '',
			owner_user_id TEXT NOT NULL DEFAULT '',
			spend_limit_nano_usd BIGINT NOT NULL DEFAULT 0,
			spend_used_nano_usd BIGINT NOT NULL DEFAULT 0,
			usage_nano_usd BIGINT NOT NULL DEFAULT 0,
			plan_nano_usd BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_client_rollups_spend ON finance_client_rollups(spend_used_nano_usd DESC, client_id)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_client_rollups_owner ON finance_client_rollups(owner_user_id)`,
		`CREATE TABLE IF NOT EXISTS finance_model_rollups (
			model TEXT PRIMARY KEY,
			request_count BIGINT NOT NULL DEFAULT 0,
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			spend_nano_usd BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_model_rollups_spend ON finance_model_rollups(spend_nano_usd DESC, model)`,
		`CREATE TABLE IF NOT EXISTS finance_token_daily_rollups (
			date TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			request_count BIGINT NOT NULL DEFAULT 0,
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			cached_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			image_output_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY(date, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_token_daily_date_total ON finance_token_daily_rollups(date DESC, total_tokens DESC, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_finance_token_daily_user_date ON finance_token_daily_rollups(user_id, date DESC)`,
		`CREATE TABLE IF NOT EXISTS billing_requests (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			client_id TEXT NOT NULL,
			client_name TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			account_label TEXT NOT NULL DEFAULT '',
			failed_account_labels TEXT NOT NULL DEFAULT '',
			account_group TEXT NOT NULL DEFAULT '',
			account_group_multiplier_bps BIGINT NOT NULL DEFAULT 0,
			billing_source TEXT NOT NULL DEFAULT 'cash',
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			fast_mode BOOLEAN NOT NULL DEFAULT FALSE,
			status TEXT NOT NULL,
			estimated_prompt_tokens BIGINT NOT NULL DEFAULT 0,
			estimated_completion_tokens BIGINT NOT NULL DEFAULT 0,
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			cached_prompt_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_5m_tokens BIGINT NOT NULL DEFAULT 0,
			cache_creation_1h_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			image_output_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			input_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			cached_input_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			cache_write_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			cache_write_5m_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			cache_write_1h_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			output_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			image_output_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0,
			reserved_nano_usd BIGINT NOT NULL DEFAULT 0,
			actual_nano_usd BIGINT NOT NULL DEFAULT 0,
			first_token_ms BIGINT NOT NULL DEFAULT 0,
			fingerprint TEXT NOT NULL DEFAULT '',
			cache_diagnostics TEXT NOT NULL DEFAULT '',
			created_at_ns BIGINT NOT NULL,
			settled_at_ns BIGINT NOT NULL DEFAULT 0,
			UNIQUE(request_id, client_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_user_created ON billing_requests(user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_client_created ON billing_requests(client_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_created ON billing_requests(created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_status_created ON billing_requests(status, created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_status_user ON billing_requests(status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_status_client ON billing_requests(status, client_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_user_client_status ON billing_requests(user_id, client_id, status)`,
		`CREATE TABLE IF NOT EXISTS billing_ledger (
			seq BIGSERIAL PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			user_id TEXT NOT NULL,
			client_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			amount_nano_usd BIGINT NOT NULL,
			balance_after_nano_usd BIGINT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_created ON billing_ledger(user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_user_seq ON billing_ledger(user_id, seq DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_settle_user_client_amount ON billing_ledger(user_id, client_id, amount_nano_usd) WHERE client_id <> '' AND kind = 'settle'`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_settle_client_amount_user ON billing_ledger(client_id, amount_nano_usd, user_id) WHERE client_id <> '' AND kind = 'settle'`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_settle_request_client ON billing_ledger(request_id, client_id) WHERE request_id <> '' AND kind = 'settle'`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_settle_created_amount ON billing_ledger(created_at_ns, amount_nano_usd) WHERE kind = 'settle'`,
		`CREATE INDEX IF NOT EXISTS idx_billing_ledger_finance_current_user_kind_created ON billing_ledger(user_id, kind, created_at_ns, amount_nano_usd) WHERE kind IN ('manual_credit', 'account_merge', 'manual_debit', 'plan_purchase', 'settle')`,
		`CREATE TABLE IF NOT EXISTS billing_plans (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			group_name TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			price_nano_usd BIGINT NOT NULL DEFAULT 0,
			period_quota_nano_usd BIGINT NOT NULL DEFAULT 0,
			period_duration_sec BIGINT NOT NULL DEFAULT 86400,
			period_count BIGINT NOT NULL DEFAULT 1,
			sort_order BIGINT NOT NULL DEFAULT 0,
			created_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_plans_enabled_sort ON billing_plans(enabled DESC, sort_order ASC, created_at_ns ASC)`,
		`CREATE TABLE IF NOT EXISTS billing_plan_groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			sale_disabled BOOLEAN NOT NULL DEFAULT FALSE,
			quota_price_ratio TEXT NOT NULL DEFAULT '1:1',
			sort_order BIGINT NOT NULL DEFAULT 0,
			created_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS user_plan_entitlements (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			plan_id TEXT NOT NULL,
			plan_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			price_nano_usd BIGINT NOT NULL DEFAULT 0,
			period_quota_nano_usd BIGINT NOT NULL DEFAULT 0,
			base_period_quota_nano_usd BIGINT NOT NULL DEFAULT 0,
			period_duration_sec BIGINT NOT NULL DEFAULT 86400,
			total_periods BIGINT NOT NULL DEFAULT 1,
			remaining_periods BIGINT NOT NULL DEFAULT 1,
			current_quota_nano_usd BIGINT NOT NULL DEFAULT 0,
			priority BIGINT NOT NULL DEFAULT 0,
			current_period_started_at_ns BIGINT NOT NULL DEFAULT 0,
			current_period_ends_at_ns BIGINT NOT NULL DEFAULT 0,
			expires_at_ns BIGINT NOT NULL DEFAULT 0,
			purchased_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_plan_entitlements_user_status ON user_plan_entitlements(user_id, status, purchased_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS plan_quota_ledger (
			seq BIGSERIAL PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			entitlement_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			client_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			amount_nano_usd BIGINT NOT NULL,
			quota_after_nano_usd BIGINT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_quota_ledger_user_created ON plan_quota_ledger(user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_quota_ledger_entitlement_created ON plan_quota_ledger(entitlement_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_quota_ledger_created ON plan_quota_ledger(created_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS billing_funding_allocations (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL,
			client_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			source TEXT NOT NULL,
			entitlement_id TEXT NOT NULL DEFAULT '',
			reserved_nano_usd BIGINT NOT NULL DEFAULT 0,
			actual_nano_usd BIGINT NOT NULL DEFAULT 0,
			created_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			UNIQUE(request_id, client_id, source, entitlement_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_funding_allocations_request ON billing_funding_allocations(request_id, client_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_funding_allocations_user ON billing_funding_allocations(user_id, created_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS payment_orders (
			id TEXT PRIMARY KEY,
			out_trade_no TEXT NOT NULL UNIQUE,
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			amount_nano_usd BIGINT NOT NULL DEFAULT 0,
			paid_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL,
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_user_created ON payment_orders(user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_status_updated ON payment_orders(status, updated_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_created ON payment_orders(created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_status_created ON payment_orders(status, created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_provider_created ON payment_orders(provider, created_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS payment_refunds (
			id TEXT PRIMARY KEY,
			order_id TEXT NOT NULL,
			out_trade_no TEXT NOT NULL,
			user_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			status TEXT NOT NULL,
			amount_nano_usd BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL,
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_refunds_order_created ON payment_refunds(order_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_refunds_user_created ON payment_refunds(user_id, created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_refunds_created ON payment_refunds(created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_refunds_status_created ON payment_refunds(status, created_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_refunds_status_updated ON payment_refunds(status, updated_at_ns DESC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS site_messages (
			id TEXT PRIMARY KEY,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			payload TEXT NOT NULL,
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_site_messages_created ON site_messages(created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_site_messages_enabled_created ON site_messages(enabled, created_at_ns DESC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS site_message_reads (
			message_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			read_at_ns BIGINT NOT NULL,
			PRIMARY KEY(message_id, user_id),
			FOREIGN KEY(message_id) REFERENCES site_messages(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_site_message_reads_user ON site_message_reads(user_id)`,
		`CREATE TABLE IF NOT EXISTS support_tickets (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			title TEXT NOT NULL DEFAULT '',
			last_message TEXT NOT NULL DEFAULT '',
			last_actor_id TEXT NOT NULL DEFAULT '',
			last_read_by_user_at_ns BIGINT NOT NULL DEFAULT 0,
			last_read_by_admin_at_ns BIGINT NOT NULL DEFAULT 0,
			created_at_ns BIGINT NOT NULL,
			updated_at_ns BIGINT NOT NULL,
			closed_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_support_tickets_user_updated ON support_tickets(user_id, updated_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_support_tickets_status_updated ON support_tickets(status, updated_at_ns DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_support_tickets_updated ON support_tickets(updated_at_ns DESC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS support_messages (
			id TEXT PRIMARY KEY,
			ticket_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_role TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at_ns BIGINT NOT NULL,
			payload TEXT NOT NULL,
			FOREIGN KEY(ticket_id) REFERENCES support_tickets(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_support_messages_ticket_created ON support_messages(ticket_id, created_at_ns ASC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS documents (
			id TEXT PRIMARY KEY,
			slug_key TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL,
			pinned BOOLEAN NOT NULL DEFAULT FALSE,
			noindex BOOLEAN NOT NULL DEFAULT FALSE,
			search_text TEXT NOT NULL DEFAULT '',
			visibility TEXT NOT NULL DEFAULT 'public',
			category_key TEXT NOT NULL DEFAULT '',
			sort_order BIGINT NOT NULL DEFAULT 0,
			published_at_ns BIGINT NOT NULL DEFAULT 0,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_status_updated ON documents(status, updated_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS document_redirects (
			from_slug_key TEXT PRIMARY KEY,
			from_slug TEXT NOT NULL,
			to_slug TEXT NOT NULL,
			status_code BIGINT NOT NULL DEFAULT 301,
			created_at_ns BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_targets (
			id TEXT PRIMARY KEY,
			account_group TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT true,
			public_visible BOOLEAN NOT NULL DEFAULT true,
			interval_seconds BIGINT NOT NULL DEFAULT 300,
			updated_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_results (
			id TEXT PRIMARY KEY,
			target_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT '',
			latency_ms BIGINT NOT NULL DEFAULT 0,
			checked_at_ns BIGINT NOT NULL DEFAULT 0,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_targets_group_model ON monitor_targets(account_group, model)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_results_target_checked ON monitor_results(target_id, checked_at_ns DESC)`,
		`CREATE TABLE IF NOT EXISTS audit (
			seq BIGSERIAL PRIMARY KEY,
			event_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			actor_text TEXT NOT NULL DEFAULT '',
			resource_text TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL,
			summary_payload TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_kind_status_seq ON audit(kind, status, seq DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_terms (
			seq BIGINT NOT NULL,
			term_type TEXT NOT NULL,
			term TEXT NOT NULL,
			PRIMARY KEY(seq, term_type, term),
			FOREIGN KEY(seq) REFERENCES audit(seq) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_terms_type_term_seq ON audit_terms(term_type, term, seq DESC)`,
	}
	for _, statement := range statements {
		if _, err := r.db.Exec(statement); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`ALTER TABLE billing_plan_groups ADD COLUMN IF NOT EXISTS quota_price_ratio TEXT NOT NULL DEFAULT '1:1'`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS account_label TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS failed_account_labels TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_creation_5m_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_creation_1h_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS image_output_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_write_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_write_5m_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_write_1h_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS image_output_price_nano_usd_per_1m BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_requests ADD COLUMN IF NOT EXISTS cache_diagnostics TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE openai_response_bindings ADD COLUMN IF NOT EXISTS prompt_cache_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE finance_token_daily_rollups ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE finance_token_daily_rollups ADD COLUMN IF NOT EXISTS image_output_tokens BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := r.db.Exec(statement); err != nil {
			return err
		}
	}
	if err := r.rebuildFinanceTokenDailyRollups(); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_clients_api_key_hash ON clients(api_key_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,
		`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`,
		`CREATE INDEX IF NOT EXISTS idx_users_enabled ON users(enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_users_inviter ON users(inviter_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_users_created ON users(created_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_users_updated ON users(updated_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_users_last_login ON users(last_login_at_ns DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_clients_owner ON clients(owner_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_clients_owner_enabled_name ON clients(owner_user_id, enabled DESC, name ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_list ON documents(status, pinned DESC, updated_at_ns DESC, slug_key ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_seo ON documents(status, noindex, pinned DESC, updated_at_ns DESC, slug_key ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_status_user ON billing_requests(status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_requests_user_client_status ON billing_requests(user_id, client_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_orders_status_paid_at ON payment_orders(status, paid_at_ns DESC)`,
	} {
		if _, err := r.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
