package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestValidateSend(t *testing.T) {
	valid := sendRequest{To: "5598999999999", Text: "Lembrete", Key: "entrega-42-debate-2026"}
	if err := validateSend(valid); err != nil {
		t.Fatalf("esperava válido, veio erro: %v", err)
	}
	casos := map[string]sendRequest{
		"destino com sinal de mais": {To: "+5598999999999", Text: "x", Key: valid.Key},
		"destino curto":             {To: "123", Text: "x", Key: valid.Key},
		"destino com letras":        {To: "55989abc9999", Text: "x", Key: valid.Key},
		"texto vazio":               {To: valid.To, Text: "   ", Key: valid.Key},
		"chave curta":               {To: valid.To, Text: "x", Key: "abc"},
		"chave com espaço":          {To: valid.To, Text: "x", Key: "chave com espaco"},
	}
	for nome, req := range casos {
		if err := validateSend(req); err == nil {
			t.Errorf("%s: esperava erro de validação", nome)
		}
	}
}

func TestPacerEspacaEnviosERecusaEsperaLonga(t *testing.T) {
	inicio := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := &pacer{
		minGap:  10 * time.Second,
		jitter:  0,
		maxWait: 30 * time.Second,
		now:     func() time.Time { return inicio },
		rnd:     func(time.Duration) time.Duration { return 0 },
	}
	esperado := []time.Duration{0, 10 * time.Second, 20 * time.Second, 30 * time.Second}
	for i, want := range esperado {
		got, ok := p.reserve()
		if !ok || got != want {
			t.Fatalf("envio %d: esperava espera %v, veio %v (ok=%v)", i+1, want, got, ok)
		}
	}
	if espera, ok := p.reserve(); ok || espera != 40*time.Second {
		t.Fatalf("quinto envio deveria ser recusado com espera de 40s, veio %v (ok=%v)", espera, ok)
	}
}

func TestPacerNaoReservaQuandoRecusa(t *testing.T) {
	inicio := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := &pacer{minGap: 10 * time.Second, maxWait: 5 * time.Second,
		now: func() time.Time { return inicio }, rnd: func(time.Duration) time.Duration { return 0 }}
	p.next = inicio.Add(time.Minute)
	if _, ok := p.reserve(); ok {
		t.Fatal("espera de 60s deveria ser recusada com maxWait de 5s")
	}
	if !p.next.Equal(inicio.Add(time.Minute)) {
		t.Fatal("recusa não deve alterar o próximo horário reservado")
	}
}

func openTestStore(t *testing.T) *deliveries {
	t.Helper()
	store, err := openDeliveries(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("abrir store: %v", err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func TestClaimEIdempotenciaDeEnvioConcluido(t *testing.T) {
	store := openTestStore(t)
	agora := time.Now()
	const chave = "chave-aaaaaaaa"

	cur, reservou, err := store.claim(chave, agora)
	if err != nil || !reservou || cur.State != stateReserved {
		t.Fatalf("primeira reserva: estado=%+v reservou=%v err=%v", cur, reservou, err)
	}
	cur, reservou, err = store.claim(chave, agora)
	if err != nil || reservou || cur.State != stateReserved {
		t.Fatalf("segunda reserva deveria ver pendente: estado=%+v reservou=%v err=%v", cur, reservou, err)
	}
	if err := store.finish(chave, stateSent, "MSG-1"); err != nil {
		t.Fatal(err)
	}
	cur, reservou, err = store.claim(chave, agora)
	if err != nil || reservou || cur.State != stateSent || cur.MessageID != "MSG-1" {
		t.Fatalf("após envio: estado=%+v reservou=%v err=%v", cur, reservou, err)
	}
}

func TestChaveFalhadaPodeSerTentadaDeNovo(t *testing.T) {
	store := openTestStore(t)
	agora := time.Now()
	const chave = "chave-bbbbbbbb"
	if _, _, err := store.claim(chave, agora); err != nil {
		t.Fatal(err)
	}
	if err := store.finish(chave, stateFailed, ""); err != nil {
		t.Fatal(err)
	}
	if _, reservou, err := store.claim(chave, agora); err != nil || !reservou {
		t.Fatalf("chave 'falhou' deveria ser reaproveitada: reservou=%v err=%v", reservou, err)
	}
}

func TestChaveIncertaNaoEReaproveitada(t *testing.T) {
	store := openTestStore(t)
	agora := time.Now()
	const chave = "chave-cccccccc"
	if _, _, err := store.claim(chave, agora); err != nil {
		t.Fatal(err)
	}
	if err := store.finish(chave, stateUncertain, ""); err != nil {
		t.Fatal(err)
	}
	cur, reservou, err := store.claim(chave, agora)
	if err != nil || reservou || cur.State != stateUncertain {
		t.Fatalf("chave incerta não deve ser reenviada: estado=%+v reservou=%v err=%v", cur, reservou, err)
	}
}

func TestCountSinceConsideraEstadosEJanela(t *testing.T) {
	store := openTestStore(t)
	agora := time.Now()
	ontem := agora.AddDate(0, 0, -1)
	for _, c := range []struct {
		chave string
		when  time.Time
		state string
	}{
		{"chave-dddddddd", agora, stateSent},
		{"chave-eeeeeeee", agora, stateReserved},
		{"chave-ffffffff", agora, stateFailed},
		{"chave-gggggggg", ontem, stateSent},
	} {
		if _, _, err := store.claim(c.chave, c.when); err != nil {
			t.Fatal(err)
		}
		if err := store.finish(c.chave, c.state, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.countSince(startOfDay(agora), stateSent, stateReserved, stateUncertain)
	if err != nil || n != 2 {
		t.Fatalf("esperava 2 (um enviado e um pendente de hoje), veio %d (err=%v)", n, err)
	}
}

func TestStartOfDayUsaMeiaNoitePelaHoraLocal(t *testing.T) {
	t1 := time.Date(2026, 10, 25, 15, 30, 0, 0, time.UTC)
	if got := startOfDay(t1); got.Hour() != 0 || got.Day() != 25 {
		t.Fatalf("startOfDay(%v) = %v", t1, got)
	}
}
