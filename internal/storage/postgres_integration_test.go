package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const postgresIntegrationDSNEnv = "AG_POSTGRES_TEST_DSN"

func postgresIntegrationDSN(t *testing.T) string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(postgresIntegrationDSNEnv))
	if raw == "" {
		t.Skipf("set %s to run PostgreSQL integration tests", postgresIntegrationDSNEnv)
	}
	adminDB, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatalf("open postgres admin connection: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("ping postgres admin connection: %v", err)
	}

	schema := fmt.Sprintf("agw_test_%d", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + quotePostgresIdentifier(schema)); err != nil {
		t.Fatalf("create postgres test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DROP SCHEMA IF EXISTS ` + quotePostgresIdentifier(schema) + ` CASCADE`)
	})
	return postgresDSNWithSearchPath(raw, schema)
}

func postgresDSNWithSearchPath(raw, schema string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	return strings.TrimRight(raw, " \t\r\n") + " search_path=" + schema
}

func quotePostgresIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func TestPostgresIntegrationBillingConcurrencyAcrossRepositories(t *testing.T) {
	dsn := postgresIntegrationDSN(t)
	repoA, err := NewPostgresRepository(dsn, "")
	if err != nil {
		t.Fatalf("NewPostgresRepository A returned error: %v", err)
	}
	t.Cleanup(func() { _ = repoA.Close() })
	repoB, err := NewPostgresRepository(dsn, "")
	if err != nil {
		t.Fatalf("NewPostgresRepository B returned error: %v", err)
	}
	t.Cleanup(func() { _ = repoB.Close() })

	if err := repoA.ConfigureUsageLogRetention(365); err != nil {
		t.Fatal(err)
	}
	if err := repoA.UpsertUser(core.User{ID: "user_pg_concurrent", Username: "pg-concurrent", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repoA.UpsertClient(core.APIClient{ID: "client_pg_concurrent", Name: "PG Concurrent", APIKey: "gw_pg_concurrent", OwnerUserID: "user_pg_concurrent", Enabled: true, SpendLimitNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}

	var successes atomic.Int64
	var expectedRejects atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := repoA
			if i%2 == 1 {
				repo = repoB
			}
			_, err := repo.ReserveBilling(core.BillingReservationInput{
				RequestID:       fmt.Sprintf("req_pg_concurrent_%02d", i),
				ClientID:        "client_pg_concurrent",
				UserID:          "user_pg_concurrent",
				Model:           "gpt-pg",
				ReservedNanoUSD: 100,
				Fingerprint:     fmt.Sprintf("fp_%02d", i),
			})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrClientSpendLimitExceeded):
				expectedRejects.Add(1)
			default:
				t.Errorf("ReserveBilling returned unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if successes.Load() != 10 || expectedRejects.Load() != 6 {
		t.Fatalf("billing concurrency successes=%d rejects=%d, want 10/6", successes.Load(), expectedRejects.Load())
	}
	user, err := repoA.GetUser("user_pg_concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 0 {
		t.Fatalf("final user balance = %d, want 0", user.BalanceNanoUSD)
	}
	spend, err := repoA.GetClientSpend("client_pg_concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 1000 {
		t.Fatalf("final client spend = %d, want 1000", spend.SpendUsedNanoUSD)
	}
}

func TestPostgresIntegrationPaymentCompletionIsIdempotentAcrossRepositories(t *testing.T) {
	dsn := postgresIntegrationDSN(t)
	repoA, err := NewPostgresRepository(dsn, "")
	if err != nil {
		t.Fatalf("NewPostgresRepository A returned error: %v", err)
	}
	t.Cleanup(func() { _ = repoA.Close() })
	repoB, err := NewPostgresRepository(dsn, "")
	if err != nil {
		t.Fatalf("NewPostgresRepository B returned error: %v", err)
	}
	t.Cleanup(func() { _ = repoB.Close() })

	if err := repoA.UpsertUser(core.User{ID: "user_pg_payment", Username: "pg-payment", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_pg_once",
		OutTradeNo:    "out_pg_once",
		UserID:        "user_pg_payment",
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Currency:      "CNY",
	}
	if err := repoA.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}

	var credited atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := repoA
			if i%2 == 1 {
				repo = repoB
			}
			_, ok, err := repo.CompletePaymentOrder("out_pg_once", "trade_pg_once", core.NanoUSDPerUSD, time.Now().UTC())
			if err != nil {
				t.Errorf("CompletePaymentOrder returned error: %v", err)
				return
			}
			if ok {
				credited.Add(1)
			}
		}()
	}
	wg.Wait()

	if credited.Load() != 1 {
		t.Fatalf("credited completions = %d, want 1", credited.Load())
	}
	user, err := repoA.GetUser("user_pg_payment")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("final user balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD)
	}
}
