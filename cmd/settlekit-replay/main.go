package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"settlekit/internal/payments"
	"settlekit/internal/store"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, store.ErrOutboxNotDead) {
			fmt.Fprintln(os.Stderr, "event is not dead-lettered")
		} else {
			fmt.Fprintln(os.Stderr, "replay failed:", err)
		}
		os.Exit(1)
	}
}

func run() error {
	eventID := flag.String("event-id", "", "UUID of a dead-lettered webhook event")
	flag.Parse()
	if _, err := payments.ParseID(*eventID); err != nil {
		return errors.New("valid --event-id is required")
	}
	dsn := os.Getenv("SETTLEKIT_DATABASE_URL")
	if dsn == "" {
		return errors.New("SETTLEKIT_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		return err
	}
	if err := store.RequeueDeadLetter(ctx, pool, *eventID); err != nil {
		return err
	}
	fmt.Println("requeued", *eventID)
	return nil
}
