//go:build integration

package credit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice.local/system/backend/internal/testdb"
)

type fixture struct {
	pool      *pgxpool.Pool
	principal Principal
	target    pgtype.UUID
}

func makeFixture(t *testing.T) fixture {
	return makeFixtureWithExpiry(t, false)
}

func makeFixtureWithExpiry(t *testing.T, expired bool) fixture {
	t.Helper()
	pool := testdb.Open(t)
	actor, session, generation, target := testdb.UUID(t), testdb.UUID(t), testdb.UUID(t), testdb.UUID(t)
	factor := testdb.UUID(t)
	ctx := t.Context()
	username := func(id pgtype.UUID) string { return "u" + fmt.Sprintf("%x", id.Bytes[:8]) }
	for _, item := range []struct {
		id          pgtype.UUID
		role, state string
	}{
		{actor, "SUPERADMIN", "NOT_REQUIRED"}, {target, "USER", "INITIAL_PENDING"},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO users(id,username,role,onboarding_state,device_state) VALUES($1,$2,$3,'READY',$4)`, item.id, username(item.id), item.role, item.state)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO credit_accounts(user_id) VALUES($1)`, target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO mfa_factors(id,user_id,state,seed_ciphertext,factor_version) VALUES($1,$2,'VERIFIED',$3,1)`, factor, actor, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	createdAt, expiresAt := time.Now(), time.Now().Add(8*time.Hour)
	if expired {
		createdAt, expiresAt = time.Now().Add(-9*time.Hour), time.Now().Add(-time.Minute)
	}
	_, err = pool.Exec(ctx, `INSERT INTO device_sessions(id,user_id,token_hash,generation,auth_version,binding_epoch,factor_version,mfa_verified_at,last_active_at,expires_at,status,created_at)
		VALUES($1,$2,$3,$4,1,1,1,clock_timestamp(),clock_timestamp(),$5,'ACTIVE',$6)`, session, actor, session.Bytes[:], generation, expiresAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool: pool, principal: Principal{ActorUserID: actor, SessionID: session, Generation: generation}, target: target}
}

func (f fixture) command(t *testing.T, points int64) Command {
	t.Helper()
	return Command{TargetUserID: f.target, Points: points, Reason: "  人工加分  ", IdempotencyKey: testdb.UUID(t)}
}

func (f fixture) state(t *testing.T) (int64, int64, int64, int64) {
	t.Helper()
	var available, ledger, audit, pending int64
	ctx := t.Context()
	if err := f.pool.QueryRow(ctx, `SELECT available FROM credit_accounts WHERE user_id=$1`, f.target).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM credit_entries WHERE account_id=$1`, f.target).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE target_id=$1 AND action='CREDIT_GRANT'`, f.target).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_records WHERE actor_id=$1 AND status='PENDING'`, f.principal.ActorUserID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return available, ledger, audit, pending
}

