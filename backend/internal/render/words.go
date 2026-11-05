package render

import (
	"fmt"
	"strings"
)

// Spanish amount-in-words for the es-MX invoice line "SON: (… PESOS CC/100
// M.N.)". The rules are fixed by the template version; the algorithm version
// is part of the render profile, not of business content.

var units = [...]string{
	"", "UNO", "DOS", "TRES", "CUATRO", "CINCO", "SEIS", "SIETE", "OCHO", "NUEVE",
	"DIEZ", "ONCE", "DOCE", "TRECE", "CATORCE", "QUINCE", "DIECISÉIS", "DIECISIETE", "DIECIOCHO", "DIECINUEVE", "VEINTE",
}

var tens = [...]string{
	"", "", "VEINTI", "TREINTA", "CUARENTA", "CINCUENTA", "SESENTA", "SETENTA", "OCHENTA", "NOVENTA",
}

var hundreds = [...]string{
	"", "CIENTO", "DOSCIENTOS", "TRESCIENTOS", "CUATROCIENTOS", "QUINIENTOS",
	"SEISCIENTOS", "SETECIENTOS", "OCHOCIENTOS", "NOVECIENTOS",
}

func twoDigits(n int64) string {
	switch {
	case n <= 20:
		return units[n]
	case n < 30:
		if n == 21 {
			return "VEINTIUNO"
		}
		return tens[2] + "I" + units[n-20]
	default:
		rest := n % 10
		if rest == 0 {
			return tens[n/10]
		}
		return tens[n/10] + " Y " + units[rest]
	}
}

func threeDigits(n int64) string {
	switch {
	case n == 0:
		return ""
	case n < 100:
		return twoDigits(n)
	case n == 100:
		return "CIEN"
	default:
		rest := n % 100
		if rest == 0 {
			return hundreds[n/100]
		}
		return hundreds[n/100] + " " + twoDigits(rest)
	}
}

// group renders a thousands group with its scale word. thousands==1 is MIL,
// millions use MILLÓN/MILLONES with the masculine UNO kept before the noun.
func group(n, scale int64) string {
	if n == 0 {
		return ""
	}
	var text string
	switch scale {
	case 1:
		if n == 1 {
			return "MIL"
		}
		text = threeDigits(n)
	case 2:
		if n == 1 {
			return "UN MILLÓN"
		}
		text = threeDigits(n)
	default:
		return ""
	}
	if scale == 2 {
		return text + " MILLONES"
	}
	return text + " MIL"
}

// integerWords renders 0..999999999999 in uppercase Spanish (es-MX usage:
// VEINTIUNO, MIL, MILLONES). The invoice total limit keeps values inside
// this range.
func integerWords(n int64) string {
	if n == 0 {
		return "CERO"
	}
	parts := []string{}
	if g := group(n/1000000, 2); g != "" {
		parts = append(parts, g)
	}
	if g := group((n/1000)%1000, 1); g != "" {
		parts = append(parts, g)
	}
	if g := threeDigits(n % 1000); g != "" {
		parts = append(parts, g)
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}

// AmountWords renders the fixed es-MX legal line for a non-negative cent
// amount, e.g. 58000 -> "QUINIENTOS OCHENTA PESOS 00/100 M.N.". Numbers
// ending in uno apocopate before PESOS: 21 -> "VEINTIUN PESOS 00/100 M.N.".
func AmountWords(totalCents int64) string {
	if totalCents < 0 {
		return ""
	}
	pesos := totalCents / 100
	cents := totalCents % 100
	if pesos == 1 {
		return fmt.Sprintf("UN PESO %02d/100 M.N.", cents)
	}
	words := integerWords(pesos)
	if pesos%10 == 1 && pesos%100 != 11 {
		switch {
		case strings.HasSuffix(words, "VEINTIUNO"):
			words = strings.TrimSuffix(words, "VEINTIUNO") + "VEINTIUN"
		case strings.HasSuffix(words, "UNO"):
			words = strings.TrimSuffix(words, "UNO") + "UN"
		}
	}
	return fmt.Sprintf("%s PESOS %02d/100 M.N.", words, cents)
}
