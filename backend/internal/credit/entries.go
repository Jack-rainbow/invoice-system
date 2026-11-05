package credit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// Entry is one ledger line of the caller's own wallet, decimal strings only.
type Entry struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	DeltaAvailable string    `json:"delta_available"`
	DeltaFrozen    string    `json:"delta_frozen"`
	AfterAvailable string    `json:"after_available"`
	AfterFrozen    string    `json:"after_frozen"`
	OperationID    string    `json:"operation_id"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// EntryPage lists entries newest first with a sealed cursor bound to the owner.
type EntryPage struct {
	Items      []Entry `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

var entriesCursorContext = "CREDIT_ENTRIES_CURSOR/v1"

// Entries returns the owner's ledger page. Balances shown here are historical
// facts of each entry, never a live wallet promise.
func (s *Service) Entries(ctx context.Context, owner pgtype.UUID, cursor string, limit int32) (EntryPage, error) {
	if !owner.Valid {
		return EntryPage{}, ErrInvalid
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return EntryPage{}, ErrInvalid
	}
	if s.keys == nil {
		return EntryPage{}, ErrIntegrity
	}
	context := entriesCursorContext + "/" + uuidText(owner)
	params := db.CreditEntriesPageParams{AccountID: owner, PageLimit: limit + 1}
	if cursor != "" {
		plain, e := s.keys.OpenCursor(context, cursor)
		if e != nil {
			return EntryPage{}, ErrInvalid
		}
		created, id, ok := splitEntriesCursor(plain)
		if !ok {
			return EntryPage{}, ErrInvalid
		}
		params.CursorCreated = pgtype.Timestamptz{Time: created, Valid: true}
		params.CursorID = id
	}
	rows, e := db.New(s.pool).CreditEntriesPage(ctx, params)
	if e != nil {
		return EntryPage{}, e
	}
	page := EntryPage{Items: make([]Entry, 0, len(rows))}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		sealed, e := s.keys.SealCursor(context, last.CreatedAt.Time.UTC().Format(time.RFC3339Nano)+"|"+uuidText(last.ID))
		if e != nil {
			return EntryPage{}, e
		}
		page.NextCursor = &sealed
	}
	for _, row := range rows {
		page.Items = append(page.Items, Entry{
			ID: uuidText(row.ID), Kind: row.Kind,
			DeltaAvailable: strconv.FormatInt(row.DeltaAvailable, 10),
			DeltaFrozen:    strconv.FormatInt(row.DeltaFrozen, 10),
			AfterAvailable: strconv.FormatInt(row.AfterAvailable, 10),
			AfterFrozen:    strconv.FormatInt(row.AfterFrozen, 10),
			OperationID:    uuidText(row.OperationID), Reason: row.Reason,
			CreatedAt: row.CreatedAt.Time.UTC(),
		})
	}
	return page, nil
}

func splitEntriesCursor(plain string) (time.Time, pgtype.UUID, bool) {
	for i := 0; i < len(plain); i++ {
		if plain[i] != '|' {
			continue
		}
		created, e := time.Parse(time.RFC3339Nano, plain[:i])
		if e != nil {
			return time.Time{}, pgtype.UUID{}, false
		}
		id, ok := parseUUIDText(plain[i+1:])
		if !ok {
			return time.Time{}, pgtype.UUID{}, false
		}
		return created.UTC(), id, true
	}
	return time.Time{}, pgtype.UUID{}, false
}

// LookupOutcome reports the recorded state of one idempotent operation of the
// actor. Absence is not proof that an earlier ambiguous commit cannot finish.
type LookupOutcome struct {
	OperationID string
	Operation   string
	Status      string
	ResourceID  *string
}

// LookupOperation resolves one of the actor's own idempotency records. Only
// operations this deployment actually records are accepted.
func (s *Service) LookupOperation(ctx context.Context, actor pgtype.UUID, operation string, idempotencyKey pgtype.UUID) (LookupOutcome, error) {
	switch operation {
	case "CREATE_USER", "CREDIT_GRANT", "GENERATE":
	default:
		return LookupOutcome{}, ErrInvalid
	}
	if !actor.Valid || !idempotencyKey.Valid {
		return LookupOutcome{}, ErrInvalid
	}
	key := sha256.Sum256([]byte(uuidText(idempotencyKey)))
	row, e := db.New(s.pool).FindOperationByIdempotency(ctx, db.FindOperationByIdempotencyParams{
		ActorID: actor, Operation: operation, KeyHash: key[:]})
	if errors.Is(e, pgx.ErrNoRows) {
		return LookupOutcome{}, ErrNotFound
	}
	if e != nil {
		return LookupOutcome{}, e
	}
	out := LookupOutcome{OperationID: uuidText(row.ID), Operation: row.Operation, Status: row.Status}
	if row.ResourceID.Valid {
		id := uuidText(row.ResourceID)
		out.ResourceID = &id
	}
	return out, nil
}

// parseUUIDText accepts the canonical lowercase UUID form used in cursors.
func parseUUIDText(s string) (pgtype.UUID, bool) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return pgtype.UUID{}, false
	}
	hexed := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	raw, e := hex.DecodeString(hexed)
	if e != nil {
		return pgtype.UUID{}, false
	}
	var id pgtype.UUID
	copy(id.Bytes[:], raw)
	id.Valid = validUUID(id)
	return id, true
}
