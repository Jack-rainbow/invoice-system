// Package money computes invoice amounts using non-negative fixed-point values.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

var (
	// ErrInvalidAmount indicates an invalid decimal representation or precision.
	ErrInvalidAmount = errors.New("invalid amount")
	// ErrAmountLimit indicates a valid input whose invoice exceeds the total limit.
	ErrAmountLimit = errors.New("amount limit exceeded")
)

// Totals contains exact snapshot values and two-decimal display amounts.
type Totals struct {
	UnitCents          int64
	QuantityMicros     int64
	SubtotalCents      int64
	TaxCents           int64
	TotalCents         int64
	Subtotal           string
	Tax                string
	Total              string
	CanonicalUnitPrice string
	CanonicalQuantity  string
}

// Calculate rounds the subtotal first, then its fixed 16 percent tax, using
// non-negative HALF_UP. Decimal input must include an integer part and, when a
// decimal point is present, at least one fractional digit. Leading zeros count
// toward the nine-digit integer limit. Errors return an empty Totals value.
func Calculate(unitPrice, quantity string) (Totals, error) {
	unit, err := parseFixed(unitPrice, 2)
	if err != nil {
		return Totals{}, fmt.Errorf("unit price: %w", err)
	}
	qty, err := parseFixed(quantity, 6)
	if err != nil {
		return Totals{}, fmt.Errorf("quantity: %w", err)
	}

	subtotal := new(big.Int).Mul(big.NewInt(unit), big.NewInt(qty))
	subtotal.Add(subtotal, big.NewInt(500000))
	subtotal.Quo(subtotal, big.NewInt(1000000))
	tax := new(big.Int).Mul(subtotal, big.NewInt(16))
	tax.Add(tax, big.NewInt(50))
	tax.Quo(tax, big.NewInt(100))
	total := new(big.Int).Add(subtotal, tax)
	if total.Cmp(big.NewInt(99999999999)) > 0 {
		return Totals{}, ErrAmountLimit
	}

	// All results are non-negative and bounded by total before conversion.
	subCents, taxCents, totalCents := subtotal.Int64(), tax.Int64(), total.Int64()
	canonicalQty := strings.TrimRight(strings.TrimRight(formatFixed(qty, 6), "0"), ".")
	return Totals{
		UnitCents: unit, QuantityMicros: qty,
		SubtotalCents: subCents, TaxCents: taxCents, TotalCents: totalCents,
		Subtotal: formatFixed(subCents, 2), Tax: formatFixed(taxCents, 2), Total: formatFixed(totalCents, 2),
		CanonicalUnitPrice: formatFixed(unit, 2), CanonicalQuantity: canonicalQty,
	}, nil
}

func parseFixed(input string, decimals int) (int64, error) {
	if len(input) == 0 || len(input) > 9+1+decimals {
		return 0, ErrInvalidAmount
	}
	integerDigits, fractionalDigits := 0, 0
	dot := false
	var value int64
	for i := 0; i < len(input); i++ {
		c := input[i]
		if c == '.' && !dot && integerDigits > 0 {
			dot = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, ErrInvalidAmount
		}
		if dot {
			fractionalDigits++
		} else {
			integerDigits++
		}
		if integerDigits > 9 || fractionalDigits > decimals {
			return 0, ErrInvalidAmount
		}
		value = value*10 + int64(c-'0')
	}
	if integerDigits == 0 || (dot && fractionalDigits == 0) {
		return 0, ErrInvalidAmount
	}
	// At most fifteen digits are accumulated, safely within int64.
	for i := fractionalDigits; i < decimals; i++ {
		value *= 10
	}
	return value, nil
}

func formatFixed(value int64, decimals int) string {
	digits := strconv.FormatInt(value, 10)
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals+1-len(digits)) + digits
	}
	return digits[:len(digits)-decimals] + "." + digits[len(digits)-decimals:]
}
