//go:build ignore

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "test database setup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	raw := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return fmt.Errorf("external test database must use a PostgreSQL URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, raw)
	if err != nil {
		return fmt.Errorf("connect external test database failed (%T)", err)
	}
	defer pool.Close()
	if len(os.Args) == 2 && os.Args[1] == "create" {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		schema := "sk_test_" + hex.EncodeToString(random[:])
		if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return fmt.Errorf("create isolated schema: %w", err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		fmt.Println(schema, parsed.String())
		return nil
	}
	if len(os.Args) == 3 && os.Args[1] == "drop" {
		schema := os.Args[2]
		if len(schema) != len("sk_test_")+16 || schema[:len("sk_test_")] != "sk_test_" {
			return fmt.Errorf("invalid isolated schema name")
		}
		if _, err := hex.DecodeString(schema[len("sk_test_"):]); err != nil {
			return fmt.Errorf("invalid isolated schema name")
		}
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		return err
	}
	return fmt.Errorf("usage: testdb create | drop SCHEMA")
}
