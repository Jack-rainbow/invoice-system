// Package document provides invoice field validation and normalization primitives.
package document

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"invoice.local/system/backend/internal/money"
)

// Fields is the nine user-writable DraftFields values. A nil certification
// timestamp represents JSON null; a pointer to an empty string is invalid.
type Fields struct {
	RecipientName          string  `json:"recipient_name"`
	ProductCode            string  `json:"product_code"`
	Quantity               string  `json:"quantity"`
	UnitCode               string  `json:"unit_code"`
	UnitDescription        string  `json:"unit_description"`
	Description            string  `json:"description"`
	IssuedAt               string  `json:"issued_at"`
	CertificationDisplayAt *string `json:"certification_display_at"`
	UnitPrice              string  `json:"unit_price"`
}

// Stable validation codes are suitable for localization and field highlighting.
const (
	CodeRequired         = "required"
	CodeInvalidUTF8      = "invalid_utf8"
	CodeControlCharacter = "control_character"
	CodeLineBreak        = "line_break"
	CodeTooLong          = "too_long"
	CodeTooManyLines     = "too_many_lines"
	CodeInvalidDecimal   = "invalid_decimal"
	CodeInvalidTimestamp = "invalid_timestamp"
	CodeAmountLimit      = "amount_limit"
)

// FieldError contains a field identifier and a stable code, never its input.
// The derived field "total" identifies an invoice amount-limit failure.
type FieldError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Code }

// NormalizeFields validates all values and returns an independent normalized
// copy and exact amounts. Every error returns zero Fields and zero money.Totals.
//
// Required text must contain a non-whitespace code point. Meaningful text keeps
// its original spaces and Unicode sequence: no trim, case-folding, or Unicode
// normalization is performed. Limits count code points, not bytes or graphemes.
// Only unit_description accepts line feeds and converts CRLF to LF. Timestamps
// require an explicit offset and become UTC RFC3339Nano strings. Decimal grammar,
// precision, leading-zero limits, and canonical values come from money.Calculate.
// The caller owns schema/versioning, canonical snapshot serialization, and hashes.
func NormalizeFields(input Fields) (Fields, money.Totals, error) {
	result := input
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"recipient_name", input.RecipientName, 160},
		{"product_code", input.ProductCode, 32},
		{"unit_code", input.UnitCode, 32},
		{"description", input.Description, 240},
	} {
		if err := validateText(field.name, field.value, true, field.max, false); err != nil {
			return Fields{}, money.Totals{}, err
		}
	}
	result.UnitDescription = strings.ReplaceAll(input.UnitDescription, "\r\n", "\n")
	if err := validateText("unit_description", result.UnitDescription, false, 40, true); err != nil {
		return Fields{}, money.Totals{}, err
	}
	for _, field := range []struct{ name, value string }{
		{"quantity", input.Quantity}, {"unit_price", input.UnitPrice},
	} {
		if err := validateText(field.name, field.value, false, 0, false); err != nil {
			return Fields{}, money.Totals{}, err
		}
	}
	// Validate each decimal with a zero counterpart to attribute grammar errors
	// without duplicating money's parser or triggering an unrelated total limit.
	if _, err := money.Calculate("0", input.Quantity); err != nil {
		return Fields{}, money.Totals{}, &FieldError{"quantity", CodeInvalidDecimal}
	}
	if _, err := money.Calculate(input.UnitPrice, "0"); err != nil {
		return Fields{}, money.Totals{}, &FieldError{"unit_price", CodeInvalidDecimal}
	}
	totals, err := money.Calculate(input.UnitPrice, input.Quantity)
	if err != nil {
		if errors.Is(err, money.ErrAmountLimit) {
			return Fields{}, money.Totals{}, &FieldError{"total", CodeAmountLimit}
		}
		// Both decimal inputs passed the same parser above.
		return Fields{}, money.Totals{}, &FieldError{"total", CodeInvalidDecimal}
	}
	result.Quantity, result.UnitPrice = totals.CanonicalQuantity, totals.CanonicalUnitPrice
	result.IssuedAt, err = normalizeTimestamp("issued_at", input.IssuedAt)
	if err != nil {
		return Fields{}, money.Totals{}, err
	}
	if input.CertificationDisplayAt != nil {
		certification, err := normalizeTimestamp("certification_display_at", *input.CertificationDisplayAt)
		if err != nil {
			return Fields{}, money.Totals{}, err
		}
		result.CertificationDisplayAt = &certification
	}
	return result, totals, nil
}

func validateText(field, value string, required bool, max int, multiline bool) error {
	if !utf8.ValidString(value) {
		return &FieldError{field, CodeInvalidUTF8}
	}
	if required && strings.TrimSpace(value) == "" {
		return &FieldError{field, CodeRequired}
	}
	for _, r := range value {
		switch {
		case r == '\r' || r == '\u2028' || r == '\u2029' || (r == '\n' && !multiline):
			return &FieldError{field, CodeLineBreak}
		case r == '\n' && multiline:
			continue
		case unicode.IsControl(r) || isBidiControl(r):
			return &FieldError{field, CodeControlCharacter}
		}
	}
	if multiline && strings.Count(value, "\n") > 1 {
		return &FieldError{field, CodeTooManyLines}
	}
	if max > 0 {
		for _, line := range strings.Split(value, "\n") {
			if utf8.RuneCountInString(line) > max {
				return &FieldError{field, CodeTooLong}
			}
		}
	}
	return nil
}

// The Unicode Bidi_Control set includes directional marks, embeddings,
// overrides, and isolates; ordinary Arabic/Hebrew and joining characters remain
// valid. Avoid rejecting all format characters, which would damage normal text.
func isBidiControl(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// time.Parse accepts comma fractions, single-digit hours, overprecision, and
// some invalid offsets. Enforce the accepted RFC3339Nano wire shape first so
// malformed input cannot silently lose precision or acquire a guessed zone.
var timestampShape = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func normalizeTimestamp(field, value string) (string, error) {
	if err := validateText(field, value, false, 0, false); err != nil {
		return "", err
	}
	if len(value) > 35 || !timestampShape.MatchString(value) {
		return "", &FieldError{field, CodeInvalidTimestamp}
	}
	parsed, err := time.ParseInLocation(time.RFC3339Nano, value, time.UTC)
	if err != nil {
		return "", &FieldError{field, CodeInvalidTimestamp}
	}
	utc := parsed.UTC()
	// Converting an extreme offset must still produce a four-digit RFC3339 year.
	if utc.Year() < 0 || utc.Year() > 9999 {
		return "", &FieldError{field, CodeInvalidTimestamp}
	}
	return utc.Format(time.RFC3339Nano), nil
}
