package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"settlekit/internal/api"
	"settlekit/internal/indexer"
	"settlekit/internal/store"
	"settlekit/internal/telemetry"
	"settlekit/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("settlekit exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	chainID, err := strconv.ParseUint(os.Getenv("SETTLEKIT_CHAIN_ID"), 10, 64)
	if err != nil || chainID == 0 {
		return errors.New("SETTLEKIT_CHAIN_ID must be a positive integer")
	}
	confirmations, err := confirmationPolicy(chainID, os.Getenv("SETTLEKIT_CONFIRMATIONS"))
	if err != nil {
		return err
	}
	escrow, err := addressEnv("SETTLEKIT_ESCROW_ADDRESS")
	if err != nil {
		return err
	}
	token, err := addressEnv("SETTLEKIT_TOKEN_ADDRESS")
	if err != nil {
		return err
	}
	dsn := os.Getenv("SETTLEKIT_DATABASE_URL")
	if dsn == "" {
		return errors.New("SETTLEKIT_DATABASE_URL is required")
	}
	listen := os.Getenv("SETTLEKIT_LISTEN_ADDRESS")
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := store.Open(startup, dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	if err := store.ApplyMigrations(startup, pool); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	rpcURL := os.Getenv("SETTLEKIT_RPC_URL")
	if rpcURL == "" {
		return errors.New("SETTLEKIT_RPC_URL is required")
	}
	fallbackURL := os.Getenv("SETTLEKIT_RPC_FALLBACK_URL")
	var source *indexer.RPCSource
	if fallbackURL == "" {
		source, err = indexer.DialRPC(startup, rpcURL, escrow)
	} else {
		source, err = indexer.NewRPCSource(rpcURL, escrow)
	}
	if err != nil {
		return err
	}
	defer source.Close()
	var activeSource indexer.Source = source
	if fallbackURL != "" {
		fallback, err := indexer.DialRPC(startup, fallbackURL, escrow)
		if err != nil {
			return fmt.Errorf("connect fallback RPC: %w", err)
		}
		defer fallback.Close()
		activeSource, err = indexer.NewFailover(source, fallback, chainID)
		if err != nil {
			return err
		}
	}
	startBlock := uint64(0)
	if raw := os.Getenv("SETTLEKIT_START_BLOCK"); raw != "" {
		startBlock, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return errors.New("SETTLEKIT_START_BLOCK must be an integer")
		}
	}
	maxReorgDepth := uint64(64)
	if raw := os.Getenv("SETTLEKIT_MAX_REORG_DEPTH"); raw != "" {
		maxReorgDepth, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return errors.New("SETTLEKIT_MAX_REORG_DEPTH must be an integer")
		}
	}
	watcher, err := indexer.New(pool, activeSource, chainID, escrow, confirmations, startBlock, maxReorgDepth)
	if err != nil {
		return err
	}
	dispatcher, err := webhook.New(pool, os.Getenv("SETTLEKIT_WEBHOOK_URL"), []byte(os.Getenv("SETTLEKIT_WEBHOOK_SECRET")))
	if err != nil {
		return err
	}
	metrics := &telemetry.Metrics{}
	dispatcher.SetObserver(func(result, eventID, traceID string) {
		metrics.Webhook(result)
		logger.Info("webhook attempt", "trace_id", traceID, "event_id", eventID, "result", result)
	})
	var lastSuccessfulPoll atomic.Int64
	var chainHealthy atomic.Bool
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var loggedCheckpoint, lastReorg uint64
		for {
			traceID, spanID := telemetry.NewTrace()
			if err := watcher.RunOnce(ctx); err != nil {
				metrics.Poll(false)
				chainHealthy.Store(false)
				logger.Error("chain poll failed", "trace_id", traceID, "span_id", spanID, "error", err)
				if errors.Is(err, indexer.ErrCanonicalDiscontinuity) {
					return
				}
			} else {
				metrics.Poll(true)
				chainHealthy.Store(true)
				lastSuccessfulPoll.Store(time.Now().UnixNano())
				checkpoint, head, reorgs := watcher.Progress()
				lag := uint64(0)
				if head >= checkpoint {
					lag = head - checkpoint
				}
				metrics.Progress(checkpoint, lag)
				if reorgs > lastReorg {
					for n := lastReorg; n < reorgs; n++ {
						metrics.Reorg()
					}
					logger.Warn("canonical reorg reconciled", "trace_id", traceID, "span_id", spanID, "checkpoint", checkpoint)
					lastReorg = reorgs
				}
				if checkpoint != loggedCheckpoint {
					logger.Info("chain checkpoint advanced", "trace_id", traceID, "span_id", spanID, "checkpoint", checkpoint, "lag_blocks", lag)
					loggedCheckpoint = checkpoint
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() {
		stop()
		select {
		case <-watchDone:
		case <-time.After(16 * time.Second):
			logger.Warn("chain watcher did not stop within deadline")
		}
	}()
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			worked, err := dispatcher.RunOnce(ctx)
			if err != nil {
				logger.Error("webhook dispatch failed", "error", err)
			}
			if worked && err == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() {
		stop()
		select {
		case <-dispatchDone:
		case <-time.After(12 * time.Second):
			logger.Warn("webhook dispatcher did not stop within deadline")
		}
	}()
	service, err := api.New(pool, api.Config{
		ChainID: chainID, EscrowAddress: escrow, TokenAddress: token,
		MerchantToken: os.Getenv("SETTLEKIT_MERCHANT_API_TOKEN"),
		OperatorToken: os.Getenv("SETTLEKIT_OPERATOR_API_TOKEN"),
		Metrics:       metrics,
		Logger:        logger,
		Ready: func(ctx context.Context) error {
			if !chainHealthy.Load() || time.Since(time.Unix(0, lastSuccessfulPoll.Load())) > 10*time.Second {
				return errors.New("chain poll is stale")
			}
			checkpoint, head, _ := watcher.Progress()
			if head > checkpoint {
				return errors.New("chain index is behind RPC head")
			}
			return pool.Ping(ctx)
		},
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: listen, Handler: service.Handler(),
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 7 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 8192,
	}
	rawListener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen API: %w", err)
	}
	listener := newBoundedListener(rawListener, maxHTTPConnections)
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	logger.Info("settlekit API listening", "address", listen, "chain_id", chainID)
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve API: %w", err)
		}
	case <-ctx.Done():
		shutdown, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown API: %w", err)
		}
	}
	return nil
}

func addressEnv(name string) (common.Address, error) {
	value := os.Getenv(name)
	if !common.IsHexAddress(value) || common.HexToAddress(value) == (common.Address{}) {
		return common.Address{}, fmt.Errorf("%s must be a nonzero EVM address", name)
	}
	return common.HexToAddress(value), nil
}

func confirmationPolicy(chainID uint64, raw string) (uint64, error) {
	if raw == "" && chainID == 31337 {
		return 2, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value < 2 || value > 128 {
		return 0, errors.New("SETTLEKIT_CONFIRMATIONS must be explicitly configured from 2 to 128 outside Anvil (31337)")
	}
	return value, nil
}
