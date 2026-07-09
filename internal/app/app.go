package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/backup"
	"github.com/32ns/ai-gateway/internal/config"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/netproxy"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
	"github.com/32ns/ai-gateway/internal/web"
	personalpay "personalpay/sdk-go"
)

type Options struct {
	ConfigPath string
	Host       string
	Port       string
}

type ConfigLoader func(config.Options) (config.Config, error)

type RepositoryFactory func(config.Config) (storage.Repository, func() error, error)

type Hooks struct {
	OnBeforeSeed func(*BuildContext) error
	OnAfterBuild func(*Service) error
}

type BuildContext struct {
	Config     config.Config
	Repository storage.Repository
	Registry   *providers.Registry
	Control    *controlplane.Service
}

type Builder struct {
	options           Options
	cfg               *config.Config
	configLoader      ConfigLoader
	repositoryFactory RepositoryFactory
	hooks             Hooks
}

type Service struct {
	mu         sync.RWMutex
	restoreMu  sync.Mutex
	inFlight   sync.WaitGroup
	cancelOnce sync.Once
	restoring  bool

	options Options
	builder *Builder
	handler http.Handler

	Config     config.Config
	Control    *controlplane.Service
	Gateway    *gateway.Service
	Web        *web.Server
	HTTPServer *http.Server
	Startup    StartupInfo

	cancelRequests context.CancelFunc
	closers        []func() error
}

type runtimeState struct {
	Config  config.Config
	Control *controlplane.Service
	Gateway *gateway.Service
	Web     *web.Server
	Startup StartupInfo
	Handler http.Handler
	Closers []func() error
}

type StartupInfo struct {
	AdminAccount                  core.User
	AdminSeeded                   bool
	ProtocolClient                core.APIClient
	ProtocolClientSeeded          bool
	AuditLimit                    int
	GatewayAuditErrors            bool
	GatewayAuditRetentionDays     int
	AbandonedReservationsReleased int
	AbandonedReservationsNanoUSD  int64
}

type Logger func(format string, v ...any)

func Build(options Options) (*Service, error) {
	return NewBuilder().WithOptions(options).Build()
}

func NewBuilder() *Builder {
	return &Builder{}
}

func (b *Builder) WithOptions(options Options) *Builder {
	b.options = options
	return b
}

func (b *Builder) WithConfigPath(path string) *Builder {
	b.options.ConfigPath = path
	return b
}

func (b *Builder) WithConfig(cfg config.Config) *Builder {
	b.cfg = &cfg
	return b
}

func (b *Builder) WithConfigLoader(loader ConfigLoader) *Builder {
	b.configLoader = loader
	return b
}

func (b *Builder) WithRepositoryFactory(factory RepositoryFactory) *Builder {
	b.repositoryFactory = factory
	return b
}

func (b *Builder) WithRepository(repo storage.Repository, closeFn func() error) *Builder {
	b.repositoryFactory = func(config.Config) (storage.Repository, func() error, error) {
		return repo, closeFn, nil
	}
	return b
}

func (b *Builder) WithHooks(hooks Hooks) *Builder {
	b.hooks = hooks
	return b
}

func (b *Builder) Build() (_ *Service, err error) {
	cfg, err := b.loadConfig()
	if err != nil {
		return nil, err
	}
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	svc := &Service{
		options:        b.options,
		builder:        b,
		cancelRequests: cancelRequests,
	}
	runtime, err := b.buildRuntime(cfg, svc)
	if err != nil {
		cancelRequests()
		return nil, err
	}
	svc.installRuntime(runtime)
	svc.HTTPServer = &http.Server{
		Addr:              cfg.Address,
		Handler:           http.HandlerFunc(svc.ServeHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return requestCtx
		},
	}
	if b.hooks.OnAfterBuild != nil {
		if err = b.hooks.OnAfterBuild(svc); err != nil {
			_ = svc.Close()
			return nil, fmt.Errorf("after build hook: %w", err)
		}
	}
	return svc, nil
}

