//go:build integration

package generation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/preview"
	"invoice.local/system/backend/internal/render"
	"invoice.local/system/backend/internal/testdb"
)

type noLimit struct{}

func (noLimit) Allow(context.Context, string, string, int32, int32) error { return nil }

type harness struct {
	service  *Service
	previews *preview.Service
	docs     *document.Service
	pool     *pgxpool.Pool
	owner    pgtype.UUID
	stranger pgtype.UUID
}

// principalSession builds the server-side principal facts the HTTP layer
// derives from a verified signed request; the test inserts the same rows the
// real login flow produces.
func (h harness) principal(t *testing.T, user pgtype.UUID, admin bool) credit.Principal {
	t.Helper()
	ctx := t.Context()
	session, generation := testdb.UUID(t), testdb.UUID(t)
	var token [32]byte
	if _, e := rand.Read(token[:]); e != nil {
		t.Fatal(e)
	}
	binding := pgtype.UUID{}
	factorVersion := pgtype.Int4{}
	mfaVerified := pgtype.Timestamptz{}
	if !admin {
		// USER sessions require an ACTIVE device binding; the JWK only needs the
		// structural P-256 shape the column check validates.
		binding = testdb.UUID(t)
		x := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		y := base64.RawURLEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
		jwk := `{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`
		if _, e := h.pool.Exec(ctx, `INSERT INTO device_bindings(id,user_id,public_jwk,key_thumbprint,epoch,status) VALUES($1,$2,$3::jsonb,$4,1,'ACTIVE')`,
			binding, user, jwk, "thumbprint-"+account.IDText(binding)); e != nil {
			t.Fatalf("binding fixture: %v", e)
		}
	} else {
		factorVersion = pgtype.Int4{Int32: 3, Valid: true}
		mfaVerified = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	_, e := h.pool.Exec(ctx, `INSERT INTO device_sessions(id,user_id,token_hash,binding_id,generation,auth_version,binding_epoch,factor_version,mfa_verified_at,last_active_at,expires_at,status)
VALUES($1,$2,$3,$4,$5,1,1,$6,$7,clock_timestamp(),clock_timestamp()+interval '1 hour','ACTIVE')`,
		session, user, token[:], binding, generation, factorVersion, mfaVerified)
	if e != nil {
		t.Fatalf("session fixture: %v", e)
	}
	return credit.Principal{ActorUserID: user, SessionID: session, Generation: generation}
}

func setup(t *testing.T) harness {
	t.Helper()
	pool := testdb.Open(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.json")
	if e := account.GenerateKeyringFile(keyFile); e != nil {
		t.Fatal(e)
	}
	keys, e := account.LoadKeyring(keyFile)
	if e != nil {
		t.Fatal(e)
	}
	docs, e := document.NewService(pool, keys, noLimit{})
	if e != nil {
		t.Fatal(e)
	}
	assets, e := render.Load()
	if e != nil {
		t.Fatal(e)
	}
	previews, e := preview.NewService(pool, noLimit{}, assets)
	if e != nil {
		t.Fatal(e)
	}
	service, e := NewService(pool, noLimit{}, assets)
	if e != nil {
		t.Fatal(e)
	}
	owner, stranger := testdb.UUID(t), testdb.UUID(t)
	for _, id := range []pgtype.UUID{owner, stranger} {
		name := "gen_" + strings.ReplaceAll(account.IDText(id), "-", "")
		if _, e := pool.Exec(t.Context(), `INSERT INTO users(id,username,role,onboarding_state,device_state) VALUES($1,$2,'USER','READY','ACTIVE')`, id, name); e != nil {
			t.Fatal(e)
		}
		// Every generation authorization locks the wallet; the fixture creates
		// the real zero wallet the account service would have committed.
		if _, e := pool.Exec(t.Context(), `INSERT INTO credit_accounts(user_id,available,frozen) VALUES($1,0,0)`, id); e != nil {
			t.Fatal(e)
		}
	}
	return harness{service: service, previews: previews, docs: docs, pool: pool, owner: owner, stranger: stranger}
}

func grantPoints(t *testing.T, pool *pgxpool.Pool, user pgtype.UUID, points int64) {
	t.Helper()
	if _, e := pool.Exec(t.Context(), `INSERT INTO credit_accounts(user_id,available,frozen) VALUES($1,$2,0)
ON CONFLICT (user_id) DO UPDATE SET available=credit_accounts.available+$2`, user, points); e != nil {
		t.Fatal(e)
	}
}

func sampleFields() document.Fields {
	return document.Fields{
		RecipientName: "Distribuidora del Norte, S.A. de C.V.", ProductCode: "53131620",
		Quantity: "5", UnitCode: "DPC", UnitDescription: "Días por colocación",
		Description: "Mampara fija de cristal templado 6 mm", IssuedAt: "2026-11-04T16:00:00Z",
		UnitPrice: "100.00",
	}
}

// confirmedPreview saves a fresh draft, renders the real watermarked preview
// and returns the document id, revision and manifest id a generation must echo.
func (h harness) confirmedPreview(t *testing.T, owner pgtype.UUID, fields document.Fields) (string, int32, string) {
	t.Helper()
	draft, e := h.docs.Create(t.Context(), owner, fields)
	if e != nil {
		t.Fatal(e)
	}
	revision, manifest := h.queuePreview(t, owner, draft.ID, draft.Revision)
	return draft.ID, revision, manifest
}

// queuePreview renders and waits for the watermarked preview of one saved
// revision, returning the confirmation manifest a paid generation must echo.
func (h harness) queuePreview(t *testing.T, owner pgtype.UUID, documentID string, revision int32) (int32, string) {
	t.Helper()
	ctx := t.Context()
	queued, e := h.previews.Create(ctx, owner, mustParse(t, documentID), revision)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 20; i++ {
		state, e := h.previews.Status(ctx, owner, mustParse(t, queued.ID))
		if e != nil {
			t.Fatal(e)
		}
		if state.State == "READY" && state.PreviewManifestID != nil {
			return revision, *state.PreviewManifestID
		}
		if state.State == "FAILED" || state.State == "EXPIRED" {
			code := "<nil>"
			if state.ErrorCode != nil {
				code = *state.ErrorCode
			}
			t.Fatalf("preview failed [%s]: %+v", code, state)
		}
		_, e = h.previews.RunOnce(ctx)
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("preview never became READY")
	return 0, ""
}

func mustParse(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, e := account.ParseID(id)
	if e != nil {
		t.Fatal(e)
	}
	return parsed
}

// TestFormalChainBillsOnce covers the paid core: reserve on admit, consume on
// settle, receipt-gated download, free redownload at zero balance, and a
// second version after an edit billing again.
func TestFormalChainBillsOnce(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	grantPoints(t, h.pool, h.owner, 2)
	principal := h.principal(t, h.owner, false)

	docID, revision, manifest := h.confirmedPreview(t, h.owner, sampleFields())
	key := testdb.UUID(t)
	order, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: key})
	if e != nil {
		t.Fatal(e)
	}
	if order.State != "QUEUED" || order.BillingState != "RESERVED" || order.CostPoints != "1" ||
		order.AvailablePoints != "1" || order.FrozenPoints != "1" {
		t.Fatalf("unexpected admitted order: %+v", order)
	}

	// Same key replay returns the same order without a second reservation.
	replay, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: key})
	if e != nil || replay.GenerationID != order.GenerationID || replay.FrozenPoints != "1" {
		t.Fatalf("same-key replay created or double-reserved: %v %+v", e, replay)
	}
	// A different key on the same revision returns the same live order for free.
	second, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)})
	if e != nil || second.GenerationID != order.GenerationID || second.VersionID != order.VersionID {
		t.Fatalf("new key on existing revision must resume the original order: %v %+v", e, second)
	}
	// Same key with different parameters is rejected.
	if _, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 0, IdempotencyKey: key}); !errors.Is(e, ErrConflict) {
		t.Fatalf("changed parameters on the same key must conflict: %v", e)
	}

	// Download before delivery confirmation stays pending.
	if _, e := h.service.Download(ctx, h.owner, mustParse(t, order.VersionID), "pdf"); !errors.Is(e, ErrPending) {
		t.Fatalf("download before receipt must stay pending: %v", e)
	}

	// Worker renders, settles, finalizer records the durability receipt.
	if found, e := h.service.RunOnce(ctx); e != nil || !found {
		t.Fatalf("formal worker found nothing: %v %v", found, e)
	}
	if e := h.service.RunFinalize(ctx); e != nil {
		t.Fatal(e)
	}
	final, e := h.service.Status(ctx, h.owner, mustParse(t, order.GenerationID))
	if e != nil {
		t.Fatal(e)
	}
	if final.State != "COMPLETED" || final.DeliveryState != "READY" || final.FrozenPoints != "0" || final.AvailablePoints != "1" {
		t.Fatalf("unexpected settled order: %+v", final)
	}
	if final.Downloads == nil {
		t.Fatal("completed delivery must expose download paths")
	}

	pdf, e := h.service.Download(ctx, h.owner, mustParse(t, order.VersionID), "pdf")
	if e != nil || len(pdf.Data) < 100 || pdf.ContentType != "application/pdf" {
		t.Fatalf("pdf download failed: %v %d", e, len(pdf.Data))
	}
	if string(pdf.Data[:5]) != "%PDF-" {
		t.Fatal("clean PDF must start with the PDF header")
	}
	// Zero out the balance: redownloading a purchased version stays free.
	if _, e := h.pool.Exec(ctx, "UPDATE credit_accounts SET available=0 WHERE user_id=$1", h.owner); e != nil {
		t.Fatal(e)
	}
	png, e := h.service.Download(ctx, h.owner, mustParse(t, order.VersionID), "png")
	if e != nil || len(png.Data) < 100 || png.ContentType != "image/png" {
		t.Fatalf("free redownload at zero balance failed: %v %d", e, len(png.Data))
	}
	if string(png.Data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("clean file must be a PNG")
	}

	// A stranger sees nothing of the version.
	if _, e := h.service.Download(ctx, h.stranger, mustParse(t, order.VersionID), "pdf"); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner download leaked: %v", e)
	}
	if _, e := h.service.Status(ctx, h.stranger, mustParse(t, order.GenerationID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner status leaked: %v", e)
	}

	// Ledger: exactly one RESERVE, one CONSUME, no RELEASE.
	var reserve, consume, release int
	if e := h.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM credit_entries WHERE account_id=$1 AND kind='RESERVE'),
 (SELECT count(*) FROM credit_entries WHERE account_id=$1 AND kind='CONSUME'),
 (SELECT count(*) FROM credit_entries WHERE account_id=$1 AND kind='RELEASE')`, h.owner).Scan(&reserve, &consume, &release); e != nil {
		t.Fatal(e)
	}
	if reserve != 1 || consume != 1 || release != 0 {
		t.Fatalf("ledger must record one RESERVE and one CONSUME: %d %d %d", reserve, consume, release)
	}

	// Version list exposes the delivered version with amounts.
	versions, e := h.service.Versions(ctx, h.owner, mustParse(t, docID))
	if e != nil || len(versions) != 1 {
		t.Fatalf("version list: %v %d", e, len(versions))
	}
	if versions[0].DeliveryState != "READY" || versions[0].Amounts.Total != "580.00" || versions[0].Amounts.Tax != "80.00" {
		t.Fatalf("unexpected version summary: %+v", versions[0])
	}

	// An edit creates revision 2; its generation bills the second point. The
	// balance was zeroed above to prove free redownloads, so top up first.
	grantPoints(t, h.pool, h.owner, 1)
	edited := sampleFields()
	edited.Quantity = "3"
	saved, e := h.docs.Save(ctx, h.owner, mustParse(t, docID), revision, edited)
	if e != nil {
		t.Fatal(e)
	}
	rev2, manifest2 := h.queuePreview(t, h.owner, docID, saved.Revision)
	order2, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: rev2, ManifestID: mustParse(t, manifest2),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)})
	if e != nil {
		t.Fatal(e)
	}
	if order2.VersionID == order.VersionID || order2.AvailablePoints != "0" || order2.FrozenPoints != "1" {
		t.Fatalf("second version must be a new order with a new reservation: %+v", order2)
	}
	if found, e := h.service.RunOnce(ctx); e != nil || !found {
		t.Fatalf("worker for second order: %v %v", found, e)
	}
	if e := h.service.RunFinalize(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Download(ctx, h.owner, mustParse(t, order2.VersionID), "pdf"); e != nil {
		t.Fatalf("second version download: %v", e)
	}
	t.Log("PASS billed once per version, receipt-gated downloads, zero-balance redownload, stranger isolation")
}

// TestInsufficientAndStaleRefusals verifies admissions that must never reserve.
func TestInsufficientAndStaleRefusals(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	principal := h.principal(t, h.owner, false)
	grantPoints(t, h.pool, h.owner, 1)

	docID, revision, manifest := h.confirmedPreview(t, h.owner, sampleFields())
	var conflict *ConflictError
	// Cost mismatch refuses before touching the wallet.
	if _, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 0, IdempotencyKey: testdb.UUID(t)}); !errors.As(e, &conflict) || conflict.Code != "COST_CHANGED" {
		t.Fatalf("cost mismatch must refuse: %v", e)
	}
	// Stale revision refuses with the persisted revision.
	if _, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision + 5, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)}); !errors.As(e, &conflict) || conflict.Code != "REVISION_CONFLICT" {
		t.Fatalf("stale revision must conflict: %v", e)
	}
	// A manifest from another document is not a valid confirmation.
	otherDraft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, otherDraft.ID), ExpectedRevision: 1, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)}); !errors.As(e, &conflict) || conflict.Code != "PREVIEW_STALE" {
		t.Fatalf("foreign manifest must be stale: %v", e)
	}
	// Zero balance cannot reserve.
	if _, e := h.pool.Exec(ctx, "UPDATE credit_accounts SET available=0 WHERE user_id=$1", h.owner); e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)}); !errors.As(e, &conflict) || conflict.Code != "CREDITS_INSUFFICIENT" {
		t.Fatalf("zero balance must refuse: %v", e)
	}
	var reserved int
	if e := h.pool.QueryRow(ctx, "SELECT count(*) FROM credit_entries WHERE account_id=$1", h.owner).Scan(&reserved); e != nil || reserved != 0 {
		t.Fatalf("refusals must not write ledger rows: %v %d", e, reserved)
	}
	// A stranger cannot even see the document.
	if _, e := h.service.Create(ctx, h.principal(t, h.stranger, false), Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 1, IdempotencyKey: testdb.UUID(t)}); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner generation leaked: %v", e)
	}
	t.Log("PASS cost/revision/manifest/balance refusals left the wallet untouched")
}

// TestAdministratorFreeGeneration covers cost 0 with capacity admission.
func TestAdministratorFreeGeneration(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	admin := testdb.UUID(t)
	if _, e := h.pool.Exec(ctx, `INSERT INTO users(id,username,role,onboarding_state,device_state) VALUES($1,$2,'SUPERADMIN','READY','NOT_REQUIRED')`,
		admin, "gen_admin_"+strings.ReplaceAll(account.IDText(admin), "-", "")); e != nil {
		t.Fatal(e)
	}
	if _, e := h.pool.Exec(ctx, `INSERT INTO credit_accounts(user_id,available,frozen) VALUES($1,0,0)`, admin); e != nil {
		t.Fatal(e)
	}
	if _, e := h.pool.Exec(ctx, `INSERT INTO mfa_factors(id,user_id,state,seed_ciphertext,factor_version) VALUES($1,$2,'VERIFIED',$3,3)`, testdb.UUID(t), admin, []byte("fixture-seed")); e != nil {
		t.Fatal(e)
	}
	principal := h.principal(t, admin, true)

	docID, revision, manifest := h.confirmedPreview(t, admin, sampleFields())
	order, e := h.service.Create(ctx, principal, Command{
		DocumentID: mustParse(t, docID), ExpectedRevision: revision, ManifestID: mustParse(t, manifest),
		AcceptedCost: 0, IdempotencyKey: testdb.UUID(t)})
	if e != nil {
		t.Fatal(e)
	}
	if order.CostPoints != "0" || order.AvailablePoints != "0" || order.FrozenPoints != "0" {
		t.Fatalf("administrator generation must cost nothing: %+v", order)
	}
	if found, e := h.service.RunOnce(ctx); e != nil || !found {
		t.Fatalf("admin worker: %v %v", found, e)
	}
	if e := h.service.RunFinalize(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Download(ctx, admin, mustParse(t, order.VersionID), "pdf"); e != nil {
		t.Fatalf("admin download: %v", e)
	}
	var entries int
	if e := h.pool.QueryRow(ctx, "SELECT count(*) FROM credit_entries WHERE account_id=$1", admin).Scan(&entries); e != nil || entries != 0 {
		t.Fatalf("administrator generation must not write wallet ledger rows: %v %d", e, entries)
	}
	t.Log("PASS administrator generated for free through the same capacity lane")
}
