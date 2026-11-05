//go:build integration && browser

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/generation"
	"invoice.local/system/backend/internal/preview"
	"invoice.local/system/backend/internal/render"
	"invoice.local/system/backend/internal/testdb"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminFlow(t *testing.T) {
	pool := testdb.Open(t)
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	dir := filepath.Join(filepath.Dir(os.Getenv("INVOICE_TEST_GUARD_FILE")), "admin-flow")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	goBin := os.Getenv("INVOICE_GO_BIN")
	if goBin == "" {
		t.Fatal("runner must select Go")
	}
	cli := filepath.Join(dir, "admin")
	build := exec.CommandContext(ctx, goBin, "build", "-o", cli, "./cmd/admin")
	build.Dir = filepath.Join(root, "backend")
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatalf("CLI build failed: %s", output)
	}
	runCLI := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, cli, args...)
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("protected CLI failed (no secret output expected): %s", output)
		}
	}
	keyFile := filepath.Join(dir, "keyring.json")
	runCLI("keyring-init", "-output", keyFile)
	keys, e := account.LoadKeyring(keyFile)
	if e != nil {
		t.Fatal("private keyring load")
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	dsnFile := filepath.Join(dir, "dsn")
	if e := os.WriteFile(dsnFile, []byte(os.Getenv("INVOICE_TEST_DSN")), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := map[string]any{"environment": "development", "public_origin": origin, "listen_addr": server.Listener.Addr().String(), "database_dsn_file": dsnFile, "keyring_file": keyFile, "trusted_proxy_cidrs": []string{}}
	cfgData, _ := json.Marshal(cfg)
	cfgFile := filepath.Join(dir, "config.json")
	if e := os.WriteFile(cfgFile, cfgData, 0600); e != nil {
		t.Fatal(e)
	}
	linkFile := filepath.Join(dir, "bootstrap.json")
	runCLI("init", "-config", cfgFile, "-username", "flow_admin", "-output", linkFile)
	linkData, e := os.ReadFile(linkFile)
	if e != nil {
		t.Fatal("bootstrap output")
	}
	var link account.BootstrapLink
	if json.Unmarshal(linkData, &link) != nil {
		t.Fatal("invalid bootstrap output")
	}
	info, e := os.Stat(linkFile)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("bootstrap file must be private")
	}
	service, e := account.NewService(pool, keys, origin)
	if e != nil {
		t.Fatal("new account service")
	}
	docs, e := document.NewService(pool, keys, service)
	if e != nil {
		t.Fatal("new document service")
	}
	assets, e := render.Load()
	if e != nil {
		t.Fatal("render assets")
	}
	previews, e := preview.NewService(pool, service, assets)
	if e != nil {
		t.Fatal("new preview service")
	}
	generations, e := generation.NewService(pool, service, assets)
	if e != nil {
		t.Fatal("new generation service")
	}
	handler := NewHandler(Options{Business: NewAdminAPI(service, credit.New(pool, keys), docs, previews, generations, keys, origin), WebDir: filepath.Join(root, "frontend", "dist")})
	var zeroWalletObserved atomic.Bool
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/admin/users" {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code == 201 {
				var result account.CreatedUser
				if json.Unmarshal(recorded.Body.Bytes(), &result) != nil {
					t.Error("invalid account response")
				} else {
					var available, frozen int64
					if e := pool.QueryRow(ctx, "SELECT available,frozen FROM credit_accounts WHERE user_id=$1::uuid", result.User.ID).Scan(&available, &frozen); e != nil || available != 0 || frozen != 0 {
						t.Error("new account did not commit with zero wallet")
					} else {
						zeroWalletObserved.Store(true)
					}
				}
			}
			for k, vs := range recorded.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = io.Copy(w, recorded.Body)
			return
		}
		handler.ServeHTTP(w, r)
	})
	server.StartTLS()
	defer server.Close()
	fixture := filepath.Join(dir, "browser.json")
	data, _ := json.Marshal(map[string]string{"origin": origin, "bootstrapLink": link.Link, "admin": "flow_admin", "customer": "flow_customer"})
	if e := os.WriteFile(fixture, data, 0600); e != nil {
		t.Fatal(e)
	}
	node := os.Getenv("NODE_BIN")
	if node == "" {
		t.Fatal("NODE_BIN is required for real browser test")
	}
	cmd := exec.CommandContext(ctx, node, filepath.Join(root, "frontend", "tests", "admin", "browser-admin.mjs"), fixture)
	cmd.Dir = root
	output, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("admin browser flow failed: %s", output)
	}
	t.Log(string(output))
	if !zeroWalletObserved.Load() {
		t.Fatal("zero wallet not observed in committed database before grant")
	}
	var available, frozen, grants, sessions, bindings, passwords int64
	e = pool.QueryRow(ctx, `SELECT c.available,c.frozen,
 (SELECT count(*) FROM credit_entries l WHERE l.account_id=u.id AND l.kind='GRANT'),
 (SELECT count(*) FROM device_sessions d WHERE d.user_id=u.id),
 (SELECT count(*) FROM device_bindings d WHERE d.user_id=u.id),
 (SELECT count(*) FROM password_credentials p WHERE p.user_id=u.id)
 FROM users u JOIN credit_accounts c ON c.user_id=u.id WHERE u.username='flow_customer'`).Scan(&available, &frozen, &grants, &sessions, &bindings, &passwords)
	if e != nil {
		t.Fatalf("database assertions failed: %v", e)
	}
	if available != 5 || frozen != 0 || grants != 1 || sessions != 0 || bindings != 0 || passwords != 0 {
		t.Fatalf("unexpected persisted result: available=%d frozen=%d grants=%d sessions=%d bindings=%d passwords=%d", available, frozen, grants, sessions, bindings, passwords)
	}
	var encrypted bool
	if e := pool.QueryRow(ctx, "SELECT customer_verification_ref LIKE '1.%' AND customer_verification_ref <> 'manual-test-channel' FROM users WHERE username='flow_customer'").Scan(&encrypted); e != nil || !encrypted {
		t.Fatal("verification reference must be encrypted")
	}
	var reasonRecorded bool
	if e := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs a JOIN users u ON u.id=a.target_id WHERE u.username='flow_customer' AND a.action='USER_CREATED' AND a.redacted_meta='{"reason":"authorized browser integration test"}'::jsonb)`).Scan(&reasonRecorded); e != nil || !reasonRecorded {
		t.Fatal("account creation reason must be retained without setup secret/contact")
	}
	t.Log("PASS committed zero wallet, one GRANT despite retry/attacks, no USER session/binding/password bypass, encrypted verification reference")
	// A full MFA session may read management metadata after the recent window,
	// but the same identity must not create accounts or grant points.
	var p credit.Principal
	if e := pool.QueryRow(ctx, `SELECT s.user_id,s.id,s.generation FROM device_sessions s JOIN users u ON u.id=s.user_id WHERE u.username='flow_admin' AND s.status='ACTIVE'`).Scan(&p.ActorUserID, &p.SessionID, &p.Generation); e != nil {
		t.Fatal("current admin session missing")
	}
	if _, e := pool.Exec(ctx, "UPDATE device_sessions SET mfa_verified_at=clock_timestamp()-interval '6 minutes' WHERE id=$1", p.SessionID); e != nil {
		t.Fatal("test MFA age")
	}
	if page, e := service.ManagedUsers(ctx, p, account.UserFilter{}); e != nil || len(page.Items) != 2 {
		t.Fatal("full MFA read incorrectly requires recent MFA")
	}
	if _, e := service.CreateUser(ctx, p, testdb.UUID(t), account.CreateUserCommand{Username: "must_not_exist", Verification: "manual-test-channel", Reason: "must reject stale MFA"}); !errors.Is(e, credit.ErrUnauthorized) {
		t.Fatal("stale MFA permitted account creation")
	}
	var target pgtype.UUID
	if e := pool.QueryRow(ctx, "SELECT id FROM users WHERE username='flow_customer'").Scan(&target); e != nil {
		t.Fatal(e)
	}
	if _, e := credit.New(pool, keys).Grant(ctx, p, credit.Command{TargetUserID: target, Points: 9, Reason: "must reject stale MFA", IdempotencyKey: testdb.UUID(t)}); !errors.Is(e, credit.ErrUnauthorized) {
		t.Fatal("stale MFA permitted grant")
	}
	t.Log("PASS full MFA metadata read and recent MFA write boundary")
}
