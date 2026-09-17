package paddle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	corepostgres "github.com/data-insights-ai/rho-billing/postgres"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestWebhookPostgresCommitsAndDeduplicatesProviderEvent(t *testing.T) {
	store, db := webhookPostgresStore(t)
	if err := store.CreateAccount(t.Context(), "account-webhook-pg", "subject-webhook-pg"); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		return "account-webhook-pg", nil
	}))

	firstBody := webhookBody(webhookEventID, "notification-first", `{"customer_id":"ctm_1"}`)
	first, err := integration.ReceiveWebhook(t.Context(), store.Queue(), verifier, firstBody, signedWebhookHeaders(firstBody), webhookScope())
	if err != nil {
		t.Fatalf("first ReceiveWebhook() error = %v", err)
	}
	if got := webhookInboxCount(t, db, first.Account, first.ID); got != 1 {
		t.Fatalf("inbox rows after committed receive = %d, want 1", got)
	}

	secondBody := webhookBody(webhookEventID, "notification-second", `{"customer_id":"ctm_1"}`)
	second, err := integration.ReceiveWebhook(t.Context(), store.Queue(), verifier, secondBody, signedWebhookHeaders(secondBody), webhookScope())
	if err != nil {
		t.Fatalf("notification replay ReceiveWebhook() error = %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("notification metadata changed event fingerprint: first=%s second=%s", first.Fingerprint(), second.Fingerprint())
	}
	if got := webhookInboxCount(t, db, first.Account, first.ID); got != 1 {
		t.Fatalf("inbox rows after notification replay = %d, want 1", got)
	}
}

func TestWebhookPostgresReceiveFailureCannotReportSuccess(t *testing.T) {
	store, db := webhookPostgresStore(t)
	if err := store.CreateAccount(t.Context(), "account-webhook-failure", "subject-webhook-failure"); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	eventID := "evt_abcdefghijklmnopqrstuvwxyz"
	body := webhookBody(eventID, "notification-failure", `{}`)
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) {
		return "account-webhook-failure", nil
	}))
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION reject_receipt_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt commit failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE CONSTRAINT TRIGGER reject_receipt AFTER INSERT ON billing_inbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_receipt_commit()`); err != nil {
		t.Fatal(err)
	}

	message, err := integration.ReceiveWebhook(t.Context(), store.Queue(), verifier, body, signedWebhookHeaders(body), webhookScope())
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "P0001" || pgErr.Message != "injected receipt commit failure" {
		t.Fatalf("ReceiveWebhook() error = %v, want deferred commit failure", err)
	}
	if message.Account != "" || message.ID != "" || len(message.Payload) != 0 {
		t.Fatalf("ReceiveWebhook() returned success message after receive failure: %+v", message)
	}
	if got := webhookInboxCount(t, db, "account-webhook-failure", eventID); got != 0 {
		t.Fatalf("inbox rows after injected receive failure = %d, want 0", got)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER reject_receipt ON billing_inbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.ReceiveWebhook(t.Context(), store.Queue(), verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatalf("retry after commit failure: %v", err)
	}
	if got := webhookInboxCount(t, db, "account-webhook-failure", eventID); got != 1 {
		t.Fatalf("retried receipt count=%d", got)
	}
}

func webhookPostgresStore(t *testing.T) (*corepostgres.Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BILLING_TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open configured PostgreSQL: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(t.Context()); err != nil {
		_ = db.Close()
		t.Fatalf("configured PostgreSQL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("rho_paddle_webhook_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		_ = db.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `SET search_path TO `+schema); err != nil {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = db.Close()
		t.Fatalf("select isolated schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = db.Close()
	})
	store := corepostgres.NewWithClock(db, func() time.Time { return webhookNow })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store, db
}

func webhookInboxCount(t *testing.T, db *sql.DB, account billing.AccountID, messageID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, account, messageID).Scan(&count); err != nil {
		t.Fatalf("count inbox rows: %v", err)
	}
	return count
}