func (b *Builder) buildRuntime(cfg config.Config, owner *Service) (_ runtimeState, err error) {
	runtimeStarted := time.Now()
	defer logStartupDuration("runtime build", runtimeStarted, true)
	stageStarted := time.Now()
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	repo, closeRepo, err := b.buildRepository(cfg)
	if err != nil {
		cancelRuntime()
		return runtimeState{}, err
	}
	logStartupDuration("open repository", stageStarted, false)
	var closers []func() error
	if closeRepo != nil {
		closers = append(closers, closeRepo)
	}
	defer func() {
		if err != nil {
			cancelRuntime()
			_ = closeRuntime(closers)
		}
	}()

	stageStarted = time.Now()
	retentionSettings, err := storage.LoadStartupSystemSettings(repo)
	if err != nil {
		return runtimeState{}, fmt.Errorf("load retention settings: %w", err)
	}
	retentionSettings = core.NormalizeSystemSettings(retentionSettings)
	auditLimit := retentionSettings.Retention.AuditLimit
	if retentionSettings.UpdatedAt.IsZero() && cfg.AuditLimit > 0 {
		auditLimit = cfg.AuditLimit
	}
	gatewayAuditErrors := retentionSettings.Retention.GatewayAuditErrors
	gatewayAuditRetentionDays := retentionSettings.Retention.GatewayAuditRetentionDays
	if retentionSettings.UpdatedAt.IsZero() {
		gatewayAuditErrors = cfg.GatewayAuditErrors
		gatewayAuditRetentionDays = cfg.GatewayAuditRetentionDays
	}
	if err = repo.ConfigureAuditLimit(auditLimit); err != nil {
		return runtimeState{}, fmt.Errorf("configure audit limit: %w", err)
	}
	if err = repo.ConfigureUsageLogRetention(retentionSettings.Retention.UsageLogMaxAgeDays); err != nil {
		return runtimeState{}, fmt.Errorf("configure usage log retention: %w", err)
	}
	if err = repo.ConfigureBillingLedgerRetention(retentionSettings.Retention.BillingLedgerRetentionDays); err != nil {
		return runtimeState{}, fmt.Errorf("configure billing ledger retention: %w", err)
	}
	gatewayAuditRetentionConfig := 0
	if !cfg.GatewayAudit && gatewayAuditErrors {
		gatewayAuditRetentionConfig = gatewayAuditRetentionDays
	}
	if err = repo.ConfigureGatewayAuditRetention(gatewayAuditRetentionConfig); err != nil {
		return runtimeState{}, fmt.Errorf("configure gateway audit retention: %w", err)
	}
	logStartupDuration("configure retention", stageStarted, false)

	stageStarted = time.Now()
	registry := providers.NewRegistry(
		&providers.OpenAIAdapter{},
		&providers.ClaudeAdapter{},
	)
	netproxy.ConfigureTransport(netproxy.TransportConfig{
		MaxIdleConns:        cfg.UpstreamMaxIdleConns,
		MaxIdleConnsPerHost: cfg.UpstreamMaxIdleConnsPerHost,
		MaxConnsPerHost:     cfg.UpstreamMaxConnsPerHost,
	})
	providers.ConfigureHTTPTransport()

	control := controlplane.New(repo, registry)
	control.SetGatewayAuditRetentionEnabled(!cfg.GatewayAudit)
	personalPaySDK, err := personalpay.Open(personalpay.Options{
		DefaultExpireAfter: personalPayExpireAfter(retentionSettings.Payment.PersonalPay),
		AndroidToken:       personalPayAndroidToken(retentionSettings),
		Notifier: personalpay.NotifierFunc(func(notification personalpay.OrderNotification) {
			go applyPersonalPayNotification(runtimeCtx, control, notification)
		}),
	})
	if err != nil {
		return runtimeState{}, fmt.Errorf("open personalpay sdk: %w", err)
	}
	personalPaySDK.StartBackground()
	paymentClient := payments.NewClient(nil)
	paymentClient.SetPersonalPayEngine(personalPaySDK)
	control.SetPaymentClient(paymentClient)
	control.SetSystemSettingsHook(func(settings core.SystemSettings) {
		configurePersonalPaySDK(personalPaySDK, settings)
	})
	logStartupDuration("initialize services", stageStarted, false)
	closers = append(closers, func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return personalPaySDK.Shutdown(shutdownCtx)
	})
	buildContext := &BuildContext{
		Config:     cfg,
		Repository: repo,
		Registry:   registry,
		Control:    control,
	}
	if b.hooks.OnBeforeSeed != nil {
		if err = b.hooks.OnBeforeSeed(buildContext); err != nil {
			return runtimeState{}, fmt.Errorf("before seed hook: %w", err)
		}
	}

	stageStarted = time.Now()
	if err = control.SeedDefaults(); err != nil {
		return runtimeState{}, fmt.Errorf("seed defaults: %w", err)
	}
	adminAccount, adminSeeded, err := control.EnsureAdminUser("", "")
	if err != nil {
		return runtimeState{}, fmt.Errorf("ensure admin user: %w", err)
	}
	seededClient, clientSeeded, err := control.EnsureProtocolClient(cfg.APIKey)
	if err != nil {
		return runtimeState{}, fmt.Errorf("ensure protocol client: %w", err)
	}
	logStartupDuration("seed defaults", stageStarted, false)

	stageStarted = time.Now()
	accountPool := accounts.NewPool(repo)
	failoverEngine := failover.NewEngine(accountPool, registry)
	router := routing.NewRouter()
	gatewayRepo := storage.NewGatewayAuditFilterRepositoryWithOptions(repo, storage.GatewayAuditFilterOptions{
		Enabled:    cfg.GatewayAudit,
		ErrorsOnly: cfg.GatewayAuditErrors,
	})
	gatewayService := gateway.New(gatewayRepo, router, failoverEngine).WithQuotaRegistry(registry)

	webServer := web.NewServerWithOptions(control, gatewayService, web.ServerOptions{
		ConfigPath:               AbsoluteConfigPath(b.options.ConfigPath),
		StatePath:                cfg.StatePath,
		DatabaseBackend:          cfg.DatabaseBackend,
		PostgresDSN:              cfg.PostgresDSN,
		MasterKey:                cfg.MasterKey,
		ProtocolRequestBodyLimit: int64(cfg.ProtocolRequestBodyLimit),
		RestoreBackup:            owner.RestoreBackup,
		PersonalPayAndroid:       personalPaySDK.AndroidWebSocketHandler(),
		TrustedProxyCIDRs:        cfg.TrustedProxyCIDRs,
	})
	logStartupDuration("initialize web server", stageStarted, false)
	stageStarted = time.Now()
	if err = control.ApplySystemSettingsWithFallbacks(cfg.AuditLimit, controlplane.SystemSettingsFallbacks{
		PublicBaseURL:             cfg.PublicBaseURL,
		GatewayAuditErrors:        cfg.GatewayAuditErrors,
		GatewayAuditRetentionDays: cfg.GatewayAuditRetentionDays,
	}); err != nil {
		return runtimeState{}, fmt.Errorf("apply system settings: %w", err)
	}
	logStartupDuration("apply system settings", stageStarted, false)
	stageStarted = time.Now()
	abandonedReservations, err := control.ReleaseAbandonedGatewayReservations(time.Now().UTC(), "released after gateway restart")
	if err != nil {
		return runtimeState{}, fmt.Errorf("release abandoned gateway reservations: %w", err)
	}
	logStartupDuration("release abandoned reservations", stageStarted, false)
	stageStarted = time.Now()
	if waitForQuotaRefresh := refreshAccountQuotasOnStartup(runtimeCtx, control); waitForQuotaRefresh != nil {
		closers = append(closers, waitForQuotaRefresh)
	}
	if waitForAccountRuntimeMaintenance := startAccountRuntimeMaintenance(runtimeCtx, control); waitForAccountRuntimeMaintenance != nil {
		closers = append(closers, waitForAccountRuntimeMaintenance)
	}
	if waitForMonitorScheduler := startMonitorScheduler(runtimeCtx, control, gatewayService); waitForMonitorScheduler != nil {
		closers = append(closers, waitForMonitorScheduler)
	}
	if waitForGatewayAuditRetention := startGatewayAuditRetentionScheduler(runtimeCtx, control, repo, cfg.GatewayAudit); waitForGatewayAuditRetention != nil {
		closers = append(closers, waitForGatewayAuditRetention)
	}
	if waitForAndroidBackup := startAndroidBackupScheduler(runtimeCtx, control, personalPaySDK, backupOptionsForConfig(cfg, b.options.ConfigPath)); waitForAndroidBackup != nil {
		closers = append(closers, waitForAndroidBackup)
	}
	logStartupDuration("start background workers", stageStarted, false)
	stageStarted = time.Now()
	systemSettings, err := control.GetStartupSystemSettings()
	if err != nil {
		return runtimeState{}, fmt.Errorf("load system settings: %w", err)
	}
	startupAuditLimit := systemSettings.Retention.AuditLimit
	if systemSettings.UpdatedAt.IsZero() && cfg.AuditLimit > 0 {
		startupAuditLimit = cfg.AuditLimit
	}
	startupGatewayAuditErrors := systemSettings.Retention.GatewayAuditErrors
	startupGatewayAuditRetentionDays := systemSettings.Retention.GatewayAuditRetentionDays
	if systemSettings.UpdatedAt.IsZero() {
		startupGatewayAuditErrors = cfg.GatewayAuditErrors
		startupGatewayAuditRetentionDays = cfg.GatewayAuditRetentionDays
	}
	logStartupDuration("load startup settings", stageStarted, false)
	// closeRuntime runs closers in reverse order; keep cancellation last so
	// runtime background work stops before repository handles are closed.
	closers = append(closers, func() error {
		cancelRuntime()
		return nil
	})

	startup := StartupInfo{
		AdminAccount:                  adminAccount,
		AdminSeeded:                   adminSeeded,
		ProtocolClient:                seededClient,
		ProtocolClientSeeded:          clientSeeded,
		AuditLimit:                    startupAuditLimit,
		GatewayAuditErrors:            startupGatewayAuditErrors,
		GatewayAuditRetentionDays:     startupGatewayAuditRetentionDays,
		AbandonedReservationsReleased: abandonedReservations.Count,
		AbandonedReservationsNanoUSD:  abandonedReservations.AmountNanoUSD,
	}
	return runtimeState{
		Config:  cfg,
		Control: control,
		Gateway: gatewayService,
		Web:     webServer,
		Startup: startup,
		Handler: limitInFlight(webServer.Handler(), cfg.MaxInFlight),
		Closers: closers,
	}, nil
}

