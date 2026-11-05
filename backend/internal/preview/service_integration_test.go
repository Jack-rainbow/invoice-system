//go:build integration

package preview

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/render"
	"invoice.local/system/backend/internal/testdb"
)

type noLimit struct{}

func (noLimit) Allow(context.Context, string, string, int32, int32) error { return nil }

type harness struct {
	service  *Service
	docs     *document.Service
	pool     *pgxpool.Pool
	owner    pgtype.UUID
	stranger pgtype.UUID
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
	service, e := NewService(pool, noLimit{}, assets)
	if e != nil {
		t.Fatal(e)
	}
	owner, stranger := testdb.UUID(t), testdb.UUID(t)
	for _, id := range []pgtype.UUID{owner, stranger} {
		name := "preview_" + strings.ReplaceAll(account.IDText(id), "-", "")
		if _, e := pool.Exec(t.Context(), `INSERT INTO users(id,username,role,onboarding_state,device_state) VALUES($1,$2,'USER','READY','ACTIVE')`, id, name); e != nil {
			t.Fatal(e)
		}
	}
	return harness{service: service, docs: docs, pool: pool, owner: owner, stranger: stranger}
}

func sampleFields() document.Fields {
	return document.Fields{
		RecipientName: "Distribuidora del Norte, S.A. de C.V.", ProductCode: "53131620",
		Quantity: "5", UnitCode: "DPC", UnitDescription: "Días por colocación",
		Description: "Mampara fija de cristal templado 6 mm", IssuedAt: "2026-11-04T16:00:00Z",
		UnitPrice: "100.00",
	}
}

// drain renders until the queue is empty so tests observe terminal states.
func (h harness) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 10; i++ {
		found, e := h.service.RunOnce(t.Context())
		if e != nil {
			t.Fatalf("worker: %v", e)
		}
		if !found {
			return
		}
	}
	t.Fatal("queue did not drain")
}

