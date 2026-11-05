package document

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"invoice.local/system/backend/internal/money"
)

func validFields() Fields {
	return Fields{
		RecipientName: "  José 王 مرحبا 👩‍💻  ", ProductCode: "53131620",
		Quantity: "005.000000", UnitCode: "DPC", UnitDescription: "Docenas de piezas",
		Description: "Producto e\u0301 100 ml", IssuedAt: "2026-11-04T10:00:00-06:00", UnitPrice: "00100.0",
	}
}

func TestNormalizeFieldsCanonicalAndIndependent(t *testing.T) {
	cert := "2024-02-29T12:30:01.120000000+02:30"
	input := validFields()
	input.CertificationDisplayAt = &cert
	input.UnitDescription = "línea 一\r\nlínea 二"
	original := input
	wantCert := "2024-02-29T10:00:01.12Z"
	want := input
	want.Quantity, want.UnitPrice = "5", "100.00"
	want.IssuedAt = "2026-11-04T16:00:00Z"
	want.UnitDescription = "línea 一\nlínea 二"
	want.CertificationDisplayAt = &wantCert
	got, totals, err := NormalizeFields(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize = %#v, %v; want %#v", got, err, want)
	}
	if totals.UnitCents != 10000 || totals.QuantityMicros != 5000000 || totals.Subtotal != "500.00" || totals.Tax != "80.00" || totals.Total != "580.00" {
		t.Fatalf("incorrect totals: %#v", totals)
	}
	if !reflect.DeepEqual(input, original) || cert != "2024-02-29T12:30:01.120000000+02:30" {
		t.Fatal("input was mutated")
	}
	*got.CertificationDisplayAt = "changed"
	if cert != "2024-02-29T12:30:01.120000000+02:30" {
		t.Fatal("result aliases input optional timestamp")
	}
}

func TestNormalizeFieldsEquivalentInputs(t *testing.T) {
	a := validFields()
	b := a
	b.Quantity, b.UnitPrice, b.IssuedAt = "5.0", "100.00", "2026-11-04T16:00:00.000Z"
	gotA, totalsA, errA := NormalizeFields(a)
	gotB, totalsB, errB := NormalizeFields(b)
	if errA != nil || errB != nil || !reflect.DeepEqual(gotA, gotB) || totalsA != totalsB {
		t.Fatalf("equivalent inputs differ: %#v %#v %v %v", gotA, gotB, errA, errB)
	}
	again, totalsAgain, err := NormalizeFields(gotA)
	if err != nil || !reflect.DeepEqual(again, gotA) || totalsAgain != totalsA {
		t.Fatal("normalization is not idempotent")
	}
}

func TestNormalizeFieldsPreservesUnicodeSequence(t *testing.T) {
	input := validFields()
	input.Description = " é e\u0301 \u00a0 👩‍💻 "
	got, _, err := NormalizeFields(input)
	if err != nil || got.Description != input.Description || got.RecipientName != input.RecipientName {
		t.Fatalf("Unicode sequences or spaces were changed: %v", err)
	}
}

func TestNormalizeFieldsCommonChecksApplyToEveryField(t *testing.T) {
	for _, test := range []struct {
		field string
		set   func(*Fields, string)
	}{
		{"recipient_name", func(f *Fields, v string) { f.RecipientName = v }},
		{"product_code", func(f *Fields, v string) { f.ProductCode = v }},
		{"quantity", func(f *Fields, v string) { f.Quantity = v }},
		{"unit_code", func(f *Fields, v string) { f.UnitCode = v }},
		{"unit_description", func(f *Fields, v string) { f.UnitDescription = v }},
		{"description", func(f *Fields, v string) { f.Description = v }},
		{"issued_at", func(f *Fields, v string) { f.IssuedAt = v }},
		{"certification_display_at", func(f *Fields, v string) { f.CertificationDisplayAt = &v }},
		{"unit_price", func(f *Fields, v string) { f.UnitPrice = v }},
	} {
		t.Run(test.field, func(t *testing.T) {
			for _, invalid := range []struct{ value, code string }{
				{string([]byte{0xff}), CodeInvalidUTF8},
				{"private\tvalue", CodeControlCharacter},
				{"private\u202evalue", CodeControlCharacter},
			} {
				input := validFields()
				test.set(&input, invalid.value)
				assertFieldFailure(t, input, test.field, invalid.code)
			}
		})
	}
}

