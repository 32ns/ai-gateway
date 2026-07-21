package web

import (
	"io/fs"
	"net/http"
	"strings"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFiles, staticErr := fs.Sub(s.assetFS, "static")
	if s.personalPayAndroid != nil {
		mux.HandleFunc("/personalpay/android/ws", s.handlePersonalPayAndroid)
	}
	s.registerPublicRoutes(mux)
	s.registerConsoleRoutes(mux)
	s.registerAdminRoutes(mux)
	s.registerProtocolRoutes(mux)
	s.registerUtilityRoutes(mux, staticFiles, staticErr)
	return s.withTrustedProxyContext(mux)
}

func (s *Server) handlePersonalPayAndroid(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.personalPayAndroid == nil {
		http.NotFound(w, r)
		return
	}
	settings := s.controlCurrentSettings().Payment.PersonalPay
	if !settings.Enabled {
		http.Error(w, "personalpay is disabled", http.StatusForbidden)
		return
	}
	if strings.TrimSpace(settings.AndroidToken) == "" {
		http.Error(w, "personalpay android token is required", http.StatusUnauthorized)
		return
	}
	s.personalPayAndroid.ServeHTTP(w, r)
}

func (s *Server) registerPublicRoutes(mux *http.ServeMux) {
	dashboardHandler := s.requireConsoleUser(http.HandlerFunc(s.handleDashboard))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if user, err := s.currentUserFromSession(r); err != nil || user.ID == "" {
				s.handleHome(w, r)
				return
			}
		}
		dashboardHandler.ServeHTTP(w, r)
	})
	mux.Handle("/login", http.HandlerFunc(s.handleLogin))
	mux.Handle("/login/oauth/", http.HandlerFunc(s.handleLoginOAuth))
	mux.Handle("/password/forgot", http.HandlerFunc(s.handlePasswordForgot))
	mux.Handle("/password/reset", http.HandlerFunc(s.handlePasswordReset))
	mux.Handle("/register", http.HandlerFunc(s.handleRegister))
	mux.Handle("/register/email-code/send", http.HandlerFunc(s.handleRegisterEmailCodeSend))
	mux.HandleFunc("/models", s.handleUserModelsPage)
	mux.HandleFunc("/plans", s.handlePlansPage)
	mux.HandleFunc("/status", s.handleStatusPage)
	mux.Handle("/docs", http.HandlerFunc(s.handleDocs))
	mux.Handle("/docs/", http.HandlerFunc(s.handleDocs))
	mux.HandleFunc("/robots.txt", s.handleRobots)
	mux.HandleFunc("/"+indexNowKey+".txt", s.handleIndexNowKey)
	mux.HandleFunc("/sitemap.xml", s.handleSitemap)
	mux.HandleFunc("/sitemap-docs.xml", s.handleSitemap)
	mux.HandleFunc("/llms.txt", s.handleLLMSText)
	mux.HandleFunc("/payments/notify/", s.handlePaymentNotify)
	mux.HandleFunc("/api/internal/balance-migrations/claim", s.handleBalanceMigrationClaim)
}

func (s *Server) registerConsoleRoutes(mux *http.ServeMux) {
	imageLabHandler := func(handler http.HandlerFunc) http.Handler {
		return s.requireImageLabAccess(http.HandlerFunc(handler))
	}
	mux.Handle("/logout", s.requireConsoleUser(http.HandlerFunc(s.handleLogout)))
	mux.Handle("/console/events", s.requireConsoleUser(http.HandlerFunc(s.handleConsoleEvents)))
	mux.Handle("/console/events/state", s.requireConsoleUser(http.HandlerFunc(s.handleConsoleEventState)))
	mux.Handle("/profile/password", s.requireConsoleUser(http.HandlerFunc(s.handlePasswordPage)))
	mux.Handle("/profile/oauth", s.requireConsoleUser(http.HandlerFunc(s.handleProfileOAuthPage)))
	mux.Handle("/profile/oauth/", s.requireConsoleUser(http.HandlerFunc(s.handleProfileOAuth)))
	mux.Handle("/profile/balance-migration/code", s.requireConsoleUser(http.HandlerFunc(s.handleBalanceMigrationCode)))
	mux.Handle("/payments/create", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentCreate)))
	mux.Handle("/payments/cancel", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentCancel)))
	mux.Handle("/payments/qr", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentQRCode)))
	mux.Handle("/payments/status", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentStatus)))
	mux.Handle("/payments/refresh", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentRefresh)))
	mux.Handle("/payments/return/", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentReturn)))
	mux.Handle("/payments", s.requireConsoleUser(http.HandlerFunc(s.handlePaymentsPage)))
	mux.Handle("/plans/purchase", s.requireConsoleUser(http.HandlerFunc(s.handlePlanPurchase)))
	mux.Handle("/plans/entitlements/priority", s.requireConsoleUser(http.HandlerFunc(s.handlePlanEntitlementPriority)))
	mux.Handle("/messages/users/search", s.requireConsoleUser(http.HandlerFunc(s.handleMessageUserSearch)))
	mux.Handle("/messages", s.requireConsoleUser(http.HandlerFunc(s.handleMessagesPage)))
	mux.Handle("/messages/", s.requireConsoleUser(http.HandlerFunc(s.handleMessageActions)))
	mux.Handle("/support/ws", s.requireConsoleUser(http.HandlerFunc(s.handleSupportWebSocket)))
	mux.Handle("/support", s.requireConsoleUser(http.HandlerFunc(s.handleSupportPage)))
	mux.Handle("/images/api/bootstrap", imageLabHandler(s.handleImageLabBootstrap))
	mux.Handle("/images/api/generate", imageLabHandler(s.handleImageLabGenerate))
	mux.Handle("/images/api/jobs", imageLabHandler(s.handleImageLabJobsList))
	mux.Handle("/images/api/jobs/", imageLabHandler(s.handleImageLabJobActions))
	mux.Handle("/images/api/proxy", imageLabHandler(s.handleImageLabProxy))
	mux.Handle("/images", imageLabHandler(s.handleImageLabPage))
	mux.Handle("/clients", s.requireConsoleUser(http.HandlerFunc(s.handleClientsPage)))
	mux.Handle("/clients/", s.requireConsoleUser(http.HandlerFunc(s.handleClientActions)))
	mux.Handle("/logs", s.requireConsoleUser(http.HandlerFunc(s.handleUsageLogsPage)))
}

