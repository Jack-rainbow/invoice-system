//go:build integration

package document

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/testdb"
)

type noLimit struct{}

func (noLimit) Allow(context.Context, string, string, int32, int32) error { return nil }

func setup(t *testing.T) (*Service, *pgxpool.Pool, pgtype.UUID, pgtype.UUID) {
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
	service, e := NewService(pool, keys, noLimit{})
	if e != nil {
		t.Fatal(e)
	}
	ctx := t.Context()
	first, second := testdb.UUID(t), testdb.UUID(t)
	for _, id := range []pgtype.UUID{first, second} {
		// Usernames must be unique across the whole private test database, not
		// just within one test, because every test shares one cluster.
		name := "draft_" + strings.ReplaceAll(account.IDText(id), "-", "")
		if _, e := pool.Exec(ctx, `INSERT INTO users(id,username,role,onboarding_state,device_state) VALUES($1,$2,'USER','READY','ACTIVE')`, id, name); e != nil {
			t.Fatal(e)
		}
	}
	return service, pool, first, second
}

func TestDraftLifecycle(t *testing.T) {
	service, _, ownerID, stranger := setup(t)
	ctx := t.Context()

	draft, e := service.Create(ctx, ownerID, sample())
	if e != nil {
		t.Fatal(e)
	}
	if draft.Revision != 1 || draft.ID == "" || draft.RenderProfileID == "" || draft.Amounts.Total != "580.00" {
		t.Fatalf("unexpected created draft: %+v", draft)
	}
	if draft.Fields.Quantity != "5" || draft.Fields.UnitPrice != "100.00" {
		t.Fatal("stored draft must be normalized")
	}

	// Another owner can neither read nor overwrite the draft.
	if _, e := service.Get(ctx, stranger, mustID(t, draft.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner read leaked or failed incorrectly: %v", e)
	}
	if _, e := service.Save(ctx, stranger, mustID(t, draft.ID), 1, sample()); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner write leaked or failed incorrectly: %v", e)
	}
	if e := service.Delete(ctx, stranger, mustID(t, draft.ID), 1); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner delete leaked or failed incorrectly: %v", e)
	}

	// Equivalent input that normalizes to the same canonical bytes keeps revision 1.
	same := sample()
	same.Quantity, same.UnitPrice = "5.000", "100"
	unchanged, e := service.Save(ctx, ownerID, mustID(t, draft.ID), 1, same)
	if e != nil || unchanged.Revision != 1 {
		t.Fatalf("unchanged canonical content must not bump revision: %v %+v", e, unchanged)
	}

	changed := sample()
	changed.RecipientName = "CLIENTE ACTUALIZADO"
	second, e := service.Save(ctx, ownerID, mustID(t, draft.ID), 1, changed)
	if e != nil || second.Revision != 2 || second.Fields.RecipientName != "CLIENTE ACTUALIZADO" {
		t.Fatalf("changed content must create revision 2: %v %+v", e, second)
	}
	// Stale expected revision is rejected and reports the persisted revision.
	var conflict *ConflictError
	if _, e := service.Save(ctx, ownerID, mustID(t, draft.ID), 1, changed); !errors.As(e, &conflict) || conflict.Code != "REVISION_CONFLICT" || conflict.Current != 2 {
		t.Fatalf("stale save must conflict with current revision: %v", e)
	}
	if e := service.Delete(ctx, ownerID, mustID(t, draft.ID), 1); !errors.As(e, &conflict) || conflict.Code != "REVISION_CONFLICT" {
		t.Fatalf("stale delete must conflict: %v", e)
	}
	readBack, e := service.Get(ctx, ownerID, mustID(t, draft.ID))
	if e != nil || readBack.Revision != 2 || readBack.Amounts.Total != "580.00" {
		t.Fatalf("read back mismatch: %v %+v", e, readBack)
	}
	if e := service.Delete(ctx, ownerID, mustID(t, draft.ID), 2); e != nil {
		t.Fatal(e)
	}
	if _, e := service.Get(ctx, ownerID, mustID(t, draft.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatal("deleted draft must not be readable")
	}
}

func TestDraftValidationAndLimits(t *testing.T) {
	service, _, ownerID, _ := setup(t)
	ctx := t.Context()
	invalid := sample()
	invalid.RecipientName = " \t "
	var violation *ValidationError
	if _, e := service.Create(ctx, ownerID, invalid); !errors.As(e, &violation) || violation.Errors[0].Code != "REQUIRED" {
		t.Fatalf("blank required text must be a required-field validation error: %v", e)
	}
	tooLong := sample()
	tooLong.Description = longText(241)
	if _, e := service.Create(ctx, ownerID, tooLong); !errors.As(e, &violation) || violation.Errors[0].Code != "TEXT_DOES_NOT_FIT" {
		t.Fatalf("overlong text must map to TEXT_DOES_NOT_FIT: %v", e)
	}
	if _, e := service.List(ctx, ownerID, Filter{Limit: 101}); !errors.Is(e, ErrInvalid) {
		t.Fatal("limit above 100 must be rejected")
	}
	if _, e := service.List(ctx, ownerID, Filter{Status: "DELETED"}); !errors.Is(e, ErrInvalid) {
		t.Fatal("unknown status filter must be rejected")
	}
}

