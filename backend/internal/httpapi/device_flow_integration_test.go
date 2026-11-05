//go:build integration && browser

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/generation"
	db "invoice.local/system/backend/internal/platform/postgres/db"
	"invoice.local/system/backend/internal/preview"
	"invoice.local/system/backend/internal/render"
	"invoice.local/system/backend/internal/testdb"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDeviceFlow(t *testing.T) {
	pool := testdb.Open(t)
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(filepath.Dir(os.Getenv("INVOICE_TEST_GUARD_FILE")), "device-flow")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	keyFile := filepath.Join(dir, "keys.json")
	if e := account.GenerateKeyringFile(keyFile); e != nil {
		t.Fatal(e)
	}
	keys, e := account.LoadKeyring(keyFile)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	service, e := account.NewService(pool, keys, origin)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	// Same protected initializer as the CLI. Every browser credential and customer
	// below is then created through the actual UI and public handlers.
	link, e := service.InitializeAdministrator(ctx, "device_admin", false)
	if e != nil {
		t.Fatal(e)
	}
	docs, e := document.NewService(pool, keys, service)
	if e != nil {
		t.Fatal(e)
	}
	assets, e := render.Load()
	if e != nil {
		t.Fatal(e)
	}
	previews, e := preview.NewService(pool, service, assets)
	if e != nil {
		t.Fatal(e)
	}
	generations, e := generation.NewService(pool, service, assets)
	if e != nil {
		t.Fatal(e)
	}
	// The API process drains the preview and formal queues in the background;
	// this test wires the same workers so the browser path exercises the real
	// render, settlement and delivery confirmation, not a stub.
	go func() {
		for {
			if _, e := previews.RunOnce(ctx); e != nil {
				t.Logf("preview worker: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	go func() {
		for {
			if _, e := generations.RunOnce(ctx); e != nil {
				t.Logf("formal worker: %v", e)
			}
			if e := generations.RunFinalize(ctx); e != nil {
				t.Logf("finalizer: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	server.Config.Handler = NewHandler(Options{Business: NewAdminAPI(service, credit.New(pool, keys), docs, previews, generations, keys, origin), WebDir: filepath.Join(root, "frontend/dist")})
	server.StartTLS()
	defer server.Close()
	fixture := filepath.Join(dir, "browser.json")
	data, _ := json.Marshal(map[string]string{"origin": origin, "bootstrapLink": link.Link, "admin": "device_admin", "customer": "device_customer"})
	if e := os.WriteFile(fixture, data, 0600); e != nil {
		t.Fatal(e)
	}
	node := os.Getenv("NODE_BIN")
	if node == "" {
		t.Fatal("NODE_BIN required")
	}
	cmd := exec.CommandContext(ctx, node, filepath.Join(root, "frontend/tests/device/browser-account.mjs"), fixture)
	cmd.Dir = root
	output, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("device browser flow failed: %s", output)
	}
	t.Log(string(output))
	var result struct {
		IdleProbe time.Time `json:"idle_probe_time"`
	}
	if json.Unmarshal(output, &result) != nil || result.IdleProbe.IsZero() {
		t.Fatal("idle probe result missing")
	}
	var available, frozen, bindings, passwords, pending, completed, activeSessions int64
	var state, onboarding string
	e = pool.QueryRow(ctx, `SELECT c.available,c.frozen,u.device_state,u.onboarding_state,
 (SELECT count(*) FROM device_bindings b WHERE b.user_id=u.id AND b.status='ACTIVE'),
 (SELECT count(*) FROM password_credentials p WHERE p.user_id=u.id),
 (SELECT count(*) FROM binding_transfers b WHERE b.user_id=u.id AND b.status='PENDING'),
 (SELECT count(*) FROM binding_transfers b WHERE b.user_id=u.id AND b.purpose='INITIAL' AND b.status='CONSUMED'),
 (SELECT count(*) FROM device_sessions s WHERE s.user_id=u.id AND s.status='ACTIVE')
 FROM users u JOIN credit_accounts c ON c.user_id=u.id WHERE u.username='device_customer'`).Scan(&available, &frozen, &state, &onboarding, &bindings, &passwords, &pending, &completed, &activeSessions)
	if e != nil {
		t.Fatal(e)
	}
	if available != 0 || frozen != 0 || state != "ACTIVE" || onboarding != "READY" || bindings != 1 || passwords != 1 || pending != 0 || completed != 1 || activeSessions != 1 {
		t.Fatal("unexpected persisted device/account/wallet state")
	}
	var tokenConsumed bool
	if e := pool.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM password_tokens WHERE user_id=(SELECT id FROM users WHERE username='device_customer') AND purpose='INITIAL_SETUP'`).Scan(&tokenConsumed); e != nil || !tokenConsumed {
		t.Fatal("initial setup token was not consumed")
	}
	t.Log("PASS committed READY USER, one ACTIVE binding/session, consumed initial token/transfer, one password, unchanged zero wallet")
	// The paid chain must be fully persisted: one grant in, one version out,
	// both formal objects published, receipt recorded, wallet back to zero.
	var versions, receipts, published, grants, reserves, consumes, releases, tickets int64
	e = pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM document_versions v JOIN users u ON u.id=v.owner_id WHERE u.username='device_customer'),
 (SELECT count(*) FROM delivery_receipts r JOIN document_versions v ON v.id=r.version_id JOIN users u ON u.id=v.owner_id WHERE u.username='device_customer'),
 (SELECT count(*) FROM artifact_objects o JOIN users u ON u.id=o.owner_id WHERE u.username='device_customer' AND o.state='PUBLISHED' AND o.type IN ('CLEAN_PDF','CLEAN_PNG')),
 (SELECT count(*) FROM credit_entries l JOIN users u ON u.id=l.account_id WHERE u.username='device_customer' AND l.kind='GRANT'),
 (SELECT count(*) FROM credit_entries l JOIN users u ON u.id=l.account_id WHERE u.username='device_customer' AND l.kind='RESERVE'),
 (SELECT count(*) FROM credit_entries l JOIN users u ON u.id=l.account_id WHERE u.username='device_customer' AND l.kind='CONSUME'),
 (SELECT count(*) FROM credit_entries l JOIN users u ON u.id=l.account_id WHERE u.username='device_customer' AND l.kind='RELEASE'),
 (SELECT count(*) FROM capacity_tickets t JOIN users u ON u.id=t.owner_id WHERE u.username='device_customer' AND t.status='COMPLETED')
 `).Scan(&versions, &receipts, &published, &grants, &reserves, &consumes, &releases, &tickets)
	if e != nil {
		t.Fatal(e)
	}
	if versions != 1 || receipts != 1 || published != 2 || grants != 1 || reserves != 1 || consumes != 1 || releases != 0 || tickets != 1 {
		t.Fatalf("paid chain not persisted exactly once: versions=%d receipts=%d published=%d grants=%d reserves=%d consumes=%d releases=%d tickets=%d",
			versions, receipts, published, grants, reserves, consumes, releases, tickets)
	}
	var orderBilling string
	if e := pool.QueryRow(ctx, `SELECT o.billing_state FROM generation_orders o JOIN users u ON u.id=o.owner_id WHERE u.username='device_customer'`).Scan(&orderBilling); e != nil || orderBilling != "COMPLETED" {
		t.Fatalf("order billing state = %q (%v), want COMPLETED", orderBilling, e)
	}
	t.Log("PASS paid chain: 1 grant -> 1 reserved order -> 1 completed version, 2 published formats, delivery receipt, wallet 0/0")
	var idle time.Time
	if e := pool.QueryRow(ctx, `SELECT last_active_at FROM device_sessions WHERE user_id=(SELECT id FROM users WHERE username='device_customer') AND status='ACTIVE'`).Scan(&idle); e != nil || !idle.Before(result.IdleProbe.Add(-time.Millisecond)) {
		t.Fatal("unsigned context unexpectedly extended USER idle time")
	}
	// A foreign subject must be filtered before FOR UPDATE, even when its row is
	// held elsewhere. This was a cross-context lock/DoS counterexample.
	var oldChallenge, sessionID pgtype.UUID
	if e := pool.QueryRow(ctx, "SELECT id FROM device_challenges WHERE purpose='INITIAL_SETUP' LIMIT 1").Scan(&oldChallenge); e != nil {
		t.Fatal(e)
	}
	if e := pool.QueryRow(ctx, `SELECT id FROM device_sessions WHERE user_id=(SELECT id FROM users WHERE username='device_customer') AND status='ACTIVE'`).Scan(&sessionID); e != nil {
		t.Fatal(e)
	}
	holder, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelCleanup()
		_ = holder.Rollback(cleanup)
	}()
	if _, e := holder.Exec(ctx, "SELECT id FROM device_challenges WHERE id=$1 FOR UPDATE", oldChallenge); e != nil {
		t.Fatal(e)
	}
	probe, cancelProbe := context.WithTimeout(ctx, time.Second)
	defer cancelProbe()
	_, e = db.New(pool).LockDeviceChallenge(probe, db.LockDeviceChallengeParams{ID: oldChallenge, SubjectID: sessionID, SubjectKind: "SESSION"})
	if !errors.Is(e, pgx.ErrNoRows) {
		t.Fatal("foreign subject took a row lock before ownership filtering")
	}
	t.Log("PASS unsigned context does not touch idle; foreign challenge filtered before row lock")
}
