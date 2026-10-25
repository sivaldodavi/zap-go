package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Estados de um envio. Nenhum guarda telefone nem texto da mensagem.
const (
	stateReserved  = "pendente"  // reservado; o envio ainda não terminou
	stateSent      = "enviado"   // o WhatsApp confirmou com um ID de mensagem
	stateFailed    = "falhou"    // garantidamente não enviado; pode ser tentado de novo
	stateUncertain = "incerto"   // o envio foi tentado e não há confirmação; não reenviar sozinho
	retentionDays  = 90
)

// deliveries guarda o estado de cada chave de idempotência em gateway.db.
type deliveries struct{ db *sql.DB }

type delivery struct {
	State     string
	MessageID string
}

func openDeliveries(path string) (*deliveries, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	schema := `CREATE TABLE IF NOT EXISTS deliveries (
		key        TEXT PRIMARY KEY,
		state      TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS deliveries_created_at ON deliveries (created_at);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("criar tabela: %w", err)
	}
	return &deliveries{db: db}, nil
}

func (d *deliveries) close() error { return d.db.Close() }

func (d *deliveries) prune(now time.Time) error {
	cutoff := now.AddDate(0, 0, -retentionDays).Unix()
	_, err := d.db.Exec(`DELETE FROM deliveries WHERE created_at < ?`, cutoff)
	return err
}

// claim reserva a chave se ela estiver livre ou 'falhou'. Retorna o estado atual e se
// esta chamada ficou com a reserva.
func (d *deliveries) claim(key string, now time.Time) (delivery, bool, error) {
	res, err := d.db.Exec(
		`INSERT OR IGNORE INTO deliveries (key, state, message_id, created_at) VALUES (?, ?, '', ?)`,
		key, stateReserved, now.Unix())
	if err != nil {
		return delivery{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return delivery{State: stateReserved}, true, nil
	}
	res, err = d.db.Exec(
		`UPDATE deliveries SET state = ?, created_at = ? WHERE key = ? AND state = ?`,
		stateReserved, now.Unix(), key, stateFailed)
	if err != nil {
		return delivery{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return delivery{State: stateReserved}, true, nil
	}
	cur, err := d.get(key)
	return cur, false, err
}

func (d *deliveries) get(key string) (delivery, error) {
	var cur delivery
	err := d.db.QueryRow(`SELECT state, message_id FROM deliveries WHERE key = ?`, key).
		Scan(&cur.State, &cur.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery{}, errors.New("chave de envio sumiu durante a consulta")
	}
	return cur, err
}

func (d *deliveries) finish(key, state, messageID string) error {
	_, err := d.db.Exec(`UPDATE deliveries SET state = ?, message_id = ? WHERE key = ?`, state, messageID, key)
	return err
}

// countSince conta envios com algum dos estados informados desde 'since'.
func (d *deliveries) countSince(since time.Time, states ...string) (int, error) {
	args := []any{since.Unix()}
	marks := make([]string, len(states))
	for i, s := range states {
		marks[i] = "?"
		args = append(args, s)
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM deliveries WHERE created_at >= ? AND state IN (%s)`,
		strings.Join(marks, ", "))
	var n int
	err := d.db.QueryRow(query, args...).Scan(&n)
	return n, err
}

// pacer espaça os envios: cada um ocupa um intervalo mínimo mais uma folga aleatória.
type pacer struct {
	mu      sync.Mutex
	next    time.Time
	minGap  time.Duration
	jitter  time.Duration
	maxWait time.Duration
	now     func() time.Time
	rnd     func(limit time.Duration) time.Duration
}

// reserve devolve quanto esperar antes de enviar. Se a espera passar de maxWait, ok é false
// e nada é reservado.
func (p *pacer) reserve() (wait time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	slot := now
	if p.next.After(now) {
		slot = p.next
	}
	wait = slot.Sub(now)
	if wait > p.maxWait {
		return wait, false
	}
	gap := p.minGap
	if p.jitter > 0 {
		gap += p.rnd(p.jitter)
	}
	p.next = slot.Add(gap)
	return wait, true
}

type admitStatus int

const (
	admitted    admitStatus = iota // pode enviar após Wait
	alreadySent                    // chave já enviada: devolver o resultado anterior
	inDoubt                        // chave em estado incerto ou pendente: não reenviar
	throttled                      // ritmo: tentar depois de Wait
	dailyCapped                    // limite diário atingido
)

type admitResult struct {
	Status   admitStatus
	Existing delivery
	Wait     time.Duration
}

// gateway serializa reserva, limite diário e pacing. O envio em si acontece fora do lock.
type gateway struct {
	mu         sync.Mutex
	store      *deliveries
	pacer      *pacer
	dailyLimit int
	now        func() time.Time
}

func newGateway(cfg config, store *deliveries) *gateway {
	return &gateway{
		store:      store,
		dailyLimit: cfg.DailyLimit,
		now:        time.Now,
		pacer: &pacer{
			minGap:  cfg.MinInterval,
			jitter:  cfg.Jitter,
			maxWait: cfg.MaxWait,
			now:     time.Now,
			rnd:     randDuration,
		},
	}
}

func randDuration(limit time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(limit)))
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// admit decide se uma chave pode gerar um envio agora.
func (g *gateway) admit(key string) (admitResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	cur, claimed, err := g.store.claim(key, now)
	if err != nil {
		return admitResult{}, err
	}
	if !claimed {
		if cur.State == stateSent {
			return admitResult{Status: alreadySent, Existing: cur}, nil
		}
		return admitResult{Status: inDoubt, Existing: cur}, nil
	}

	// A partir daqui a reserva existe. Qualquer recusa antes do envio libera a chave como 'falhou'.
	reject := func(status admitStatus, wait time.Duration) (admitResult, error) {
		if err := g.store.finish(key, stateFailed, ""); err != nil {
			return admitResult{}, err
		}
		return admitResult{Status: status, Wait: wait}, nil
	}

	used, err := g.store.countSince(startOfDay(now), stateReserved, stateUncertain, stateSent)
	if err != nil {
		return admitResult{}, err
	}
	if used > g.dailyLimit { // a própria reserva já está contada
		return reject(dailyCapped, 0)
	}

	wait, ok := g.pacer.reserve()
	if !ok {
		return reject(throttled, wait)
	}
	return admitResult{Status: admitted, Wait: wait}, nil
}

func (g *gateway) complete(key, messageID string) error {
	return g.store.finish(key, stateSent, messageID)
}

func (g *gateway) fail(key string) error {
	return g.store.finish(key, stateFailed, "")
}

func (g *gateway) markUncertain(key string) error {
	return g.store.finish(key, stateUncertain, "")
}

func (g *gateway) sentToday() (int, error) {
	return g.store.countSince(startOfDay(g.now()), stateSent)
}
