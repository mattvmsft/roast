// Command roast-broker fronts an S3-compatible bucket with authenticated,
// content-bound presigned uploads for Roast run bundles.
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
	"syscall"
	"time"

	"github.com/bluesky-social/roast/internal/broker"
	"github.com/bluesky-social/roast/internal/config"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			slog.Error("roast broker healthcheck failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("roast broker stopped", "error", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	address := os.Getenv("ROAST_BROKER_LISTEN_ADDRESS")
	if address == "" {
		address = ":8080"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse ROAST_BROKER_LISTEN_ADDRESS: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}

func run() error {
	maxBytes, err := envInt64("ROAST_BROKER_MAX_BUNDLE_BYTES", 128<<20)
	if err != nil {
		return err
	}
	pathStyle, err := envBool("ROAST_BROKER_S3_PATH_STYLE", false)
	if err != nil {
		return err
	}
	store, err := broker.NewS3Store(broker.S3Config{
		Endpoint: os.Getenv("ROAST_BROKER_S3_ENDPOINT"), Bucket: os.Getenv("ROAST_BROKER_S3_BUCKET"),
		Region: os.Getenv("ROAST_BROKER_S3_REGION"), AccessKey: os.Getenv("ROAST_BROKER_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("ROAST_BROKER_S3_SECRET_KEY"), SessionToken: os.Getenv("ROAST_BROKER_S3_SESSION_TOKEN"),
		PathStyle: pathStyle,
	})
	if err != nil {
		return err
	}
	var clientConfig []byte
	if path := os.Getenv("ROAST_BROKER_CLIENT_CONFIG_FILE"); path != "" {
		clientConfig, err = os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := config.ValidateProvisionDocument(clientConfig); err != nil {
			return err
		}
	}
	service := &broker.Server{
		Store: store, SigningKey: []byte(os.Getenv("ROAST_BROKER_SIGNING_KEY")),
		PrincipalHeader: os.Getenv("ROAST_BROKER_PRINCIPAL_HEADER"),
		Prefix:          os.Getenv("ROAST_BROKER_S3_PREFIX"), MaxBundleBytes: maxBytes,
		ClientConfig: clientConfig,
	}
	handler, err := service.Handler()
	if err != nil {
		return err
	}
	address := os.Getenv("ROAST_BROKER_LISTEN_ADDRESS")
	if address == "" {
		address = ":8080"
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("roast broker listening", "address", address)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func envInt64(name string, fallback int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	return value, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New(name + " must be true or false")
	}
	return value, nil
}
