package config

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"invoice.local/system/backend/internal/strictjson"
)

var ErrInvalid = errors.New("invalid application configuration")
var hostname = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type Config struct {
	Environment       string   `json:"environment"`
	ListenAddr        string   `json:"listen_addr"`
	PublicOrigin      string   `json:"public_origin"`
	DatabaseDSNFile   string   `json:"database_dsn_file"`
	KeyringFile       string   `json:"keyring_file"`
	TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs"`
	TLSCertFile       string   `json:"tls_cert_file"`
	TLSKeyFile        string   `json:"tls_key_file"`
	WebDir            string   `json:"web_dir"`
}

func Parse(r io.Reader) (Config, error) {
	var c Config
	if err := strictjson.Decode(r, &c, 64<<10); err != nil {
		return Config{}, ErrInvalid
	}
	if c.Environment != "development" && c.Environment != "production" {
		return Config{}, ErrInvalid
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return Config{}, ErrInvalid
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil || !validPort(port) {
		return Config{}, ErrInvalid
	}
	ip := net.ParseIP(host)
	if ip == nil || (c.Environment == "development" && !ip.IsLoopback()) {
		return Config{}, ErrInvalid
	}
	u, err := url.Parse(c.PublicOrigin)
	if err != nil || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Host == "" {
		return Config{}, ErrInvalid
	}
	name := u.Hostname()
	if net.ParseIP(name) == nil && !hostname.MatchString(name) {
		return Config{}, ErrInvalid
	}
	if strings.HasSuffix(u.Host, ":") || (u.Port() != "" && !validPort(u.Port())) {
		return Config{}, ErrInvalid
	}
	if u.Scheme != "https" {
		originIP := net.ParseIP(name)
		if c.Environment != "development" || u.Scheme != "http" || (name != "localhost" && (originIP == nil || !originIP.IsLoopback())) {
			return Config{}, ErrInvalid
		}
	}
	for _, raw := range c.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 {
			return Config{}, ErrInvalid
		}
	}
	if c.Environment == "production" && (c.DatabaseDSNFile == "" || c.KeyringFile == "" || len(c.TrustedProxyCIDRs) == 0) {
		return Config{}, ErrInvalid
	}
	return c, nil
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, ErrInvalid
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		return Config{}, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, ErrInvalid
	}
	for _, value := range []*string{&c.DatabaseDSNFile, &c.KeyringFile, &c.TLSCertFile, &c.TLSKeyFile, &c.WebDir} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
	}
	if c.Environment == "production" {
		for _, path := range []string{c.DatabaseDSNFile, c.KeyringFile} {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0007 != 0 {
				return Config{}, ErrInvalid
			}
		}
	}
	return c, nil
}

func validPort(raw string) bool {
	n, err := strconv.Atoi(raw)
	return err == nil && n > 0 && n <= 65535
}