func (b *Builder) loadConfig() (config.Config, error) {
	if b.cfg != nil {
		cfg := *b.cfg
		if err := cfg.Normalize(); err != nil {
			return config.Config{}, err
		}
		return cfg, nil
	}
	loader := b.configLoader
	if loader == nil {
		loader = config.LoadWithOptions
	}
	return loader(config.Options{Path: b.options.ConfigPath, Host: b.options.Host, Port: b.options.Port})
}

func (b *Builder) buildRepository(cfg config.Config) (storage.Repository, func() error, error) {
	if b.repositoryFactory != nil {
		repo, closeFn, err := b.repositoryFactory(cfg)
		if err != nil {
			return nil, nil, err
		}
		if repo == nil {
			return nil, nil, errors.New("repository factory returned nil")
		}
		return repo, closeFn, nil
	}
	switch cfg.DatabaseBackend {
	case "postgres":
		baseRepo, err := storage.NewPostgresRepository(cfg.PostgresDSN, cfg.MasterKey)
		if err != nil {
			return nil, nil, fmt.Errorf("open postgres repository: %w", err)
		}
		cachedRepo := storage.NewCachedRepository(baseRepo)
		asyncRepo := storage.NewAsyncAuditRepository(cachedRepo, 65536)
		closeFn := func() error {
			return errors.Join(asyncRepo.Close(), baseRepo.Close())
		}
		return asyncRepo, closeFn, nil
	default:
		baseRepo, err := storage.NewSQLiteRepository(cfg.StatePath, cfg.MasterKey)
		if err != nil {
			return nil, nil, fmt.Errorf("open repository: %w", err)
		}
		cachedRepo := storage.NewCachedRepository(baseRepo)
		asyncRepo := storage.NewAsyncAuditRepository(cachedRepo, 65536)
		closeFn := func() error {
			return errors.Join(asyncRepo.Close(), baseRepo.Close())
		}
		return asyncRepo, closeFn, nil
	}
}

