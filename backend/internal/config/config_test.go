package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevelopmentOriginRequiresLoopback(t *testing.T) {
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:8080", "https://invoice.example.com"} {
		if _, err := Parse(strings.NewReader(`{"environment":"development","listen_addr":"127.0.0.1:8080","public_origin":"` + origin + `"}`)); err != nil {
			t.Fatalf("valid development config %s: %v", origin, err)
		}
	}
	for _, origin := range []string{"http://invoice.example.com", "https://example.com/path", "https://user:pass@example.com", "https://example.com?x=1", "https://example.com#x", "javascript:alert(1)", "https://example.com/", "https://example.com:99999", "https://*.example.com"} {
		if _, err := Parse(strings.NewReader(`{"environment":"development","listen_addr":"127.0.0.1:8080","public_origin":"` + origin + `"}`)); err == nil {
			t.Fatalf("unsafe origin accepted: %s", origin)
		}
	}
}

func TestLoadProductionRejectsMissingOrPublicSecretFiles(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "db-secret")
	keyFile := filepath.Join(dir, "key-secret")
	cfg := Config{Environment: "production", ListenAddr: "127.0.0.1:8080", PublicOrigin: "https://invoice.example.com", DatabaseDSNFile: dbFile, KeyringFile: keyFile, TrustedProxyCIDRs: []string{"127.0.0.1/32"}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configFile); err == nil {
		t.Fatal("missing secret files accepted")
	}
	for _, p := range []string{dbFile, keyFile} {
		if err := os.WriteFile(p, []byte("test fixture only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(configFile); err != nil {
		t.Fatalf("private regular secret files rejected: %v", err)
	}
	if err := os.Chmod(dbFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configFile); err == nil {
		t.Fatal("world-readable secret accepted")
	}
}

func TestProductionRequiresSecretsAndTrustedProxy(t *testing.T) {
	valid := `{"environment":"production","listen_addr":"127.0.0.1:8080","public_origin":"https://invoice.example.com","database_dsn_file":"/run/secrets/db","keyring_file":"/run/secrets/keyring","trusted_proxy_cidrs":["127.0.0.1/32"]}`
	if _, err := Parse(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		strings.Replace(valid, `"https://invoice.example.com"`, `"http://localhost:8080"`, 1),
		strings.Replace(valid, `"/run/secrets/db"`, `""`, 1),
		strings.Replace(valid, `"/run/secrets/keyring"`, `""`, 1),
		strings.Replace(valid, `["127.0.0.1/32"]`, `[]`, 1),
		strings.Replace(valid, `["127.0.0.1/32"]`, `["0.0.0.0/0"]`, 1),
		strings.Replace(valid, `["127.0.0.1/32"]`, `["not-a-network"]`, 1),
	} {
		if _, err := Parse(strings.NewReader(value)); err == nil {
			t.Fatal("unsafe production config accepted")
		}
	}
}

func TestRejectUnknownDuplicateTrailingAndOversizedConfig(t *testing.T) {
	values := []string{
		`{"environment":"development","public_origin":"http://localhost","disable_auth":true}`,
		`{"environment":"production","environment":"development","public_origin":"http://localhost"}`,
		`{"environment":"development","public_origin":"http://localhost"} {}`,
		`{"environment":"unknown","public_origin":"https://example.com"}`,
		`{"environment":"development","public_origin":"http://localhost","listen_addr":"example.com:8080"}`,
		`{"environment":"development","public_origin":"http://localhost","listen_addr":"0.0.0.0:8080"}`,
		strings.Repeat(" ", 65537),
	}
	for _, value := range values {
		if _, err := Parse(strings.NewReader(value)); err == nil {
			t.Fatalf("invalid config accepted: %.100s", value)
		}
	}
}

func TestLoadPathsRelativeToConfigFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if e := os.WriteFile(file, []byte(`{"environment":"development","public_origin":"https://localhost:8443","database_dsn_file":"private/db","keyring_file":"private/keys","tls_cert_file":"cert.pem","tls_key_file":"key.pem","web_dir":"../dist"}`), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(file)
	if e != nil {
		t.Fatal(e)
	}
	if c.DatabaseDSNFile != filepath.Join(dir, "private/db") || c.KeyringFile != filepath.Join(dir, "private/keys") || c.TLSCertFile != filepath.Join(dir, "cert.pem") || c.TLSKeyFile != filepath.Join(dir, "key.pem") || c.WebDir != filepath.Clean(filepath.Join(dir, "../dist")) {
		t.Fatal("config paths depend on shell working directory")
	}
}
