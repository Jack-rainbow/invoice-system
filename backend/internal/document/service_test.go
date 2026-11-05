package document

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"invoice.local/system/backend/internal/account"
)

func testUUID() string { return "11111111-1111-4111-8111-111111111111" }

func testUUIDType() pgtype.UUID {
	id, e := account.ParseID(testUUID())
	if e != nil {
		panic(e)
	}
	return id
}

func sample() Fields {
	return Fields{
		RecipientName: "CLIENTE EJEMPLO", ProductCode: "53131620", Quantity: "5", UnitCode: "DPC",
		UnitDescription: "Docenas de piezas", Description: "Producto de ejemplo 100 ml",
		IssuedAt: "2026-11-04T10:00:00-06:00", UnitPrice: "100.00",
	}
}

func TestParseLimit(t *testing.T) {
	for _, input := range []string{"", "1", "100", "20"} {
		if _, e := ParseLimit(input); e != nil && input != "" {
			t.Fatalf("valid limit rejected: %s", input)
		}
	}
	for _, input := range []string{"0", "101", "-1", "1.0", " 1", "01", "abc"} {
		if _, e := ParseLimit(input); e == nil {
			t.Fatalf("invalid limit accepted: %q", input)
		}
	}
}

func TestSplitCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 4, 10, 0, 0, 123456000, time.UTC)
	id := testUUID()
	plain := created.Format(time.RFC3339Nano) + "|" + id
	parsed, parsedID, ok := splitCursor(plain)
	if !ok || !parsed.Equal(created) || parsedID.String() != id {
		t.Fatal("cursor round trip lost microsecond precision or identity")
	}
	for _, input := range []string{"", "|", "not-a-time|" + id, created.Format(time.RFC3339Nano) + "|", created.Format(time.RFC3339Nano) + "|x"} {
		if _, _, ok := splitCursor(input); ok {
			t.Fatalf("malformed cursor accepted: %q", input)
		}
	}
}

func TestCanonicalEqualityForEquivalentInput(t *testing.T) {
	a, _, e := NormalizeFields(sample())
	if e != nil {
		t.Fatal(e)
	}
	alternate := sample()
	alternate.Quantity = "5.000"
	alternate.UnitPrice = "100"
	b, _, e := NormalizeFields(alternate)
	if e != nil {
		t.Fatal(e)
	}
	left, _ := canonical(a)
	right, _ := canonical(b)
	if string(left) != string(right) {
		t.Fatalf("equivalent decimals must normalize to identical draft bytes: %s vs %s", left, right)
	}
}

func TestDraftFromComputesServerAmounts(t *testing.T) {
	normalized, _, e := NormalizeFields(sample())
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(normalized)
	draft, e := draftFrom(testUUIDType(), 1, testUUIDType(), payload)
	if e != nil {
		t.Fatal(e)
	}
	if draft.Amounts.Subtotal != "500.00" || draft.Amounts.Tax != "80.00" || draft.Amounts.Total != "580.00" || draft.Amounts.TaxRate != "0.16" {
		t.Fatalf("unexpected amounts: %+v", draft.Amounts)
	}
	if draft.CalculationVersion != "mxn-iva16-halfup-v1" {
		t.Fatal("calculation version must be fixed")
	}
	if _, _, e := parseDraft([]byte(`{"quantity":"x"}`)); e == nil {
		t.Fatal("corrupt stored draft must not be accepted")
	}
}

func TestFieldCodesMapToContractEnum(t *testing.T) {
	cases := []struct {
		in   *FieldError
		want string
	}{
		{&FieldError{"recipient_name", CodeRequired}, "REQUIRED"},
		{&FieldError{"description", CodeTooLong}, "TEXT_DOES_NOT_FIT"},
		{&FieldError{"unit_description", CodeTooManyLines}, "TEXT_DOES_NOT_FIT"},
		{&FieldError{"total", CodeAmountLimit}, "OUT_OF_RANGE"},
		{&FieldError{"issued_at", CodeInvalidTimestamp}, "INVALID_FORMAT"},
	}
	for _, c := range cases {
		got := fieldCodes(c.in)
		if len(got) != 1 || got[0].Code != c.want || got[0].Field != c.in.Field {
			t.Fatalf("%s/%s mapped to %+v", c.in.Field, c.in.Code, got)
		}
	}
}

func TestConflictErrorCarriesCode(t *testing.T) {
	e := &ConflictError{Code: "REVISION_CONFLICT", Current: 7}
	var target *ConflictError
	if !errors.As(error(e), &target) || target.Current != 7 || target.Error() != "REVISION_CONFLICT" {
		t.Fatal("conflict error must be inspectable")
	}
}
