package money_test

import (
	"errors"
	"strings"
	"testing"

	"invoice.local/system/backend/internal/money"
)

func TestCalculate(t *testing.T) {
	tests := []struct {
		name, price, quantity            string
		unit, qty, subtotal, tax, total  int64
		subtotalText, taxText, totalText string
	}{
		{"whole", "100", "5", 10000, 5000000, 50000, 8000, 58000, "500.00", "80.00", "580.00"},
		{"tax rounding", "86.21", "5", 8621, 5000000, 43105, 6897, 50002, "431.05", "68.97", "500.02"},
		{"decimal exactness", "0.10", "3", 10, 3000000, 30, 5, 35, "0.30", "0.05", "0.35"},
		{"half cent up", "0.05", "0.1", 5, 100000, 1, 0, 1, "0.01", "0.00", "0.01"},
		{"below half cent", "0.05", "0.099999", 5, 99999, 0, 0, 0, "0.00", "0.00", "0.00"},
		{"zero", "0", "0", 0, 0, 0, 0, 0, "0.00", "0.00", "0.00"},
		{"max price zero quantity", "999999999.99", "0", 99999999999, 0, 0, 0, 0, "0.00", "0.00", "0.00"},
		{"max quantity zero price", "0", "999999999.999999", 0, 999999999999999, 0, 0, 0, "0.00", "0.00", "0.00"},
		{"total upper bound", "862068965.51", "1", 86206896551, 1000000, 86206896551, 13793103448, 99999999999, "862068965.51", "137931034.48", "999999999.99"},
		{"leading zeros", "000000001.20", "000000002.500000", 120, 2500000, 300, 48, 348, "3.00", "0.48", "3.48"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := money.Calculate(tt.price, tt.quantity)
			if err != nil {
				t.Fatal(err)
			}
			if got.UnitCents != tt.unit || got.QuantityMicros != tt.qty || got.SubtotalCents != tt.subtotal || got.TaxCents != tt.tax || got.TotalCents != tt.total {
				t.Fatalf("unexpected integer snapshot: %+v", got)
			}
			if got.Subtotal != tt.subtotalText || got.Tax != tt.taxText || got.Total != tt.totalText {
				t.Fatalf("unexpected display amounts: %+v", got)
			}
		})
	}
}

func TestCalculateRejectsInvalidInputs(t *testing.T) {
	invalid := []string{"", "-1", "+1", "1e2", "NaN", "Infinity", "１", "١", " 1", "1 ", "1\n", "1\t", "1,000", "$1", ".1", "1.", "1.2.3", "1000000000", "0000000000", strings.Repeat("1", 10000)}
	for _, value := range invalid {
		for _, quantity := range []bool{false, true} {
			price, qty := value, "1"
			if quantity {
				price, qty = "1", value
			}
			_, err := money.Calculate(price, qty)
			if !errors.Is(err, money.ErrInvalidAmount) {
				t.Errorf("Calculate(%q, %q) error = %v; want ErrInvalidAmount", price, qty, err)
			}
		}
	}
	for _, pair := range [][2]string{{"1.001", "1"}, {"1", "1.0000001"}} {
		if _, err := money.Calculate(pair[0], pair[1]); !errors.Is(err, money.ErrInvalidAmount) {
			t.Errorf("excess decimals: %v", err)
		}
	}
}

func TestCalculateRejectsAmountLimitWithoutOverflow(t *testing.T) {
	for _, pair := range [][2]string{{"862068965.52", "1"}, {"999999999.99", "999999999.999999"}, {"999999999.99", "100000000"}} {
		got, err := money.Calculate(pair[0], pair[1])
		if !errors.Is(err, money.ErrAmountLimit) {
			t.Fatalf("Calculate(%q, %q) = %+v, %v; want ErrAmountLimit", pair[0], pair[1], got, err)
		}
		if got != (money.Totals{}) {
			t.Fatalf("error returned partial usable totals: %+v", got)
		}
	}
}

func TestCanonicalInputs(t *testing.T) {
	for _, tt := range []struct{ price, qty, canonicalPrice, canonicalQty string }{
		{"000000001.2", "000000002.500000", "1.20", "2.5"},
		{"0", "0.000000", "0.00", "0"},
		{"12", "10", "12.00", "10"},
	} {
		got, err := money.Calculate(tt.price, tt.qty)
		if err != nil {
			t.Fatal(err)
		}
		if got.CanonicalUnitPrice != tt.canonicalPrice || got.CanonicalQuantity != tt.canonicalQty {
			t.Fatalf("canonical inputs: %+v", got)
		}
	}
}