func (s *Service) installRuntime(runtime runtimeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Config = runtime.Config
	s.Control = runtime.Control
	s.Gateway = runtime.Gateway
	s.Web = runtime.Web
	s.Startup = runtime.Startup
	s.handler = runtime.Handler
	s.closers = runtime.Closers
	s.restoring = false
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isReadinessRequest(r) {
		s.handleReadiness(w)
		return
	}
	restoreRequest := isRestoreRequest(r)
	s.mu.RLock()
	handler := s.handler
	restoring := s.restoring
	trackInFlight := handler != nil && !restoring && !restoreRequest
	if trackInFlight {
		s.inFlight.Add(1)
	}
	s.mu.RUnlock()
	if handler == nil || (restoring && !restoreRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"restoring","message":"service runtime is reloading"}}` + "\n"))
		return
	}
	if trackInFlight {
		defer s.inFlight.Done()
	}
	handler.ServeHTTP(w, r)
}

func (s *Service) handleReadiness(w http.ResponseWriter) {
	s.mu.RLock()
	ready := s.handler != nil && !s.restoring
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_ready"}` + "\n"))
		return
	}
	_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
}

func (s *Service) RestoreBackup(backupPath string, opts backup.Options, preRestoreDir string) (string, error) {
	if s == nil {
		return "", errors.New("app service is not built")
	}
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()

	backupOpts := opts
	if backupOpts.DatabaseBackend == "" {
		backupOpts.DatabaseBackend = s.Config.DatabaseBackend
	}
	if backupOpts.PostgresDSN == "" {
		backupOpts.PostgresDSN = s.Config.PostgresDSN
	}
	restoreOpts := backupOpts
	restoreOpts.ConfigPath = ""
	restoreOpts.TargetMasterKey = s.Config.MasterKey
	preRestoreOpts := backupOpts
	preRestoreOpts.DataSets = nil
	preRestoreOpts.SourceMasterKey = s.Config.MasterKey
	preRestoreOpts.TargetMasterKey = s.Config.MasterKey
	preRestorePath := backup.PreRestoreBackupPath(preRestoreDir)
	if _, err := backup.Create(preRestorePath, preRestoreOpts); err != nil {
		return "", fmt.Errorf("create pre-restore backup: %w", err)
	}

	oldClosers := s.beginRuntimeReload()
	if err := closeRuntime(oldClosers); err != nil {
		_ = s.rebuildRuntime()
		return preRestorePath, fmt.Errorf("close current runtime: %w", err)
	}
	if err := backup.RestoreOnly(backupPath, restoreOpts); err != nil {
		if rollbackErr := s.restoreAndRebuild(preRestorePath, preRestoreOpts); rollbackErr != nil {
			return preRestorePath, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
		}
		return preRestorePath, err
	}
	if err := s.rebuildRuntime(); err != nil {
		if rollbackErr := s.restoreAndRebuild(preRestorePath, preRestoreOpts); rollbackErr != nil {
			return preRestorePath, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
		}
		return preRestorePath, err
	}
	return preRestorePath, nil
}