func TestDraftListKeyset(t *testing.T) {
	service, pool, ownerID, stranger := setup(t)
	ctx := t.Context()
	ids := make([]pgtype.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		draft, e := service.Create(ctx, ownerID, sample())
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, mustID(t, draft.ID))
		// created_at is ordering metadata, never an ownership or version field.
		if _, e := pool.Exec(ctx, `UPDATE documents SET created_at=$2 WHERE id=$1`, ids[i], time.Date(2026, 11, 4-i, 0, 0, 0, 0, time.UTC)); e != nil {
			t.Fatal(e)
		}
	}
	first, e := service.List(ctx, ownerID, Filter{Limit: 2})
	if e != nil || len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page must return two items and a cursor: %v %+v", e, first)
	}
	if first.Items[0].ID != account.IDText(ids[0]) || first.Items[1].ID != account.IDText(ids[1]) {
		t.Fatal("documents must be ordered by created_at descending")
	}
	if first.Items[0].Status != "DRAFT" || first.Items[0].Revision != 1 {
		t.Fatalf("unexpected summary status: %+v", first.Items[0])
	}
	rest, e := service.List(ctx, ownerID, Filter{Limit: 2, Cursor: *first.NextCursor})
	if e != nil || len(rest.Items) != 1 || rest.NextCursor != nil || rest.Items[0].ID != account.IDText(ids[2]) {
		t.Fatalf("second page must contain the remaining item only: %v %+v", e, rest)
	}
	// A cursor is bound to owner and filter; reuse across either is rejected.
	if _, e := service.List(ctx, stranger, Filter{Limit: 2, Cursor: *first.NextCursor}); !errors.Is(e, ErrInvalid) {
		t.Fatalf("cursor reuse by another owner must be rejected: %v", e)
	}
	if _, e := service.List(ctx, ownerID, Filter{Limit: 2, Status: "DRAFT", Cursor: *first.NextCursor}); !errors.Is(e, ErrInvalid) {
		t.Fatalf("cursor reuse with a different filter must be rejected: %v", e)
	}
	empty, e := service.List(ctx, stranger, Filter{})
	if e != nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty page must be items=[] and next_cursor=null: %v %+v", e, empty)
	}
	filtered, e := service.List(ctx, ownerID, Filter{Status: "HAS_VERSIONS"})
	if e != nil || len(filtered.Items) != 0 {
		t.Fatalf("drafts without versions must not match HAS_VERSIONS: %v %+v", e, filtered)
	}
}

func TestDraftDeleteRefusesVersionedDocument(t *testing.T) {
	service, pool, ownerID, _ := setup(t)
	ctx := t.Context()
	draft, e := service.Create(ctx, ownerID, sample())
	if e != nil {
		t.Fatal(e)
	}
	// Build the minimal real immutable chain that forbids deletion.
	var profile pgtype.UUID
	if e := pool.QueryRow(ctx, `SELECT id FROM render_profiles WHERE status='ACTIVE' LIMIT 1`).Scan(&profile); e != nil {
		t.Fatal(e)
	}
	manifestID, rendererID := testdb.UUID(t), testdb.UUID(t)
	if _, e := pool.Exec(ctx, `INSERT INTO renderer_manifests(id,image_digest,asset_hashes,algorithm_versions,locale,page_settings,archive_ref)
		VALUES($1,'sha256:test','{}','{}','es-MX','{}','archive/test')`, rendererID); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO preview_manifests(id,owner_id,document_id,revision,profile_id,canonical_bytes,content_hash,amounts,expires_at)
		VALUES($1,$2,$3,1,$4,$5,'hash-test','{}',now()+interval '10 minutes')`, manifestID, ownerID, mustID(t, draft.ID), profile, []byte(`{"fields":{}}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO document_versions(id,document_id,owner_id,version_no,source_revision,manifest_id,renderer_manifest_id,snapshot_json,unit_cents,qty_micros,subtotal_cents,tax_cents,total_cents)
		VALUES($1,$2,$3,1,1,$4,$5,'{}',10000,5000000,50000,8000,58000)`, testdb.UUID(t), mustID(t, draft.ID), ownerID, manifestID, rendererID); e != nil {
		t.Fatal(e)
	}
	var conflict *ConflictError
	if e := service.Delete(ctx, ownerID, mustID(t, draft.ID), 1); !errors.As(e, &conflict) || conflict.Code != "DOCUMENT_HAS_VERSIONS" {
		t.Fatalf("versioned document must not be deleted: %v", e)
	}
	if page, e := service.List(ctx, ownerID, Filter{}); e != nil || len(page.Items) != 1 || page.Items[0].Status != "HAS_VERSIONS" {
		t.Fatalf("list status must reflect the immutable version: %v %+v", e, page)
	}
}

func mustID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	id, e := account.ParseID(raw)
	if e != nil {
		t.Fatal("invalid server-generated id")
	}
	return id
}

func longText(n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}