func assertFieldFailure(t *testing.T, input Fields, field, code string) {
	t.Helper()
	got, totals, err := NormalizeFields(input)
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != field || fe.Code != code {
		t.Fatalf("error = %v; want %s/%s", err, field, code)
	}
	if !reflect.DeepEqual(got, Fields{}) || totals != (money.Totals{}) {
		t.Fatalf("failure returned partial result: %#v %#v", got, totals)
	}
}

func TestNormalizeFieldsTextBoundaries(t *testing.T) {
	setters := []struct {
		field string
		max   int
		set   func(*Fields, string)
	}{
		{"recipient_name", 160, func(f *Fields, v string) { f.RecipientName = v }},
		{"product_code", 32, func(f *Fields, v string) { f.ProductCode = v }},
		{"unit_code", 32, func(f *Fields, v string) { f.UnitCode = v }},
		{"description", 240, func(f *Fields, v string) { f.Description = v }},
	}
	for _, test := range setters {
		t.Run(test.field, func(t *testing.T) {
			input := validFields()
			test.set(&input, "")
			assertFieldFailure(t, input, test.field, CodeRequired)
			// Code points, not UTF-8 bytes: each emoji occupies four bytes.
			test.set(&input, strings.Repeat("😀", test.max))
			if _, _, err := NormalizeFields(input); err != nil {
				t.Fatalf("at limit: %v", err)
			}
			test.set(&input, strings.Repeat("😀", test.max+1))
			assertFieldFailure(t, input, test.field, CodeTooLong)
			for _, value := range []string{"a\nb", "a\r\nb", "a\rb", "a\u2028b", "a\u2029b"} {
				test.set(&input, value)
				assertFieldFailure(t, input, test.field, CodeLineBreak)
			}
			for _, blank := range []string{" ", "\u00a0", " \u2003 "} {
				test.set(&input, blank)
				assertFieldFailure(t, input, test.field, CodeRequired)
			}
			test.set(&input, " padded value ")
			got, _, err := NormalizeFields(input)
			if err != nil {
				t.Fatalf("padded text must be preserved: %v", err)
			}
			actual := map[string]string{"recipient_name": got.RecipientName, "product_code": got.ProductCode,
				"unit_code": got.UnitCode, "description": got.Description}
			if actual[test.field] != " padded value " {
				t.Fatal("meaningful leading/trailing spaces were removed")
			}
		})
	}
}

func TestNormalizeFieldsForbiddenCharacters(t *testing.T) {
	tests := []struct{ name, value, code string }{
		{"invalid_utf8", string([]byte{0xff}), CodeInvalidUTF8},
		{"nul", "private\x00value", CodeControlCharacter},
		{"tab", "private\tvalue", CodeControlCharacter},
		{"del", "private\x7fvalue", CodeControlCharacter},
		{"c1", "private\u0085value", CodeControlCharacter},
	}
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		tests = append(tests, struct{ name, value, code string }{"bidi", "private" + string(r) + "value", CodeControlCharacter})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validFields()
			input.RecipientName = test.value
			assertFieldFailure(t, input, "recipient_name", test.code)
			_, _, err := NormalizeFields(input)
			if strings.Contains(err.Error(), "private") {
				t.Fatal("error leaks input")
			}
		})
	}
}

func TestNormalizeFieldsUnitDescription(t *testing.T) {
	for _, value := range []string{"", "\n", strings.Repeat("界", 40), strings.Repeat("界", 40) + "\r\n" + strings.Repeat("😀", 40)} {
		input := validFields()
		input.UnitDescription = value
		got, _, err := NormalizeFields(input)
		if err != nil || got.UnitDescription != strings.ReplaceAll(value, "\r\n", "\n") {
			t.Fatalf("valid description rejected: %v", err)
		}
	}
	for _, test := range []struct{ value, code string }{
		{"a\nb\nc", CodeTooManyLines}, {"a\n\n", CodeTooManyLines},
		{strings.Repeat("界", 41), CodeTooLong}, {"a\n" + strings.Repeat("界", 41), CodeTooLong},
		{"a\rb", CodeLineBreak}, {"a\r\r\nb", CodeLineBreak}, {"a\u2028b", CodeLineBreak},
		{"a\tb", CodeControlCharacter}, {"a\u2067b", CodeControlCharacter}, {string([]byte{0xff}), CodeInvalidUTF8},
	} {
		input := validFields()
		input.UnitDescription = test.value
		assertFieldFailure(t, input, "unit_description", test.code)
	}
}