func (s *Service) restoreAndRebuild(backupPath string, opts backup.Options) error {
	if err := backup.RestoreOnly(backupPath, opts); err != nil {
		return err
	}
	return s.rebuildRuntime()
}

func (s *Service) beginRuntimeReload() []func() error {
	s.mu.Lock()
	oldClosers := s.closers
	s.closers = nil
	s.restoring = true
	s.handler = nil
	s.mu.Unlock()
	s.inFlight.Wait()
	return oldClosers
}

func (s *Service) rebuildRuntime() error {
	if s == nil || s.builder == nil {
		return errors.New("app service builder is not available")
	}
	cfg, err := s.builder.loadConfig()
	if err != nil {
		return err
	}
	runtime, err := s.builder.buildRuntime(cfg, s)
	if err != nil {
		return err
	}
	s.installRuntime(runtime)
	return nil
}

func closeRuntime(closers []func() error) error {
	var closeErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if closers[i] != nil {
			closeErr = errors.Join(closeErr, closers[i]())
		}
	}
	return closeErr
}

func (s *Service) ListenAndServe() error {
	if s == nil || s.HTTPServer == nil {
		return errors.New("app service is not built")
	}
	if err := s.HTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancelRequestContexts()
	var shutdownErr error
	if s.HTTPServer != nil {
		shutdownErr = errors.Join(shutdownErr, s.HTTPServer.Shutdown(ctx))
		s.HTTPServer = nil
	}
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.handler = nil
	s.restoring = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		shutdownErr = errors.Join(shutdownErr, ctx.Err())
	}
	shutdownErr = errors.Join(shutdownErr, closeRuntime(closers))
	return shutdownErr
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.cancelRequestContexts()
	var closeErr error
	if s.HTTPServer != nil {
		closeErr = errors.Join(closeErr, s.HTTPServer.Close())
		s.HTTPServer = nil
	}
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.handler = nil
	s.restoring = true
	s.mu.Unlock()
	s.inFlight.Wait()
	closeErr = errors.Join(closeErr, closeRuntime(closers))
	return closeErr
}

func (s *Service) cancelRequestContexts() {
	if s == nil || s.cancelRequests == nil {
		return
	}
	s.cancelOnce.Do(s.cancelRequests)
}

func isRestoreRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && r.URL != nil && r.URL.Path == "/admin/backup/restore"
}

func isReadinessRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodGet && r.URL != nil && r.URL.Path == "/readyz"
}

func (s *Service) LogStartup(logf Logger) {
	if s == nil || logf == nil {
		return
	}
	s.mu.RLock()
	cfg := s.Config
	info := s.Startup
	s.mu.RUnlock()
	logf("AI Gateway listening on http://%s", cfg.Address)
	logf("Admin control plane username: %s", info.AdminAccount.Username)
	if info.AdminSeeded {
		logf("Initial admin user created")
	}
	logf("Audit retention limit: %d", info.AuditLimit)
	if info.ProtocolClientSeeded {
		if cfg.APIKey == "" {
			logf("Initial protocol client key: %s", info.ProtocolClient.APIKey)
		} else {
			logf("Initial protocol client key: %s", maskSecret(info.ProtocolClient.APIKey))
		}
	}
	if cfg.DatabaseBackend == "postgres" {
		logf("Storage backend: PostgreSQL")
	} else {
		logf("State database: %s", cfg.StatePath)
	}
	switch {
	case cfg.GatewayAudit:
		logf("Gateway request audit: full")
	case info.GatewayAuditErrors:
		logf("Gateway request audit: errors only, retention=%dd", info.GatewayAuditRetentionDays)
	default:
		logf("Gateway request audit: false")
	}
	logf("Max in-flight requests: %d", cfg.MaxInFlight)
	logf("Upstream transport: max_idle=%d max_idle_per_host=%d max_conns_per_host=%d", cfg.UpstreamMaxIdleConns, cfg.UpstreamMaxIdleConnsPerHost, cfg.UpstreamMaxConnsPerHost)
	logf("State secret encryption: %t", cfg.MasterKey != "")
	logf("Trusted proxy CIDRs: %s", strings.Join(cfg.TrustedProxyCIDRs, ","))
	if info.AbandonedReservationsReleased > 0 {
		logf("Released abandoned reserved gateway charges: count=%d amount=%s", info.AbandonedReservationsReleased, core.FormatNanoUSD(info.AbandonedReservationsNanoUSD))
	}
	if !config.HostIsLoopback(cfg.Host) {
		logf("WARNING: server is bound to non-loopback host %s; put it behind trusted network controls", cfg.Host)
	}
	if cfg.MasterKey == "" {
		logf("WARNING: master_key is not configured; persisted credentials, client keys, proxy URLs, image task prompts/results, and system secrets are stored without encryption")
	}
	if info.AdminSeeded {
		logf("WARNING: initial admin credentials were created; change the admin password before using the control plane")
	}
}

