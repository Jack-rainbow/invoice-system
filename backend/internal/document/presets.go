package document

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"invoice.local/system/backend/internal/account"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// PresetQuota bounds stored presets per owner; existing presets are never
// deleted to make room.
const PresetQuota = 500

// Preset is one owner-scoped reusable field set. Only user-fillable fields are
// stored: derived amounts and roles are never part of a preset.
type Preset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Fields Fields `json:"fields"`
}

// PresetPage lists presets with the same sealed-cursor contract as documents.
type PresetPage struct {
	Items      []Preset `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

const presetCursorVersion = "PRESETS_CURSOR/v1"

// SavePreset creates or replaces the named preset for the owner. The same name
// updates in place; validation is identical to a draft save.
func (s *Service) SavePreset(ctx context.Context, owner pgtype.UUID, id pgtype.UUID, name string, fields Fields) (Preset, error) {
	if !owner.Valid {
		return Preset{}, ErrInvalid
	}
	if !id.Valid {
		// Create path: the name must be a valid single-line label.
		if !validPresetName(name) {
			return Preset{}, ErrInvalid
		}
	}
	normalized, _, e := NormalizeFields(fields)
	if e != nil {
		return Preset{}, &ValidationError{Errors: fieldCodes(e)}
	}
	payload, e := json.Marshal(normalized)
	if e != nil {
		return Preset{}, ErrInvalid
	}
	if e = s.limiter.Allow(ctx, "presets-write", account.IDText(owner), 60, 60); e != nil {
		return Preset{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Preset{}, e
	}
	defer rollback(tx)
	if id.Valid {
		// Replace an existing preset; the owner scope is enforced by the row
		// lookup, and the new name must stay unique per owner.
		if _, e = q.LockPresetForOwner(ctx, db.LockPresetForOwnerParams{ID: id, OwnerID: owner}); errors.Is(e, pgx.ErrNoRows) {
			return Preset{}, ErrNotFound
		} else if e != nil {
			return Preset{}, e
		}
		if e = checkName(ctx, q, owner, id, name); e != nil {
			return Preset{}, e
		}
		if n, e := q.UpdatePresetFields(ctx, db.UpdatePresetFieldsParams{ID: id, OwnerID: owner, Name: name, FieldsJson: payload}); e != nil {
			return Preset{}, e
		} else if n != 1 {
			return Preset{}, ErrNotFound
		}
		if e = tx.Commit(ctx); e != nil {
			return Preset{}, e
		}
		return Preset{ID: account.IDText(id), Name: name, Fields: normalized}, nil
	}
	if e = checkName(ctx, q, owner, pgtype.UUID{}, name); e != nil {
		return Preset{}, e
	}
	count, e := q.PresetCountForOwner(ctx, owner)
	if e != nil {
		return Preset{}, e
	}
	if count >= PresetQuota {
		return Preset{}, ErrUnavailable
	}
	newID := account.NewID()
	if e = q.InsertPreset(ctx, db.InsertPresetParams{ID: newID, OwnerID: owner, Name: name, FieldsJson: payload}); e != nil {
		return Preset{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Preset{}, e
	}
	return Preset{ID: account.IDText(newID), Name: name, Fields: normalized}, nil
}

// checkName validates the label and rejects a duplicate name under the same
// owner (a different owner may use the same name). The database UNIQUE
// constraint is the final arbiter under concurrency.
func checkName(ctx context.Context, q *db.Queries, owner pgtype.UUID, exclude pgtype.UUID, name string) error {
	if !validPresetName(name) {
		return ErrInvalid
	}
	rows, e := q.PresetPage(ctx, db.PresetPageParams{OwnerID: owner, PageLimit: PresetQuota + 1})
	if e != nil {
		return e
	}
	for _, row := range rows {
		if row.Name == name && row.ID != exclude {
			return &ConflictError{Code: "PRESET_NAME_EXISTS"}
		}
	}
	return nil
}

func validPresetName(name string) bool {
	if len(name) == 0 || len([]rune(name)) > 160 {
		return false
	}
	for _, r := range name {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}

// DeletePreset removes one owned preset. Presets have no entitlements.
func (s *Service) DeletePreset(ctx context.Context, owner, id pgtype.UUID) error {
	if !owner.Valid || !id.Valid {
		return ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "presets-write", account.IDText(owner), 60, 60); e != nil {
		return e
	}
	n, e := db.New(s.pool).DeletePreset(ctx, db.DeletePresetParams{ID: id, OwnerID: owner})
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// ListPresets returns the owner's presets newest first.
func (s *Service) ListPresets(ctx context.Context, owner pgtype.UUID, cursor string, limit int32) (PresetPage, error) {
	if !owner.Valid {
		return PresetPage{}, ErrInvalid
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return PresetPage{}, ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "presets-read", account.IDText(owner), 120, 60); e != nil {
		return PresetPage{}, e
	}
	context := presetCursorVersion + "/" + account.IDText(owner)
	params := db.PresetPageParams{OwnerID: owner, PageLimit: limit + 1}
	if cursor != "" {
		plain, e := s.keys.OpenCursor(context, cursor)
		if e != nil {
			return PresetPage{}, ErrInvalid
		}
		created, id, ok := splitCursor(plain)
		if !ok {
			return PresetPage{}, ErrInvalid
		}
		params.CursorCreated, params.CursorID = stampTime(created), id
	}
	rows, e := db.New(s.pool).PresetPage(ctx, params)
	if e != nil {
		return PresetPage{}, e
	}
	page := PresetPage{Items: make([]Preset, 0, len(rows))}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		sealed, e := s.keys.SealCursor(context, last.CreatedAt.Time.UTC().Format(time.RFC3339Nano)+"|"+account.IDText(last.ID))
		if e != nil {
			return PresetPage{}, e
		}
		page.NextCursor = &sealed
	}
	for _, row := range rows {
		var fields Fields
		if e := json.Unmarshal(row.FieldsJson, &fields); e != nil {
			return PresetPage{}, ErrInvalid
		}
		page.Items = append(page.Items, Preset{ID: account.IDText(row.ID), Name: row.Name, Fields: fields})
	}
	return page, nil
}
