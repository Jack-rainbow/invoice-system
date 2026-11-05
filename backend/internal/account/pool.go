package account

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"os"
	"strings"
)

func OpenPool(ctx context.Context, path string, production bool) (*pgxpool.Pool, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrInvalid
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	raw, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil || len(raw) > 16384 {
		return nil, ErrInvalid
	}
	defer clear(raw)
	dsn := strings.TrimSpace(string(raw))
	if dsn == "" {
		return nil, ErrInvalid
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, ErrInvalid
	}
	if production && !strings.HasPrefix(cfg.ConnConfig.Host, "/") && (cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.InsecureSkipVerify || len(cfg.ConnConfig.Fallbacks) != 0) {
		return nil, ErrInvalid
	}
	cfg.MaxConns = 16
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["search_path"] = "public,pg_catalog"
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return nil, ErrInvalid
	}
	var version int
	if e = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); e != nil || version < 170000 {
		pool.Close()
		return nil, ErrInvalid
	}
	return pool, nil
}