func personalPayExpireAfter(settings core.PersonalPaySettings) time.Duration {
	if settings.ExpireAfterSec <= 0 {
		return time.Duration(core.DefaultPersonalPayExpireAfterSec) * time.Second
	}
	return time.Duration(settings.ExpireAfterSec) * time.Second
}

func personalPayAndroidToken(settings core.SystemSettings) string {
	settings = core.NormalizeSystemSettings(settings)
	androidToken := strings.TrimSpace(settings.Payment.PersonalPay.AndroidToken)
	if personalPayAndroidAvailable(settings) {
		return androidToken
	}
	return "personalpay-disabled"
}

func personalPayAndroidAvailable(settings core.SystemSettings) bool {
	settings = core.NormalizeSystemSettings(settings)
	return settings.Payment.PersonalPay.Enabled && strings.TrimSpace(settings.Payment.PersonalPay.AndroidToken) != ""
}

func configurePersonalPaySDK(sdk *personalpay.SDK, settings core.SystemSettings) {
	if sdk == nil {
		return
	}
	settings = core.NormalizeSystemSettings(settings)
	sdk.SetDefaultExpireAfter(personalPayExpireAfter(settings.Payment.PersonalPay))
	sdk.SetAndroidAuth(personalPayAndroidToken(settings), nil)
}

func applyPersonalPayNotification(ctx context.Context, control *controlplane.Service, notification personalpay.OrderNotification) {
	if control == nil {
		return
	}
	event := payments.PersonalPayNotificationEvent(notification)
	if event.OutTradeNo == "" {
		log.Printf("personalpay notification missing order id")
		return
	}
	for attempt := 0; attempt < 6; attempt++ {
		order, credited, err := control.ApplyPaymentEvent(event)
		if err == nil {
			if credited {
				log.Printf("personalpay payment credited: order=%s user=%s amount=%s", order.OutTradeNo, order.UserID, core.FormatNanoUSD(order.AmountNanoUSD))
			}
			return
		}
		if !errors.Is(err, storage.ErrNotFound) {
			log.Printf("personalpay notification apply failed: order=%s status=%s err=%v", event.OutTradeNo, event.ProviderStatus, err)
			return
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
	log.Printf("personalpay notification apply failed: order=%s status=%s err=%v", event.OutTradeNo, event.ProviderStatus, storage.ErrNotFound)
}

const (
	androidBackupSchedulerPollInterval = time.Minute
	gatewayAuditRetentionPollInterval  = time.Hour
	startupBackgroundWorkerDelay       = time.Minute
	startupQuotaRefreshDelay           = startupBackgroundWorkerDelay
	startupStageLogThreshold           = 250 * time.Millisecond
)

func logStartupDuration(stage string, started time.Time, always bool) {
	elapsed := time.Since(started)
	if !always && elapsed < startupStageLogThreshold {
		return
	}
	log.Printf("startup %s took %s", stage, elapsed.Round(time.Millisecond))
}

func backupOptionsForConfig(cfg config.Config, configPath string) backup.Options {
	return backup.Options{
		ConfigPath:      AbsoluteConfigPath(configPath),
		StatePath:       cfg.StatePath,
		DatabaseBackend: cfg.DatabaseBackend,
		PostgresDSN:     cfg.PostgresDSN,
		TargetMasterKey: cfg.MasterKey,
	}
}

func startGatewayAuditRetentionScheduler(ctx context.Context, control *controlplane.Service, repo storage.Repository, gatewayAuditFull bool) func() error {
	if control == nil || repo == nil || gatewayAuditFull {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if !sleepContext(ctx, gatewayAuditRetentionPollInterval) {
				return
			}
			settings, err := control.GetSystemSettings()
			if err != nil {
				log.Printf("gateway audit retention settings load failed: %v", err)
				continue
			}
			settings = core.NormalizeSystemSettings(settings)
			retentionDays := 0
			if settings.Retention.GatewayAuditErrors {
				retentionDays = settings.Retention.GatewayAuditRetentionDays
			}
			if err := repo.ConfigureGatewayAuditRetention(retentionDays); err != nil {
				log.Printf("gateway audit retention trim failed: %v", err)
			}
		}
	}()
	return func() error {
		<-done
		return nil
	}
}

func startAndroidBackupScheduler(ctx context.Context, control *controlplane.Service, sdk *personalpay.SDK, opts backup.Options) func() error {
	if control == nil || sdk == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastRunKey := ""
		pendingRunAt := time.Time{}
		noDeviceLoggedRunKey := ""
		if !sleepContext(ctx, startupBackgroundWorkerDelay) {
			return
		}
		for {
			settings, err := control.GetStartupSystemSettings()
			if err != nil {
				log.Printf("android backup settings load failed: %v", err)
				if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
					return
				}
				continue
			}
			settings = core.NormalizeSystemSettings(settings)
			if !personalPayAndroidAvailable(settings) || !settings.Backup.AndroidAutoEnabled {
				pendingRunAt = time.Time{}
				noDeviceLoggedRunKey = ""
				if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
					return
				}
				continue
			}

			now := time.Now()
			runAt := pendingRunAt
			if !pendingAndroidBackupRunCurrent(now, runAt, settings.Backup.AndroidTimeOfDay) {
				runAt = nextDailyBackupTime(now, settings.Backup.AndroidTimeOfDay)
				pendingRunAt = time.Time{}
			}
			wait := time.Until(runAt)
			if wait > androidBackupSchedulerPollInterval {
				if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
					return
				}
				continue
			}
			if wait > 0 && !sleepContext(ctx, wait) {
				return
			}

			runKey := runAt.Format("2006-01-02 15:04")
			if runKey != "" && runKey == lastRunKey {
				pendingRunAt = time.Time{}
				if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
					return
				}
				continue
			}
			pendingRunAt = runAt
			settings, err = control.GetStartupSystemSettings()
			if err != nil {
				log.Printf("android backup settings reload failed: %v", err)
				if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
					return
				}
				continue
			}
			settings = core.NormalizeSystemSettings(settings)
			if !personalPayAndroidAvailable(settings) || !settings.Backup.AndroidAutoEnabled {
				pendingRunAt = time.Time{}
				noDeviceLoggedRunKey = ""
				continue
			}
			if err := runAndroidBackup(ctx, sdk, opts, settings.Backup); err != nil {
				if errors.Is(err, personalpay.ErrNoDevice) {
					if noDeviceLoggedRunKey != runKey {
						log.Printf("android backup skipped: no online Android device; will retry while today's schedule is pending")
						noDeviceLoggedRunKey = runKey
					}
					if !sleepContext(ctx, androidBackupSchedulerPollInterval) {
						return
					}
					continue
				} else {
					log.Printf("android backup failed: %v", err)
				}
			}
			pendingRunAt = time.Time{}
			noDeviceLoggedRunKey = ""
			lastRunKey = runKey
		}
	}()
	return func() error {
		<-done
		return nil
	}
}

