package render_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"invoice.local/system/backend/internal/render"
)

func testAssets(t *testing.T) *render.Assets {
	t.Helper()
	assets, err := render.Load()
	if err != nil {
		t.Fatalf("embedded assets must load: %v", err)
	}
	return assets
}

func sampleInvoice() render.Invoice {
	return render.Invoice{
		RecipientName:   "Tecnología y Distribución del Bajío, S.A. de C.V.",
		ProductCode:     "53131620",
		Quantity:        "5",
		UnitCode:        "DPC",
		UnitDescription: "Días por colocación",
		Description:     "Mampara fija de cristal templado 6 mm, medida 1.20 x 2.10 m, instalación incluida",
		IssuedAt:        "2026-11-04T16:00:00Z",
		UnitPrice:       "100.00",
		Subtotal:        "500.00",
		Tax:             "80.00",
		Total:           "580.00",
		TotalCents:      58000,
	}
}

func TestAmountWords(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{58000, "QUINIENTOS OCHENTA PESOS 00/100 M.N."},
		{50002, "QUINIENTOS PESOS 02/100 M.N."},
		{0, "CERO PESOS 00/100 M.N."},
		{100, "UN PESO 00/100 M.N."},
		{2100, "VEINTIUN PESOS 00/100 M.N."},
		{3105, "TREINTA Y UN PESOS 05/100 M.N."},
		{100000, "MIL PESOS 00/100 M.N."},
		{100100, "MIL UN PESOS 00/100 M.N."},
		{100000000, "UN MILLÓN PESOS 00/100 M.N."},
		{250050099, "DOS MILLONES QUINIENTOS MIL QUINIENTOS PESOS 99/100 M.N."},
		{1600, "DIECISÉIS PESOS 00/100 M.N."},
		{99999999999, "NOVECIENTOS NOVENTA Y NUEVE MILLONES NOVECIENTOS NOVENTA Y NUEVE MIL NOVECIENTOS NOVENTA Y NUEVE PESOS 99/100 M.N."},
	}
	for _, c := range cases {
		if got := render.AmountWords(c.cents); got != c.want {
			t.Errorf("AmountWords(%d) = %q; want %q", c.cents, got, c.want)
		}
	}
}

func TestRenderInvoiceProducesFixedCanvas(t *testing.T) {
	assets := testAssets(t)
	page, err := assets.RenderInvoice(sampleInvoice())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if page.Bounds().Dx() != 1582 || page.Bounds().Dy() != 2048 {
		t.Fatalf("canvas = %v", page.Bounds())
	}
	// A rendered page must contain non-white ink.
	var dark int
	for y := 0; y < 2048; y += 7 {
		for x := 0; x < 1582; x += 7 {
			r, g, b, _ := page.At(x, y).RGBA()
			if r+g+b < 3*0x8000 {
				dark++
			}
		}
	}
	if dark < 100 {
		t.Fatalf("page looks blank: %d dark samples", dark)
	}
}

func TestRenderInvoiceRejectsOverflowingText(t *testing.T) {
	assets := testAssets(t)
	inv := sampleInvoice()
	inv.RecipientName = strings.Repeat("CONSORCIO EMPRESARIAL INTERNACIONAL ", 5)
	if _, err := assets.RenderInvoice(inv); !errors.Is(err, render.ErrTextDoesNotFit) {
		t.Fatalf("long recipient = %v; want ErrTextDoesNotFit", err)
	}
	inv = sampleInvoice()
	inv.Description = strings.Repeat("Descripción detallada del producto con especificaciones completas ", 4)
	if _, err := assets.RenderInvoice(inv); !errors.Is(err, render.ErrTextDoesNotFit) {
		t.Fatalf("long description = %v; want ErrTextDoesNotFit", err)
	}
	inv = sampleInvoice()
	inv.IssuedAt = "not-a-timestamp"
	if _, err := assets.RenderInvoice(inv); !errors.Is(err, render.ErrTextDoesNotFit) {
		t.Fatalf("bad timestamp = %v; want ErrTextDoesNotFit", err)
	}
}

func TestPreviewJPEGIsBoundedAndWatermarked(t *testing.T) {
	assets := testAssets(t)
	inv := sampleInvoice()
	clean, err := assets.RenderInvoice(inv)
	if err != nil {
		t.Fatalf("render clean: %v", err)
	}
	jpegBytes, err := assets.PreviewJPEG(inv, "a1b2c3d4")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(jpegBytes) == 0 || len(jpegBytes) > 2<<20 {
		t.Fatalf("preview bytes = %d; want (0, 2MiB]", len(jpegBytes))
	}
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 1000 {
		t.Fatalf("preview width = %d; want 1000", img.Bounds().Dx())
	}

	// Pixel proof: the watermarked preview must differ from a downscaled clean
	// page in a wide band of pixels, and blended strokes must exist (neither
	// pure white nor the watermark ink) so the legend is fused, not layered.
	// Resample the clean page to preview scale for comparison.
	scaled := image.NewRGBA(img.Bounds())
	nearestScale(scaled, clean)
	var changed, blended, sampled int
	for y := 0; y < img.Bounds().Dy(); y += 3 {
		for x := 0; x < img.Bounds().Dx(); x += 3 {
			sampled++
			want := grayAt(scaled, x, y)
			got := grayAt(img, x, y)
			if got != want {
				changed++
			}
			// Watermark pixels sit strictly between the page white and the ink.
			if want > 245 && got > 130 && got < 240 {
				blended++
			}
		}
	}
	if changed < sampled/20 {
		t.Fatalf("watermark barely visible: %d/%d changed", changed, sampled)
	}
	if blended < 200 {
		t.Fatalf("no blended watermark pixels found: %d", blended)
	}
}

func TestPreviewJPEGDeterministic(t *testing.T) {
	assets := testAssets(t)
	first, err := assets.PreviewJPEG(sampleInvoice(), "a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := assets.PreviewJPEG(sampleInvoice(), "a1b2c3d4")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same input must produce identical preview bytes")
	}
	other, err := assets.PreviewJPEG(sampleInvoice(), "z9y8x7w6")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) {
		t.Fatal("different user identity must change watermark pixels")
	}
}

func TestAssetHashesAreStable(t *testing.T) {
	assets := testAssets(t)
	hashes := assets.AssetHashes()
	if len(hashes) != 4 {
		t.Fatalf("hashes = %v", hashes)
	}
	again, err := render.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, hash := range hashes {
		if again.AssetHashes()[name] != hash {
			t.Fatalf("hash drift for %s", name)
		}
		if len(hash) != 64 {
			t.Fatalf("hash for %s is not sha256 hex: %q", name, hash)
		}
	}
	manifest := assets.ManifestAssets()
	for name := range hashes {
		if !strings.Contains(string(manifest), `"`+name+`"`) {
			t.Fatalf("manifest assets missing %s: %s", name, manifest)
		}
	}
}

func nearestScale(dst *image.RGBA, src *image.RGBA) {
	db := dst.Bounds()
	sb := src.Bounds()
	for y := 0; y < db.Dy(); y++ {
		sy := sb.Min.Y + y*sb.Dy()/db.Dy()
		for x := 0; x < db.Dx(); x++ {
			sx := sb.Min.X + x*sb.Dx()/db.Dx()
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}

func grayAt(img image.Image, x, y int) int {
	r, g, b, _ := img.At(x, y).RGBA()
	return int((r + g + b) / 3 >> 8)
}

var _ = color.RGBA{}
