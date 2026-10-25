// zap é um gateway de envio de mensagens WhatsApp. Não recebe nem encaminha mensagens.
//
// Comandos:
//
//	zap pair   parear o número do gateway (exibe QR code)
//	zap serve  iniciar a API HTTP de envio
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "pair":
		err = runPair(cfg)
	case "serve":
		err = runServe(cfg)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "uso: zap <pair|serve>")
	fmt.Fprintln(os.Stderr, "  pair   parear o número do gateway (exibe QR code)")
	fmt.Fprintln(os.Stderr, "  serve  iniciar a API HTTP de envio")
}

func fail(err error) {
	slog.Error("falha", "detalhe", err)
	os.Exit(1)
}

func runPair(cfg config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	lock, err := acquireLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	client, err := openClient(cfg.DataDir)
	if err != nil {
		return err
	}
	return pair(client)
}

func runServe(cfg config) error {
	token, err := readToken(cfg.TokenFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	lock, err := acquireLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	store, err := openDeliveries(filepath.Join(cfg.DataDir, "gateway.db"))
	if err != nil {
		return err
	}
	defer store.close()
	if err := store.prune(time.Now()); err != nil {
		return err
	}
	gw := newGateway(cfg, store)

	client, err := openClient(cfg.DataDir)
	if err != nil {
		return err
	}
	installReconnect(client)
	if err := connect(client); err != nil {
		return err
	}
	defer client.Disconnect()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newServer(client, gw, token).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("gateway pronto", "escuta", cfg.Listen)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		return err
	case <-quit:
		slog.Info("encerrando")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