func nextDailyBackupTime(now time.Time, timeOfDay string) time.Time {
	next := scheduledDailyBackupTime(now, timeOfDay)
	if next.Before(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func pendingAndroidBackupRunCurrent(now, pendingRunAt time.Time, timeOfDay string) bool {
	if pendingRunAt.IsZero() {
		return false
	}
	scheduled := scheduledDailyBackupTime(now, timeOfDay)
	return !now.Before(scheduled) && pendingRunAt.Equal(scheduled)
}

func scheduledDailyBackupTime(now time.Time, timeOfDay string) time.Time {
	timeOfDay = core.NormalizeSystemBackupSettings(core.SystemBackupSettings{
		AndroidTimeOfDay: timeOfDay,
		AndroidDataSets:  []string{backup.DataSetBilling},
	}).AndroidTimeOfDay
	hour := int(timeOfDay[0]-'0')*10 + int(timeOfDay[1]-'0')
	minute := int(timeOfDay[3]-'0')*10 + int(timeOfDay[4]-'0')
	return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
}

func runAndroidBackup(ctx context.Context, sdk *personalpay.SDK, opts backup.Options, settings core.SystemBackupSettings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !androidBackupHasOnlineDevice(ctx, sdk) {
		return personalpay.ErrNoDevice
	}
	settings = core.NormalizeSystemBackupSettings(settings)
	backupOpts := opts
	backupOpts.DataSets = append([]string(nil), settings.AndroidDataSets...)

	var buf bytes.Buffer
	manifest, err := backup.Write(&buf, backupOpts)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := "ai-gateway-" + manifest.CreatedAt.Format("20060102-150405") + ".agbak"
	result, err := sdk.StoreBackup(ctx, personalpay.StoreBackupRequest{
		Name:        name,
		ContentType: "application/gzip",
		Data:        buf.Bytes(),
		Retain:      5,
		Metadata: map[string]string{
			"source":        "ai-gateway",
			"dataSets":      strings.Join(settings.AndroidDataSets, ","),
			"scheduledTime": settings.AndroidTimeOfDay,
		},
	})
	if err != nil {
		return fmt.Errorf("send backup to Android: %w", err)
	}
	log.Printf("android backup queued: device=%s backup=%s size=%d data_sets=%s", result.DeviceID, result.Name, result.SizeBytes, strings.Join(settings.AndroidDataSets, ","))
	return nil
}

func androidBackupHasOnlineDevice(ctx context.Context, sdk *personalpay.SDK) bool {
	if sdk == nil {
		return false
	}
	for _, device := range sdk.ListDevices(ctx) {
		if device.Online {
			return true
		}
	}
	return false
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func AbsoluteConfigPath(path string) string {
	if path == "" {
		path = config.DefaultPath
	}
	if absPath, err := filepath.Abs(path); err == nil {
		return absPath
	}
	return path
}

func refreshAccountQuotasOnStartup(ctx context.Context, control *controlplane.Service) func() error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if control == nil {
			return
		}
		if !sleepContext(ctx, startupQuotaRefreshDelay) {
			return
		}
		var refreshed, skippedDisabled, skippedUnsupported, skippedFree, failed int
		for _, account := range control.ListAccounts() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if core.AccountControlDisabled(account) {
				skippedDisabled++
				continue
			}
			if !control.SupportsQuotaRefresh(account) {
				skippedUnsupported++
				continue
			}
			if providers.IsOpenAIChatGPTFreeAccount(account) {
				skippedFree++
				continue
			}
			refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, _, err := control.RefreshAccountQuota(refreshCtx, account.ID)
			cancel()
			if err != nil {
				failed++
				log.Printf("startup quota refresh for account %s failed: %v", account.ID, err)
				continue
			}
			refreshed++
		}
		if refreshed > 0 || failed > 0 {
			log.Printf(
				"startup quota refresh finished: refreshed=%d failed=%d skipped_disabled=%d skipped_unsupported=%d skipped_free=%d",
				refreshed,
				failed,
				skippedDisabled,
				skippedUnsupported,
				skippedFree,
			)
		}
	}()
	return func() error {
		<-done
		return nil
	}
}

