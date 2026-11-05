package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"

	"invoice.local/system/backend/internal/render"
)

// The raster profile is registered by SQL, but the renderer that must honour it
// is compiled Go code. These tests read the real migration so a manifest that
// drifts from the embedded assets, the watermark style, or its own recorded
// hash fails here instead of at the first user preview.
const profileMigration = "../../db/migrations/014_cfdi_template.sql"

var (
	manifestLiteral = regexp.MustCompile(`'(\{"assets".*?\})'::jsonb`)
	hashLiteral     = regexp.MustCompile(`'([0-9a-f]{64})'`)
)

func readProfileManifest(t *testing.T) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(profileMigration)
	if err != nil {
		t.Fatalf("read %s: %v", profileMigration, err)
	}
	sql := string(raw)
	manifest := manifestLiteral.FindStringSubmatch(sql)
	if manifest == nil {
		t.Fatalf("%s has no jsonb manifest literal", profileMigration)
	}
	hash := hashLiteral.FindStringSubmatch(sql)
	if hash == nil {
		t.Fatalf("%s has no manifest hash literal", profileMigration)
	}
	return manifest[1], hash[1]
}

func newAssetService(t *testing.T) *Service {
	t.Helper()
	assets, err := render.Load()
	if err != nil {
		t.Fatalf("load assets: %v", err)
	}
	return &Service{assets: assets}
}

func TestRegisteredProfileMatchesCompiledRenderer(t *testing.T) {
	manifest, recordedHash := readProfileManifest(t)
	service := newAssetService(t)

	if !service.assetsMatch([]byte(manifest)) {
		t.Fatalf("profile manifest does not match the compiled assets, protocol or watermark")
	}

	sum := sha256.Sum256([]byte(manifest))
	if got := hex.EncodeToString(sum[:]); got != recordedHash {
		t.Fatalf("manifest_hash mismatch: migration records %s, manifest hashes to %s", recordedHash, got)
	}
}

func TestProfileWithForeignWatermarkIsRefused(t *testing.T) {
	manifest, _ := readProfileManifest(t)
	service := newAssetService(t)

	for name, mutate := range map[string][2]string{
		"style version": {`"style_version":"preview-watermark/1"`, `"style_version":"preview-watermark/2"`},
		"alpha":         {`"alpha":0.32`, `"alpha":0.12`},
		"angle":         {`"angle_degrees":30`, `"angle_degrees":0`},
		"width":         {`"width_px":1000`, `"width_px":1400`},
		"byte limit":    {`"max_bytes":2097152`, `"max_bytes":8388608`},
		"legend text":   {`"text":"预览 未正式生成"`, `"text":"草稿"`},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(manifest, mutate[0]) {
				t.Fatalf("migration manifest no longer contains %s; update this test", mutate[0])
			}
			changed := strings.Replace(manifest, mutate[0], mutate[1], 1)
			if service.assetsMatch([]byte(changed)) {
				t.Fatalf("profile with a different %s was accepted", name)
			}
		})
	}
}

func TestProfileWithoutWatermarkIsRefused(t *testing.T) {
	service := newAssetService(t)
	manifest := `{"assets":{"bundle":"invoice-mxn-v1","sha256":{}},"renderer_protocol":"` +
		service.assets.Template.RendererProtocol + `"}`
	if service.assetsMatch([]byte(manifest)) {
		t.Fatal("profile without a watermark block was accepted")
	}
}