func TestNormalizeFieldsDates(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"2024-02-29T00:00:00+01:00", "2024-02-28T23:00:00Z"},
		{"2026-01-01T00:30:00+01:00", "2025-12-31T23:30:00Z"},
		{"2026-12-31T23:30:00-01:00", "2027-01-01T00:30:00Z"},
		{"2026-11-04T16:00:00.123456789Z", "2026-11-04T16:00:00.123456789Z"},
	} {
		input := validFields()
		input.IssuedAt = test.input
		input.CertificationDisplayAt = &test.input
		got, _, err := NormalizeFields(input)
		if err != nil || got.IssuedAt != test.want || got.CertificationDisplayAt == nil || *got.CertificationDisplayAt != test.want {
			t.Fatalf("date %s: %#v %v", test.input, got, err)
		}
	}
	for _, value := range []string{
		"", "2026-11-04", "2026-11-04T16:00:00", "2023-02-29T00:00:00Z", "2026-04-31T00:00:00Z",
		"2026-01-01T24:00:00Z", "2026-01-01T00:00:60Z", "2026-01-01T00:00:00+24:00", "2026-01-01T00:00:00+00:60",
		"2026-01-01t00:00:00z", "2026-01-01T0:00:00Z", "2026-01-01T00:00:00,1Z", "2026-01-01T00:00:00.1234567890Z",
		"2026-01-01T00:00:00Z ", "0000-01-01T00:00:00+01:00", "9999-12-31T23:59:59-01:00",
	} {
		t.Run(value, func(t *testing.T) {
			input := validFields()
			input.IssuedAt = value
			assertFieldFailure(t, input, "issued_at", CodeInvalidTimestamp)
			input = validFields()
			input.CertificationDisplayAt = &value
			assertFieldFailure(t, input, "certification_display_at", CodeInvalidTimestamp)
		})
	}
	got, _, err := NormalizeFields(validFields())
	if err != nil || got.CertificationDisplayAt != nil {
		t.Fatal("null certification must remain null")
	}
}

func TestNormalizeFieldsDecimalAndTotalBoundaries(t *testing.T) {
	for _, test := range []struct{ field, value string }{
		{"unit_price", "999999999.99"}, {"quantity", "999999999.999999"},
	} {
		input := validFields()
		input.UnitPrice, input.Quantity = "0", "0"
		if test.field == "unit_price" {
			input.UnitPrice = test.value
		} else {
			input.Quantity = test.value
		}
		if _, _, err := NormalizeFields(input); err != nil {
			t.Fatalf("maximum valid decimal: %v", err)
		}
	}
	for _, field := range []string{"quantity", "unit_price"} {
		values := []string{"", "-1", "+1", ".1", "1.", "1e2", "NaN", "Infinity", "1,000", "$1", "１", "١", " 1", "1000000000", "0000000000", "1..0", string([]byte{0xff})}
		if field == "quantity" {
			values = append(values, "0.0000001")
		} else {
			values = append(values, "0.001")
		}
		for _, value := range values {
			input := validFields()
			if field == "quantity" {
				input.Quantity = value
			} else {
				input.UnitPrice = value
			}
			code := CodeInvalidDecimal
			if value == string([]byte{0xff}) {
				code = CodeInvalidUTF8
			}
			assertFieldFailure(t, input, field, code)
		}
	}
	input := validFields()
	input.UnitPrice, input.Quantity = "999999999.99", "999999999.999999"
	assertFieldFailure(t, input, "total", CodeAmountLimit)
	input.UnitPrice, input.Quantity = "862068965.51", "1"
	_, totals, err := NormalizeFields(input)
	if err != nil || totals.TotalCents != 99999999999 {
		t.Fatalf("exact limit rejected: %#v %v", totals, err)
	}
	input.UnitPrice = "862068965.52"
	assertFieldFailure(t, input, "total", CodeAmountLimit)
}