func startAccountRuntimeMaintenance(ctx context.Context, control *controlplane.Service) func() error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		run := func(now time.Time) {
			report, err := control.ReconcileAccountRuntimeState(ctx, now.UTC())
			if err != nil {
				log.Printf("account runtime maintenance failed: %v", err)
				return
			}
			if report.Updated > 0 || report.QuotaCooldownCleared > 0 {
				log.Printf("account runtime maintenance: updated=%d reactivated=%d quota_cooldown_cleared=%d", report.Updated, report.Reactivated, report.QuotaCooldownCleared)
			}
		}
		if !sleepContext(ctx, startupBackgroundWorkerDelay) {
			return
		}
		run(time.Now())
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				run(now)
			}
		}
	}()
	return func() error {
		<-done
		return nil
	}
}

func maskSecret(value string) string {
	if len(value) <= 8 {
		if value == "" {
			return "empty"
		}
		return "****"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func limitInFlight(next http.Handler, max int) http.Handler {
	if next == nil || max <= 0 {
		return next
	}
	totalSem := make(chan struct{}, max)
	protocolSem := make(chan struct{}, protocolInFlightLimit(max))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL != nil && (r.URL.Path == "/healthz" || r.URL.Path == "/health" || r.URL.Path == "/readyz" || r.URL.Path == "/personalpay/android/ws") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL != nil && isProtocolInFlightPath(r.URL.Path) {
			if !tryAcquireInFlight(protocolSem) {
				writeInFlightOverloaded(w)
				return
			}
			defer releaseInFlight(protocolSem)
		}
		if !tryAcquireInFlight(totalSem) {
			writeInFlightOverloaded(w)
			return
		}
		defer releaseInFlight(totalSem)
		next.ServeHTTP(w, r)
	})
}

func protocolInFlightLimit(max int) int {
	if max <= 1 {
		return max
	}
	reserved := max / 16
	if reserved < 1 {
		reserved = 1
	}
	if reserved > 64 {
		reserved = 64
	}
	limit := max - reserved
	if limit < 1 {
		return 1
	}
	return limit
}

func isProtocolInFlightPath(path string) bool {
	path = strings.TrimSpace(path)
	return path == "/mcp" ||
		strings.HasPrefix(path, "/v1/") ||
		strings.HasPrefix(path, "/ag/v1/") ||
		strings.HasPrefix(path, "/api/v1/") ||
		strings.HasPrefix(path, "/anthropic/v1/")
}

func tryAcquireInFlight(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseInFlight(sem chan struct{}) {
	<-sem
}

func writeInFlightOverloaded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":{"type":"overloaded","message":"too many in-flight requests"}}` + "\n"))
}
