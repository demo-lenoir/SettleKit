package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const connectTimeout = 5 * time.Second

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration failed (%T)", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	bounded, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(bounded, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := pool.Ping(bounded); err != nil {
		pool.Close()
		if bounded.Err() != nil {
			return nil, fmt.Errorf("ping database: %w", bounded.Err())
		}
		return nil, fmt.Errorf("ping database failed (%T)", err)
	}
	return pool, nil
}
