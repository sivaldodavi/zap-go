package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	errNotPaired = errors.New("dispositivo não pareado: rode 'zap pair' primeiro")
	reconnecting atomic.Bool
)

// acquireLock impede duas instâncias usando a mesma sessão. O flock some ao fechar o arquivo.
func acquireLock(dataDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, "zap.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("outra instância do zap já está usando esta sessão")
	}
	return f, nil
}

func openClient(dataDir string) (*whatsmeow.Client, error) {
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(dataDir, "whatsapp.db"))
	container, err := sqlstore.New(context.Background(), "sqlite3", dbURI, waLog.Stdout("Database", "ERROR", true))
	if err != nil {
		return nil, fmt.Errorf("abrir sessão: %w", err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("carregar dispositivo: %w", err)
	}
	return whatsmeow.NewClient(device, waLog.Stdout("Client", "ERROR", true)), nil
}

// pair exibe o QR code e espera o pareamento terminar.
func pair(client *whatsmeow.Client) error {
	if client.Store.ID != nil {
		return errors.New("este gateway já está pareado")
	}
	qrChan, err := client.GetQRChannel(context.Background())
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		return err
	}
	defer client.Disconnect()
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		case "success":
			slog.Info("pareamento concluído")
		default:
			slog.Info("evento de pareamento", "evento", evt.Event)
		}
	}
	if client.Store.ID == nil {
		return errors.New("pareamento não concluído")
	}
	return nil
}

func connect(client *whatsmeow.Client) error {
	if client.Store.ID == nil {
		return errNotPaired
	}
	return client.Connect()
}

// installReconnect reconecta em segundo plano. O handler não bloqueia o laço de eventos.
func installReconnect(client *whatsmeow.Client) {
	client.AddEventHandler(func(evt interface{}) {
		switch evt.(type) {
		case *events.Disconnected:
			slog.Warn("desconectado do WhatsApp; tentando reconectar")
			go reconnectLoop(client)
		case *events.LoggedOut:
			slog.Error("sessão encerrada pelo WhatsApp; rode 'zap pair' de novo")
		}
	})
}

func reconnectLoop(client *whatsmeow.Client) {
	if !reconnecting.CompareAndSwap(false, true) {
		return
	}
	defer reconnecting.Store(false)
	backoff := 5 * time.Second
	for {
		time.Sleep(backoff + randDuration(3*time.Second))
		if client.IsConnected() {
			return
		}
		if err := client.Connect(); err == nil {
			slog.Info("reconectado ao WhatsApp")
			return
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}