func TestGrantZeroWalletAndSameKey(t *testing.T) {
	f := makeFixture(t)
	if a, l, u, p := f.state(t); a != 0 || l != 0 || u != 0 || p != 0 {
		t.Fatalf("initial: %d %d %d %d", a, l, u, p)
	}
	s := New(f.pool, nil)
	cmd := f.command(t, 7)
	got, err := s.Grant(t.Context(), f.principal, cmd)
	if err != nil || got.OperationID == "" || got.Wallet.Available != "7" || got.Wallet.Frozen != "0" {
		t.Fatalf("grant: %#v %v", got, err)
	}
	more, err := s.Grant(t.Context(), f.principal, cmd)
	if err != nil || more != got {
		t.Fatalf("retry: %#v %v", more, err)
	}
	read, err := s.Read(t.Context(), f.principal, cmd)
	if err != nil || read != got {
		t.Fatalf("read: %#v %v", read, err)
	}
	if a, l, u, p := f.state(t); a != 7 || l != 1 || u != 1 || p != 0 {
		t.Fatalf("final: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantConcurrentSameKey(t *testing.T) {
	f := makeFixture(t)
	s := New(f.pool, nil)
	cmd := f.command(t, 5)
	results := make([]GrantResult, 20)
	errorsSeen := make([]error, 20)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], errorsSeen[index] = s.Grant(context.Background(), f.principal, cmd)
		}(i)
	}
	group.Wait()
	for i := range results {
		if errorsSeen[i] != nil || results[i] != results[0] {
			t.Fatalf("request %d: %#v %v", i, results[i], errorsSeen[i])
		}
	}
	if a, l, u, p := f.state(t); a != 5 || l != 1 || u != 1 || p != 0 {
		t.Fatalf("concurrent: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantSameKeyDifferentRequest(t *testing.T) {
	f := makeFixture(t)
	s := New(f.pool, nil)
	cmd := f.command(t, 2)
	if _, err := s.Grant(t.Context(), f.principal, cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Points = 3
	if _, err := s.Grant(t.Context(), f.principal, cmd); !errors.Is(err, ErrConflict) {
		t.Fatalf("wanted conflict, got %v", err)
	}
	if a, l, u, p := f.state(t); a != 2 || l != 1 || u != 1 || p != 0 {
		t.Fatalf("conflict: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantSameKeyRechecksCurrentAuthorization(t *testing.T) {
	f := makeFixture(t)
	s := New(f.pool, nil)
	cmd := f.command(t, 3)
	if _, err := s.Grant(t.Context(), f.principal, cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE device_sessions SET status='REVOKED' WHERE id=$1`, f.principal.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(t.Context(), f.principal, cmd); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("same key bypassed revoked session: %v", err)
	}
	if _, err := s.Read(t.Context(), f.principal, cmd); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("read bypassed revoked session: %v", err)
	}
	if a, l, u, p := f.state(t); a != 3 || l != 1 || u != 1 || p != 0 {
		t.Fatalf("revocation changed committed grant: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantOverflowAndRollback(t *testing.T) {
	f := makeFixture(t)
	_, err := f.pool.Exec(t.Context(), `UPDATE credit_accounts SET available=$2 WHERE user_id=$1`, f.target, int64(math.MaxInt64-1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(f.pool, nil).Grant(t.Context(), f.principal, f.command(t, 2))
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("wanted overflow, got %v", err)
	}
	if a, l, u, p := f.state(t); a != math.MaxInt64-1 || l != 0 || u != 0 || p != 0 {
		t.Fatalf("overflow: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantWriteFailureRollsBackAllRows(t *testing.T) {
	f := makeFixture(t)
	ctx := t.Context()
	_, err := f.pool.Exec(ctx, `CREATE FUNCTION credit_test_reject_grant() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.reason='forced failure' THEN RAISE EXCEPTION 'forced grant failure' USING ERRCODE='23514'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(ctx, `CREATE TRIGGER credit_test_reject BEFORE INSERT ON credit_entries FOR EACH ROW EXECUTE FUNCTION credit_test_reject_grant()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = f.pool.Exec(cleanup, `DROP TRIGGER IF EXISTS credit_test_reject ON credit_entries`)
		_, _ = f.pool.Exec(cleanup, `DROP FUNCTION IF EXISTS credit_test_reject_grant()`)
	})
	cmd := f.command(t, 4)
	cmd.Reason = "forced failure"
	if _, err := New(f.pool, nil).Grant(ctx, f.principal, cmd); err == nil {
		t.Fatal("expected SQL write failure")
	}
	if a, l, u, p := f.state(t); a != 0 || l != 0 || u != 0 || p != 0 {
		t.Fatalf("rollback: %d %d %d %d", a, l, u, p)
	}
}

func TestGrantCommitRetryKnownAbort(t *testing.T) {
	for _, sqlState := range []string{"40001", "40P01"} {
		t.Run(sqlState, func(t *testing.T) {
			f := makeFixture(t)
			ctx := t.Context()
			unique := testdb.UUID(t)
			suffix := fmt.Sprintf("%x", unique.Bytes[:8])
			attemptSequence := "credit_retry_attempt_" + suffix
			operationSequence := "credit_retry_operation_" + suffix
			function := "credit_retry_function_" + suffix
			trigger := "credit_retry_trigger_" + suffix
			for _, seq := range []string{attemptSequence, operationSequence} {
				if _, err := f.pool.Exec(ctx, "CREATE SEQUENCE "+seq); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
			 IF NEW.account_id <> '%s'::uuid THEN RETURN NEW; END IF;
			 IF nextval('%s') = 1 THEN
			  PERFORM setval('%s', greatest(1, hashtextextended(NEW.operation_id::text, 0) & 9223372036854775807));
			  RAISE EXCEPTION 'injected commit abort' USING ERRCODE='%s';
			 END IF;
			 IF (SELECT last_value FROM %s) <> greatest(1, hashtextextended(NEW.operation_id::text, 0) & 9223372036854775807) THEN
			  RAISE EXCEPTION 'retry changed operation ID' USING ERRCODE='23514';
			 END IF;
			 RETURN NEW;
			END $$`, function, uuidText(f.target), attemptSequence, operationSequence, sqlState, operationSequence)
			if _, err := f.pool.Exec(ctx, body); err != nil {
				t.Fatal(err)
			}
			ddl := fmt.Sprintf("CREATE CONSTRAINT TRIGGER %s AFTER INSERT ON credit_entries DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s()", trigger, function)
			if _, err := f.pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup := context.Background()
				_, _ = f.pool.Exec(cleanup, "DROP TRIGGER IF EXISTS "+trigger+" ON credit_entries")
				_, _ = f.pool.Exec(cleanup, "DROP FUNCTION IF EXISTS "+function+"()")
				_, _ = f.pool.Exec(cleanup, "DROP SEQUENCE IF EXISTS "+attemptSequence)
				_, _ = f.pool.Exec(cleanup, "DROP SEQUENCE IF EXISTS "+operationSequence)
			})
			result, err := New(f.pool, nil).Grant(ctx, f.principal, f.command(t, 6))
			if err != nil {
				t.Fatalf("known %s abort was not retried: %v", sqlState, err)
			}
			if result.OperationID == "" || result.Wallet.Available != "6" {
				t.Fatalf("unexpected result: %#v", result)
			}
			var attempts int64
			if err := f.pool.QueryRow(ctx, "SELECT last_value FROM "+attemptSequence).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != 2 {
				t.Fatalf("want exactly 2 commit attempts, got %d", attempts)
			}
			if a, l, u, p := f.state(t); a != 6 || l != 1 || u != 1 || p != 0 {
				t.Fatalf("retry state: %d %d %d %d", a, l, u, p)
			}
		})
	}
}