func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	adminOnly := func(handler http.HandlerFunc) http.Handler {
		return s.requireAdminOnly(http.HandlerFunc(handler))
	}
	mux.Handle("/admin/accounts", adminOnly(s.handleAccountsPage))
	mux.Handle("/admin/accounts/export", adminOnly(s.handleAccountPoolExport))
	mux.Handle("/admin/accounts/import", adminOnly(s.handleAccountPoolImport))
	mux.Handle("/admin/accounts/reconcile", adminOnly(s.handleAccountRuntimeReconcile))
	mux.Handle("/admin/proxy-test", adminOnly(s.handleProxyTest))
	mux.Handle("/admin/email-test", adminOnly(s.handleEmailTest))
	mux.Handle("/admin/account-groups", adminOnly(s.handleAccountGroupsCreate))
	mux.Handle("/admin/account-groups/", adminOnly(s.handleAccountGroupActions))
	mux.Handle("/admin/models", adminOnly(s.handleModelsPage))
	mux.Handle("/admin/models/", adminOnly(s.handleModelActions))
	mux.Handle("/admin/gateway", adminOnly(s.handleGatewayPage))
	mux.Handle("/admin/status", adminOnly(s.handleAdminStatusPage))
	mux.Handle("/admin/status/targets", adminOnly(s.handleAdminStatusTargetCreate))
	mux.Handle("/admin/status/targets/", adminOnly(s.handleAdminStatusTargetActions))
	mux.Handle("/admin/finance", adminOnly(s.handleFinancePage))
	mux.Handle("/admin/finance/", adminOnly(s.handleFinanceActions))
	mux.Handle("/admin/plans", adminOnly(s.handleAdminPlansPage))
	mux.Handle("/admin/plans/grant", adminOnly(s.handleAdminPlanGrant))
	mux.Handle("/admin/plans/entitlements/cancel", adminOnly(s.handleAdminPlanEntitlementCancel))
	mux.Handle("/admin/plans/", adminOnly(s.handleAdminPlanActions))
	mux.Handle("/admin/plan-groups", adminOnly(s.handleAdminPlanGroupsCreate))
	mux.Handle("/admin/plan-groups/", adminOnly(s.handleAdminPlanGroupActions))
	mux.Handle("/admin/settings", adminOnly(s.handleSettingsPage))
	mux.Handle("/admin/support/ws", adminOnly(s.handleAdminSupportWebSocket))
	mux.Handle("/admin/support", adminOnly(s.handleAdminSupportPage))
	mux.Handle("/admin/docs", adminOnly(s.handleAdminDocsPage))
	mux.Handle("/admin/docs/", adminOnly(s.handleAdminDocActions))
	mux.Handle("/admin/mcp-tokens", adminOnly(s.handleMCPTokensPage))
	mux.Handle("/admin/mcp-tokens/", adminOnly(s.handleMCPTokenActions))
	mux.Handle("/admin/personalpay/devices/", adminOnly(s.handlePersonalPayDeviceActions))
	mux.Handle("/admin/personalpay/accounts/", adminOnly(s.handlePersonalPayAccountActions))
	mux.Handle("/admin/backup/export", adminOnly(s.handleBackupExport))
	mux.Handle("/admin/backup/inspect", adminOnly(s.handleBackupInspect))
	mux.Handle("/admin/backup/restore", adminOnly(s.handleBackupRestore))
	mux.Handle("/admin/backup", adminOnly(s.handleBackupPage))
	mux.Handle("/admin/audit", adminOnly(s.handleAuditPage))
	mux.Handle("/admin/users", adminOnly(s.handleUsersPage))
	mux.Handle("/admin/users/", adminOnly(s.handleUserActions))
	mux.Handle("/admin/connect/openai/oauth", adminOnly(s.handleOpenAIOAuthStart))
	mux.Handle("/admin/connect/openai/oauth/poll", adminOnly(s.handleOpenAIOAuthPoll))
	mux.Handle("/admin/connect/openai/codex-import-upload", adminOnly(s.handleOpenAICodexImportUpload))
	mux.Handle("/admin/connect/claude/oauth", adminOnly(s.handleClaudeOAuthStart))
	mux.Handle("/admin/connect/claude/oauth/callback", adminOnly(s.handleClaudeOAuthCallback))
	mux.Handle("/admin/connect/", adminOnly(s.handleConnectStart))
	mux.Handle("/admin/accounts/connect", adminOnly(s.handleConnectComplete))
	mux.Handle("/admin/accounts/batch/jobs/", adminOnly(s.handleAccountBatchJobActions))
	mux.Handle("/admin/accounts/batch", adminOnly(s.handleAccountBatchAction))
	mux.Handle("/admin/accounts/", adminOnly(s.handleAccountActions))
}

