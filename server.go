package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	maxBodyBytes = 16 << 10
	maxTextRunes = 1000
	sendTimeout  = 30 * time.Second
)

var (
	phonePattern = regexp.MustCompile(`^[1-9][0-9]{7,14}$`)
	keyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
)

type server struct {
	client *whatsmeow.Client
	gw     *gateway
	token  []byte
}

func newServer(client *whatsmeow.Client, gw *gateway, token []byte) *server {
	return &server{client: client, gw: gw, token: token}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.authorized(s.handleSend))
	mux.HandleFunc("GET /v1/health", s.authorized(s.handleHealth))
	return mux
}

func (s *server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), s.token) != 1 {
			writeError(w, http.StatusUnauthorized, "nao_autorizado", "token ausente ou inválido")
			return
		}
		next(w, r)
	}
}

type sendRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
	Key  string `json:"idempotency_key"`
}

type sendResponse struct {
	MessageID string `json:"message_id,omitempty"`
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req sendRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "requisicao_invalida", "JSON inválido ou com campos desconhecidos")
		return
	}
	if err := validateSend(req); err != nil {
		writeError(w, http.StatusBadRequest, "validacao", err.Error())
		return
	}
	if !s.client.IsConnected() {
		writeError(w, http.StatusServiceUnavailable, "desconectado", "gateway sem conexão com o WhatsApp")
		return
	}

	res, err := s.gw.admit(req.Key)
	if err != nil {
		slog.Error("falha ao reservar envio", "erro", err)
		writeError(w, http.StatusInternalServerError, "interno", "falha ao registrar o envio")
		return
	}
	switch res.Status {
	case alreadySent:
		writeJSON(w, http.StatusOK, sendResponse{MessageID: res.Existing.MessageID, State: stateSent, Duplicate: true})
		return
	case inDoubt:
		writeError(w, http.StatusConflict, "estado_incerto",
			"já existe envio com esta chave sem confirmação; não reenviar automaticamente")
		return
	case throttled:
		setRetryAfter(w, res.Wait)
		writeError(w, http.StatusTooManyRequests, "limite_de_ritmo", "aguarde antes de enviar de novo")
		return
	case dailyCapped:
		writeError(w, http.StatusTooManyRequests, "limite_diario", "limite diário de envios atingido")
		return
	}

	if err := sleepCtx(r.Context(), res.Wait); err != nil {
		if ferr := s.gw.fail(req.Key); ferr != nil {
			slog.Error("falha ao liberar chave", "erro", ferr)
		}
		writeError(w, http.StatusServiceUnavailable, "cancelado", "requisição cancelada antes do envio")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	id, err := s.send(ctx, req.To, req.Text)
	if err != nil {
		// Pode ter sido entregue mesmo com erro; por isso a chave não volta a ficar livre.
		if uerr := s.gw.markUncertain(req.Key); uerr != nil {
			slog.Error("falha ao marcar envio incerto", "erro", uerr)
		}
		slog.Error("envio não confirmado", "destino", maskPhone(req.To), "chave", req.Key, "erro", err)
		writeError(w, http.StatusBadGateway, "falha_envio", "o WhatsApp não confirmou o envio")
		return
	}
	if err := s.gw.complete(req.Key, id); err != nil {
		slog.Error("falha ao registrar envio", "erro", err)
	}
	slog.Info("mensagem enviada", "destino", maskPhone(req.To), "chave", req.Key)
	writeJSON(w, http.StatusOK, sendResponse{MessageID: id, State: stateSent})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	sent, err := s.gw.sentToday()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "interno", "falha ao consultar estado")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":   s.client.IsConnected(),
		"paired":      s.client.Store.ID != nil,
		"sent_today":  sent,
		"daily_limit": s.gw.dailyLimit,
	})
}

func (s *server) send(ctx context.Context, to, text string) (string, error) {
	jid, err := types.ParseJID(to + "@s.whatsapp.net")
	if err != nil {
		return "", err
	}
	resp, err := s.client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return "", err
	}
	return string(resp.ID), nil
}

func validateSend(req sendRequest) error {
	if !phonePattern.MatchString(req.To) {
		return errors.New("destino deve ser DDI+DDD+número, só dígitos (ex.: 5598999999999)")
	}
	if strings.TrimSpace(req.Text) == "" {
		return errors.New("texto vazio")
	}
	if utf8.RuneCountInString(req.Text) > maxTextRunes {
		return errors.New("texto acima de 1000 caracteres")
	}
	if !keyPattern.MatchString(req.Key) {
		return errors.New("idempotency_key deve ter de 8 a 128 caracteres: letras, números, _ ou -")
	}
	return nil
}

func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maskPhone deixa visíveis só os quatro últimos dígitos, para logs.
func maskPhone(num string) string {
	if len(num) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(num)-4) + num[len(num)-4:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Code: code, Message: message})
}