func TestGrantAuthorizationRevocation(t *testing.T) {
	cases := []struct {
		name, sql string
		subject   string
	}{
		{"ordinary actor", "", "ordinary"},
		{"revoked session", "UPDATE device_sessions SET status='REVOKED' WHERE id=$1", "session"},
		{"old generation", "UPDATE device_sessions SET generation=$2 WHERE id=$1", "none"},
		{"old factor", "UPDATE mfa_factors SET factor_version=2 WHERE user_id=$1", "actor"},
		{"old auth version", "UPDATE users SET auth_version=2 WHERE id=$1", "actor"},
		{"old binding epoch", "UPDATE users SET binding_epoch=2 WHERE id=$1", "actor"},
		// Aged mfa_verified_at no longer rejects: 2FA was removed from login and
		// the recent window with it; revoked/expired/idle cases below still do.
		{"idle session", "UPDATE device_sessions SET last_active_at=clock_timestamp()-interval '31 minutes' WHERE id=$1", "session"},
		{"absolute expired", "", "expired"},
		{"disabled actor", "UPDATE users SET is_active=false WHERE id=$1", "actor"},
		{"actor not ready", "UPDATE users SET onboarding_state='ENROLLMENT_REQUIRED' WHERE id=$1", "actor"},
		{"disabled target", "UPDATE users SET is_active=false WHERE id=$1", "target"},
		{"target hold", "UPDATE users SET recovery_state='RECOVERY_HOLD' WHERE id=$1", "target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := makeFixtureWithExpiry(t, tc.subject == "expired")
			if tc.subject == "ordinary" {
				f.principal.ActorUserID = f.target
			} else if tc.subject != "none" && tc.subject != "expired" {
				var id pgtype.UUID
				switch tc.subject {
				case "actor":
					id = f.principal.ActorUserID
				case "session":
					id = f.principal.SessionID
				case "target":
					id = f.target
				}
				if _, err := f.pool.Exec(t.Context(), tc.sql, id); err != nil {
					t.Fatal(err)
				}
			} else if tc.subject == "none" {
				f.principal.Generation = testdb.UUID(t)
			}
			_, err := New(f.pool, nil).Grant(t.Context(), f.principal, f.command(t, 1))
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("wanted unauthorized, got %v", err)
			}
			if a, l, u, p := f.state(t); a != 0 || l != 0 || u != 0 || p != 0 {
				t.Fatalf("unauthorized: %d %d %d %d", a, l, u, p)
			}
		})
	}
}