func (s *Server) registerProtocolRoutes(mux *http.ServeMux) {
	mux.Handle("/mcp", http.HandlerFunc(s.handleMCP))
	mux.Handle("/v1/models", s.requireAPIKey(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/models/", s.requireAPIKey(http.HandlerFunc(s.handleProtocolModelActions)))
	mux.Handle("/ag/v1/account/quota", s.requireAPIKey(http.HandlerFunc(s.handleGatewayClientQuota)))
	mux.Handle("/ag/v1/dashboard/billing/subscription", s.requireAPIKey(http.HandlerFunc(s.handleGatewayBillingSubscription)))
	mux.Handle("/ag/v1/dashboard/billing/usage", s.requireAPIKey(http.HandlerFunc(s.handleGatewayBillingUsage)))
	mux.Handle("/v1/embeddings", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIEmbeddings)))
	mux.Handle("/v1/chat/completions", s.requireAPIKey(http.HandlerFunc(s.handleOpenAICompletions)))
	mux.Handle("/v1/responses", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIResponses)))
	mux.Handle("/v1/responses/compact", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIResponsesCompact)))
	mux.Handle("/v1/responses/input_tokens", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIResponseInputTokens)))
	mux.Handle("/v1/responses/", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIResponseResource)))
	mux.Handle("/v1/moderations", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIModerations)))
	mux.Handle("/v1/images/generations", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIImageGenerations)))
	mux.Handle("/v1/images/edits", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIImageMultipart("/v1/images/edits"))))
	mux.Handle("/v1/audio/speech", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIAudioSpeech)))
	mux.Handle("/v1/audio/transcriptions", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIAudioMultipart("/v1/audio/transcriptions"))))
	mux.Handle("/v1/audio/translations", s.requireAPIKey(http.HandlerFunc(s.handleOpenAIAudioMultipart("/v1/audio/translations"))))
	mux.Handle("/v1/messages/count_tokens", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicCountTokens)))
	mux.Handle("/v1/messages", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicMessages)))
	mux.Handle("/api/v1/messages/count_tokens", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicCountTokens)))
	mux.Handle("/api/v1/messages", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicMessages)))
	mux.Handle("/anthropic/v1/messages/count_tokens", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicCountTokens)))
	mux.Handle("/anthropic/v1/messages", s.requireAPIKey(http.HandlerFunc(s.handleAnthropicMessages)))
}

func (s *Server) registerUtilityRoutes(mux *http.ServeMux, staticFiles fs.FS, staticErr error) {
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/support-notification-sw.js", s.handleSupportNotificationServiceWorker)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconICO())
	})
	if staticErr == nil {
		mux.Handle("/static/", cacheStaticAssets(http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles)))))
	} else {
		mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, staticErr.Error(), http.StatusInternalServerError)
		})
	}
}

func (s *Server) handleSupportNotificationServiceWorker(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/support-notification-sw.js" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Service-Worker-Allowed", "/")
	_, _ = w.Write([]byte(`self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const targetURL = new URL(event.notification?.data?.url || "/support", self.location.origin).href;
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of windows) {
      if (client.url === targetURL && "focus" in client) {
        return client.focus();
      }
    }
    if (self.clients.openWindow) {
      return self.clients.openWindow(targetURL);
    }
  })());
});
`))
}

func cacheStaticAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if strings.HasSuffix(strings.ToLower(r.URL.Path), ".svg") {
			w.Header().Set("Content-Type", "image/svg+xml")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withTrustedProxyContext(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	proxies := defaultTrustedProxies
	if s != nil && len(s.trustedProxies.prefixes) > 0 {
		proxies = s.trustedProxies
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withTrustedProxies(r.Context(), proxies)))
	})
}