func TestPreviewLifecycleMergesAndIsolatesOwners(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	id := mustID(t, draft.ID)

	queued, e := h.service.Create(ctx, h.owner, id, draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if queued.State != "QUEUED" || queued.DraftRevision != 1 || queued.ImagePath != nil {
		t.Fatalf("unexpected queued response: %+v", queued)
	}

	// Same owner, revision, profile and content merges into the same request.
	merged, e := h.service.Create(ctx, h.owner, id, draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if merged.ID != queued.ID {
		t.Fatalf("identical content must merge: %s != %s", merged.ID, queued.ID)
	}

	h.drain(t)
	ready, e := h.service.Status(ctx, h.owner, mustID(t, queued.ID))
	if e != nil {
		t.Fatal(e)
	}
	if ready.State != "READY" || ready.PreviewManifestID == nil || ready.ImagePath == nil || ready.ConfirmationExpiresAt == nil {
		t.Fatalf("unexpected ready response: %+v", ready)
	}
	image, e := h.service.Image(ctx, h.owner, mustID(t, queued.ID))
	if e != nil {
		t.Fatal(e)
	}
	if len(image) == 0 || len(image) > 2<<20 {
		t.Fatalf("preview bytes = %d", len(image))
	}
	decoded, e := jpeg.Decode(bytes.NewReader(image))
	if e != nil {
		t.Fatalf("preview is not JPEG: %v", e)
	}
	if decoded.Bounds().Dx() != 1000 {
		t.Fatalf("preview width = %d", decoded.Bounds().Dx())
	}

	// A different owner learns nothing: no status, no bytes.
	if _, e := h.service.Status(ctx, h.stranger, mustID(t, queued.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner status = %v; want ErrNotFound", e)
	}
	if _, e := h.service.Image(ctx, h.stranger, mustID(t, queued.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner image = %v; want ErrNotFound", e)
	}
	if _, e := h.service.Create(ctx, h.stranger, id, draft.Revision); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner create = %v; want ErrNotFound", e)
	}
}

func TestPreviewSupersedesWaitingSlotOnNewContent(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	id := mustID(t, draft.ID)
	first, e := h.service.Create(ctx, h.owner, id, draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	changed := sampleFields()
	changed.Description = "Mampara corrediza de aluminio"
	saved, e := h.docs.Save(ctx, h.owner, id, draft.Revision, changed)
	if e != nil {
		t.Fatal(e)
	}
	second, e := h.service.Create(ctx, h.owner, id, saved.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if second.ID == first.ID {
		t.Fatal("new content must create a new request")
	}
	// Only the newest queued request survives; the older one is retired with a
	// stable reason instead of being silently dropped.
	old, e := h.service.Status(ctx, h.owner, mustID(t, first.ID))
	if e != nil {
		t.Fatal(e)
	}
	if old.State != "EXPIRED" || old.ErrorCode == nil || *old.ErrorCode != "PREVIEW_STALE" {
		t.Fatalf("superseded request = %+v", old)
	}
	h.drain(t)
	ready, e := h.service.Status(ctx, h.owner, mustID(t, second.ID))
	if e != nil {
		t.Fatal(e)
	}
	if ready.State != "READY" {
		t.Fatalf("newest request should be ready: %+v", ready)
	}
	// The stale manifest must not be confirmable: it belongs to old content.
	if ready.PreviewManifestID == nil || *ready.PreviewManifestID == "" {
		t.Fatal("ready response must name its confirmation manifest")
	}
}

func TestPreviewRefusesRevokedProfileAndStaleRevision(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	id := mustID(t, draft.ID)
	var conflict *ConflictError
	if _, e := h.service.Create(ctx, h.owner, id, draft.Revision+1); !errors.As(e, &conflict) || conflict.Code != "REVISION_CONFLICT" || conflict.Current != draft.Revision {
		t.Fatalf("stale revision = %v", e)
	}
	// Simulate a profile revocation: the active profile is replaced by the
	// deployment, so the draft's pinned profile can no longer render. Restore
	// exactly the profile this test revoked — other packages' tests share the
	// database and their drafts stay pinned to the deployment's real profile.
	var activeID string
	if e := h.pool.QueryRow(ctx, `SELECT id::text FROM render_profiles WHERE status='ACTIVE'`).Scan(&activeID); e != nil {
		t.Fatal(e)
	}
	if _, e := h.pool.Exec(ctx, `UPDATE render_profiles SET status='REVOKED' WHERE id=$1`, activeID); e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Create(ctx, h.owner, id, draft.Revision); !errors.As(e, &conflict) || conflict.Code != "PROFILE_REVOKED" {
		t.Fatalf("revoked profile = %v", e)
	}
	if _, e := h.pool.Exec(ctx, `UPDATE render_profiles SET status='ACTIVE' WHERE id=$1`, activeID); e != nil {
		t.Fatal(e)
	}
}

func TestPreviewExpiresAndRetiresItsObject(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	id := mustID(t, draft.ID)
	created, e := h.service.Create(ctx, h.owner, id, draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	h.drain(t)
	if _, e := h.service.Image(ctx, h.owner, mustID(t, created.ID)); e != nil {
		t.Fatal(e)
	}
	// Move the service clock past the TTL: the next read reports EXPIRED and the
	// object bytes disappear in the same transaction. The row itself stays
	// untouched because expires_at must always remain after created_at.
	h.service.now = func() time.Time { return time.Now().Add(TTL + time.Minute) }
	status, e := h.service.Status(ctx, h.owner, mustID(t, created.ID))
	if e != nil {
		t.Fatal(e)
	}
	if status.State != "EXPIRED" || status.ImagePath != nil {
		t.Fatalf("expired preview = %+v", status)
	}
	if _, e := h.service.Image(ctx, h.owner, mustID(t, created.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("retired object = %v; want ErrNotFound", e)
	}
	var objects int
	if e := h.pool.QueryRow(ctx, `SELECT count(*) FROM preview_objects WHERE request_id=$1`, mustID(t, created.ID)).Scan(&objects); e != nil {
		t.Fatal(e)
	}
	if objects != 0 {
		t.Fatalf("expired preview kept %d objects", objects)
	}
}

func TestPreviewStaleQueueAfterDraftMovesOn(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	id := mustID(t, draft.ID)
	created, e := h.service.Create(ctx, h.owner, id, draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	// The draft changes before the worker runs: the queued snapshot is gone.
	changed := sampleFields()
	changed.RecipientName = "Otro Cliente"
	if _, e := h.docs.Save(ctx, h.owner, id, draft.Revision, changed); e != nil {
		t.Fatal(e)
	}
	h.drain(t)
	status, e := h.service.Status(ctx, h.owner, mustID(t, created.ID))
	if e != nil {
		t.Fatal(e)
	}
	if status.State != "FAILED" || status.ErrorCode == nil || *status.ErrorCode != "PREVIEW_STALE" {
		t.Fatalf("stale queue = %+v", status)
	}
}

func TestPreviewRateLimitApplies(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	limited := &limitedAfter{remaining: 1}
	h.service.limiter = limited
	if _, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision); e == nil {
		t.Fatal("second create must be throttled")
	}
}

type limitedAfter struct{ remaining int }

func (l *limitedAfter) Allow(context.Context, string, string, int32, int32) error {
	if l.remaining <= 0 {
		return errors.New("rate limited")
	}
	l.remaining--
	return nil
}

func TestSweepRetiresExpiredPreviewsOnly(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	created, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	h.drain(t)
	if e := h.service.Sweep(ctx); e != nil {
		t.Fatal(e)
	}
	// Nothing is expired yet, so the live preview keeps its bytes.
	if _, e := h.service.Image(ctx, h.owner, mustID(t, created.ID)); e != nil {
		t.Fatalf("sweep removed a live preview: %v", e)
	}
	// Backdate both timestamps so the CHECK(expires_at > created_at) invariant
	// still holds while the sweep sees a long-expired preview.
	if _, e := h.pool.Exec(ctx, `UPDATE preview_requests SET created_at=now()-interval '2 days', expires_at=now()-interval '1 day' WHERE id=$1`, mustID(t, created.ID)); e != nil {
		t.Fatal(e)
	}
	if e := h.service.Sweep(ctx); e != nil {
		t.Fatal(e)
	}
	var state string
	if e := h.pool.QueryRow(ctx, `SELECT state FROM preview_requests WHERE id=$1`, mustID(t, created.ID)).Scan(&state); e != nil {
		t.Fatal(e)
	}
	if state != "EXPIRED" {
		t.Fatalf("sweep left state %q", state)
	}
}

func TestPreviewManifestIsImmutableAndCarriesAmounts(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	created, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	h.drain(t)
	ready, e := h.service.Status(ctx, h.owner, mustID(t, created.ID))
	if e != nil {
		t.Fatal(e)
	}
	var amounts string
	var expires time.Time
	if e := h.pool.QueryRow(ctx, `SELECT amounts::text, expires_at FROM preview_manifests WHERE id=$1`, mustID(t, *ready.PreviewManifestID)).Scan(&amounts, &expires); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{`"total_cents": 58000`, `"total": "580.00"`, `"calculation_version": "mxn-iva16-halfup-v1"`} {
		if !strings.Contains(amounts, want) {
			t.Fatalf("manifest amounts %s missing %s", amounts, want)
		}
	}
	// Confirmation manifests are immutable history.
	if _, e := h.pool.Exec(ctx, `UPDATE preview_manifests SET amounts='{}'::jsonb WHERE id=$1`, mustID(t, *ready.PreviewManifestID)); e == nil {
		t.Fatal("preview manifest update must be rejected")
	}
}

func TestDraftDeletionRetiresItsPreviews(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	created, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision)
	if e != nil {
		t.Fatal(e)
	}
	h.drain(t)
	if _, e := h.service.Image(ctx, h.owner, mustID(t, created.ID)); e != nil {
		t.Fatal(e)
	}
	// A previewed draft is still the owner's to delete: free previews are not
	// history, so their objects, requests and confirmation manifests go with it.
	if e := h.docs.Delete(ctx, h.owner, mustID(t, draft.ID), draft.Revision); e != nil {
		t.Fatalf("delete previewed draft: %v", e)
	}
	if _, e := h.service.Status(ctx, h.owner, mustID(t, created.ID)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("preview survived its draft: %v", e)
	}
	for table, want := range map[string]int{"preview_requests": 0, "preview_manifests": 0, "preview_objects": 0} {
		var count int
		if e := h.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE owner_id=$1`, h.owner).Scan(&count); e != nil {
			t.Fatal(e)
		}
		if count != want {
			t.Fatalf("%s left %d rows", table, count)
		}
	}
}

func TestRevokedProfileRequiresExplicitAdoptionBeforePreview(t *testing.T) {
	h := setup(t)
	ctx := t.Context()
	draft, e := h.docs.Create(ctx, h.owner, sampleFields())
	if e != nil {
		t.Fatal(e)
	}
	// A draft saved under the old profile: the deployment later revoked it, so the
	// pinned template can no longer render and the content is unchanged.
	if _, e := h.pool.Exec(ctx, `UPDATE documents SET render_profile_id='00000000-0000-4000-8000-000000000001', revision=revision+1 WHERE id=$1`, mustID(t, draft.ID)); e != nil {
		t.Fatal(e)
	}
	var conflict *ConflictError
	if _, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), draft.Revision+1); !errors.As(e, &conflict) || conflict.Code != "PROFILE_REVOKED" {
		t.Fatalf("preview on revoked profile = %v", e)
	}
	profiles, e := h.docs.Profiles(ctx, h.owner)
	if e != nil {
		t.Fatal(e)
	}
	if len(profiles) != 1 || profiles[0].Status != "ACTIVE" {
		t.Fatalf("profiles = %+v", profiles)
	}
	adopted, e := h.docs.ChangeProfile(ctx, h.owner, mustID(t, draft.ID), draft.Revision+1, profiles[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	if adopted.Revision != draft.Revision+2 || adopted.RenderProfileID != profiles[0].ID {
		t.Fatalf("adoption = %+v", adopted)
	}
	// Adopting the already-active profile is a no-op, not a revision bump.
	again, e := h.docs.ChangeProfile(ctx, h.owner, mustID(t, draft.ID), adopted.Revision, profiles[0].ID)
	if e != nil || again.Revision != adopted.Revision {
		t.Fatalf("idempotent adoption = %+v, %v", again, e)
	}
	created, e := h.service.Create(ctx, h.owner, mustID(t, draft.ID), adopted.Revision)
	if e != nil {
		t.Fatalf("preview after adoption: %v", e)
	}
	h.drain(t)
	ready, e := h.service.Status(ctx, h.owner, mustID(t, created.ID))
	if e != nil || ready.State != "READY" {
		t.Fatalf("preview after adoption = %+v, %v", ready, e)
	}
}

func mustID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	id, e := account.ParseID(raw)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
