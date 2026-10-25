package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// config reúne as opções lidas do ambiente. Nada de senha em arquivo de configuração:
// o token de acesso da API fica em um arquivo separado, com permissão restrita.
type config struct {
	DataDir     string        // whatsapp.db (sessão), gateway.db (idempotência) e zap.lock
	Listen      string        // endereço da API; padrão só loopback
	TokenFile   string        // arquivo com o token Bearer da API
	MinInterval time.Duration // intervalo mínimo entre envios
	Jitter      time.Duration // folga aleatória somada ao intervalo mínimo
	MaxWait     time.Duration // espera máxima aceita antes de responder 429
	DailyLimit  int           // envios por dia (contados na hora local)
}

func loadConfig() (config, error) {
	cfg := config{
		DataDir:     envOr("ZAP_DATA_DIR", "./data"),
		Listen:      envOr("ZAP_LISTEN", "127.0.0.1:8787"),
		TokenFile:   strings.TrimSpace(os.Getenv("ZAP_TOKEN_FILE")),
		MinInterval: 20 * time.Second,
		Jitter:      10 * time.Second,
		MaxWait:     60 * time.Second,
		DailyLimit:  50,
	}
	var err error
	if cfg.MinInterval, err = durationEnv("ZAP_MIN_INTERVAL", cfg.MinInterval); err != nil {
		return cfg, err
	}
	if cfg.Jitter, err = durationEnv("ZAP_JITTER", cfg.Jitter); err != nil {
		return cfg, err
	}
	if cfg.MaxWait, err = durationEnv("ZAP_MAX_WAIT", cfg.MaxWait); err != nil {
		return cfg, err
	}
	if cfg.DailyLimit, err = intEnv("ZAP_DAILY_LIMIT", cfg.DailyLimit); err != nil {
		return cfg, err
	}
	if cfg.MinInterval < time.Second {
		return cfg, errors.New("ZAP_MIN_INTERVAL deve ser de pelo menos 1s")
	}
	if cfg.Jitter < 0 || cfg.MaxWait <= 0 || cfg.DailyLimit <= 0 {
		return cfg, errors.New("ZAP_JITTER, ZAP_MAX_WAIT e ZAP_DAILY_LIMIT têm valores inválidos")
	}
	return cfg, nil
}

// readToken lê o token da API e exige tamanho mínimo.
func readToken(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("ZAP_TOKEN_FILE não definido")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ler token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return nil, errors.New("o token deve ter pelo menos 32 caracteres")
	}
	return []byte(token), nil
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func intEnv(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}
