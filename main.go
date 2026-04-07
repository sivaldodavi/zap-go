package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jordan-wright/email"
	"github.com/mdp/qrterminal/v3"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Log em arquivo (rotativo simples: renomeia ao passar de 1 MB)
// ---------------------------------------------------------------------------

var (
	logFile *os.File
	logMu   sync.Mutex
)

func initLog(dir string) {
	path := filepath.Join(dir, "zap.log")
	rotateIfNeeded(path)
	var err error
	logFile, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "não foi possível abrir log:", err)
	}
}

func rotateIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < 1<<20 { // 1 MB
		return
	}
	os.Rename(path, path+".1")
}

func logf(format string, args ...interface{}) {
	msg := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	fmt.Print(msg)
	if logFile != nil {
		logMu.Lock()
		logFile.WriteString(msg)
		logMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type SMTPConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type Config struct {
	SMTP SMTPConfig `toml:"smtp"`
}

var exeDir string // diretório do binário, calculado uma vez no main

func loadConfig() Config {
	cfgPath := filepath.Join(exeDir, "config.toml")
	var cfg Config
	if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao ler config.toml:", err)
		os.Exit(1)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Lock file — impede dupla instância
// ---------------------------------------------------------------------------

func acquireLock() (*os.File, bool) {
	path := filepath.Join(exeDir, "zap.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, false
	}
	// Tenta lock exclusivo não-bloqueante via syscall
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil, false
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, true
}

func releaseLock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
	os.Remove(filepath.Join(exeDir, "zap.lock"))
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

func sendEmailWithAttachment(cfg Config, subject, body, fileName string, fileData []byte) {
	e := email.NewEmail()
	e.From = fmt.Sprintf("Zap <%s>", cfg.SMTP.User)
	e.To = []string{cfg.SMTP.User}
	e.Subject = subject
	e.Text = []byte(body)
	if len(fileData) > 0 {
		e.Attach(bytes.NewReader(fileData), fileName, "application/octet-stream")
	}
	addr := fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port)
	auth := smtp.PlainAuth("", cfg.SMTP.User, cfg.SMTP.Password, cfg.SMTP.Host)
	var err error
	if cfg.SMTP.Port == 465 {
		err = e.SendWithTLS(addr, auth, &tls.Config{ServerName: cfg.SMTP.Host})
	} else {
		err = e.Send(addr, auth)
	}
	if err != nil {
		logf("erro ao enviar email: %v", err)
	} else {
		logf("email enviado: %s", subject)
	}
}

// ---------------------------------------------------------------------------
// Abook & helpers
// ---------------------------------------------------------------------------

type Contact struct {
	Name, Mobile string
}

func loadAbook() []Contact {
	path := filepath.Join(os.Getenv("HOME"), ".local", "share", "abook", "addressbook")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var contacts []Contact
	var cur Contact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && line != "[format]" {
			if cur.Name != "" && cur.Mobile != "" {
				contacts = append(contacts, cur)
			}
			cur = Contact{}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			switch strings.TrimSpace(k) {
			case "name":
				cur.Name = strings.TrimSpace(v)
			case "mobile":
				cur.Mobile = strings.TrimSpace(v)
			}
		}
	}
	if cur.Name != "" && cur.Mobile != "" {
		contacts = append(contacts, cur)
	}
	return contacts
}

func cleanNumber(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// removeNinthDigit remove o 9 extra (Brasil): 55 + DD + 9 + 8 dígitos → 55 + DD + 8 dígitos
func removeNinthDigit(n string) string {
	if len(n) == 13 && strings.HasPrefix(n, "55") {
		return n[:4] + n[5:]
	}
	return ""
}

func findName(jidUser string, contacts []Contact, pushName string) string {
	jidClean := cleanNumber(jidUser)
	variants := []string{jidClean}
	if alt := removeNinthDigit(jidClean); alt != "" {
		variants = append(variants, alt)
	}
	for _, c := range contacts {
		cClean := cleanNumber(c.Mobile)
		for _, v := range variants {
			if v == cClean {
				return c.Name
			}
		}
		if len(cClean) >= 8 {
			suffix := cClean[len(cClean)-8:]
			for _, v := range variants {
				if len(v) >= 8 && v[len(v)-8:] == suffix {
					return c.Name
				}
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return jidUser
}

func portugueseWeekday(d time.Weekday) string {
	return [...]string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sab"}[d]
}

// ---------------------------------------------------------------------------
// Batcher
// ---------------------------------------------------------------------------

const flushDelay = 2 * time.Minute

type PendingMsg struct {
    msgType  string
    txt      string
    fileName string 
    fileData []byte 
    ts       time.Time
}

type Batcher struct {
	mu       sync.Mutex
	pending  map[string][]PendingMsg
	timers   map[string]*time.Timer
	names    map[string]string
	cfg      Config
	contacts []Contact
}

func newBatcher(cfg Config, contacts []Contact) *Batcher {
	return &Batcher{
		pending:  make(map[string][]PendingMsg),
		timers:   make(map[string]*time.Timer),
		names:    make(map[string]string),
		cfg:      cfg,
		contacts: contacts,
	}
}

func (b *Batcher) Add(jid, pushName string, msg PendingMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending[jid] = append(b.pending[jid], msg)

	if _, seen := b.names[jid]; !seen {
		b.names[jid] = findName(jid, b.contacts, pushName)
	} else if b.names[jid] == jid || b.names[jid] == "" {
		b.names[jid] = findName(jid, b.contacts, pushName)
	}

	if t, ok := b.timers[jid]; ok {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}

	jidCopy := jid
	b.timers[jid] = time.AfterFunc(flushDelay, func() {
		b.flush(jidCopy)
	})
}

func (b *Batcher) flush(jid string) {
	b.mu.Lock()
	msgs := b.pending[jid]
	name := b.names[jid]
	delete(b.pending, jid)
	delete(b.timers, jid)
	delete(b.names, jid)
	b.mu.Unlock()

	if len(msgs) == 0 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "De: %s\n%d mensagem(ns)\n\n", name, len(msgs))

	var lastFileData []byte
	var lastFileName string

	for i, m := range msgs {
		fmt.Fprintf(&sb, "── %d  [%s] %s %s ──\n",
			i+1, m.msgType,
			portugueseWeekday(m.ts.Weekday()),
			m.ts.Format("02/01 15:04:05"))
		if m.txt != "" {
			fmt.Fprintf(&sb, "%s\n", m.txt)
		}
		if len(m.fileData) > 0 {
			lastFileData = m.fileData
			lastFileName = m.fileName
		}
		sb.WriteString("\n")
	}

	subject := fmt.Sprintf("Zap de %s [%d msg]", name, len(msgs))
	sendEmailWithAttachment(b.cfg, subject, sb.String(), lastFileName, lastFileData)
}

// FlushAll cancela todos os timers e envia imediatamente os lotes pendentes.
// Chamado no shutdown para não perder mensagens acumuladas.
func (b *Batcher) FlushAll() {
	b.mu.Lock()
	jids := make([]string, 0, len(b.pending))
	for jid, t := range b.timers {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		jids = append(jids, jid)
	}
	b.mu.Unlock()

	for _, jid := range jids {
		b.flush(jid)
	}
}

// ---------------------------------------------------------------------------
// WhatsApp setup
// ---------------------------------------------------------------------------

func newClient() *whatsmeow.Client {
	dbPath := filepath.Join(exeDir, "whatsapp.db")
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)
	container, err := sqlstore.New(context.Background(), "sqlite3", dbURI, waLog.Stdout("Database", "ERROR", true))
	if err != nil {
		logf("erro db: %v", err)
		os.Exit(1)
	}
	device, _ := container.GetFirstDevice(context.Background())
	return whatsmeow.NewClient(device, waLog.Stdout("Client", "ERROR", true))
}

func connectClient(client *whatsmeow.Client) error {
	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			return fmt.Errorf("erro ao obter canal QR: %w", err)
		}
		if err := client.Connect(); err != nil {
			return fmt.Errorf("erro ao conectar: %w", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		if err := client.Connect(); err != nil {
			return fmt.Errorf("erro ao conectar: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handler de mensagens
// ---------------------------------------------------------------------------

func buildHandler(client *whatsmeow.Client, batcher *Batcher) func(interface{}) {
	return func(evt interface{}) {
		v, ok := evt.(*events.Message)
		if !ok || v.Info.IsFromMe {
			return
		}
		if v.Info.Chat.Server == "g.us" || v.Info.Chat.User == "status" {
			return
		}

		rawMsg := v.Message
		if rawMsg.GetDeviceSentMessage() != nil {
			rawMsg = rawMsg.GetDeviceSentMessage().GetMessage()
		}

		var txt, msgType, fileName string
		var fileData []byte
		var err error

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		switch {
		case rawMsg.GetConversation() != "":
			txt, msgType = rawMsg.GetConversation(), "Texto"
		case rawMsg.GetExtendedTextMessage() != nil:
			txt, msgType = rawMsg.GetExtendedTextMessage().GetText(), "Texto"
		case rawMsg.GetImageMessage() != nil:
			m := rawMsg.GetImageMessage()
			msgType, txt, fileName = "Imagem", "Legenda: "+m.GetCaption(), "imagem.jpg"
			fileData, err = client.Download(ctx, m)
		case rawMsg.GetStickerMessage() != nil:
			msgType, txt, fileName = "Figurinha", "[Figurinha]", "sticker.webp"
			fileData, err = client.Download(ctx, rawMsg.GetStickerMessage())
		case rawMsg.GetAudioMessage() != nil:
			m := rawMsg.GetAudioMessage()
			msgType, txt, fileName = "Áudio", fmt.Sprintf("Duração: %ds", m.GetSeconds()), "audio.ogg"
			fileData, err = client.Download(ctx, m)
		case rawMsg.GetVideoMessage() != nil:
			m := rawMsg.GetVideoMessage()
			msgType, txt, fileName = "Vídeo", "Legenda: "+m.GetCaption(), "video.mp4"
			fileData, err = client.Download(ctx, m)
		case rawMsg.GetDocumentMessage() != nil:
			m := rawMsg.GetDocumentMessage()
			msgType = "Documento"
			txt = "Arquivo original: " + m.GetFileName()
			fileName = m.GetFileName()
			if fileName == "" {
				fileName = "arquivo_recebido"
			}
			fileData, err = client.Download(ctx, m)
		}

		if msgType == "" {
			return
		}
		if err != nil {
			logf("erro download: %v", err)
		}

		batcher.Add(v.Info.Sender.User, v.Info.PushName, PendingMsg{
			msgType:  msgType,
			txt:      txt,
			fileName: fileName,
			fileData: fileData,
			ts:       v.Info.Timestamp,
		})
	}
}

// ---------------------------------------------------------------------------
// Daemon
// ---------------------------------------------------------------------------

func runDaemon() {
	lock, ok := acquireLock()
	if !ok {
		fmt.Fprintln(os.Stderr, "outra instância já está rodando")
		os.Exit(1)
	}
	defer releaseLock(lock)

	cfg := loadConfig()
	initLog(exeDir)
	contacts := loadAbook()
	client := newClient()
	batcher := newBatcher(cfg, contacts)

	client.AddEventHandler(buildHandler(client, batcher))

	// Reconexão automática com backoff exponencial
	client.AddEventHandler(func(evt interface{}) {
		if _, ok := evt.(*events.Disconnected); !ok {
			return
		}
		logf("desconectado — tentando reconectar...")
		backoff := 5 * time.Second
		for {
			time.Sleep(backoff)
			if err := connectClient(client); err == nil {
				logf("reconectado com sucesso")
				return
			} else {
				logf("reconexão falhou: %v — tentando em %s", err, backoff)
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		}
	})

	if err := connectClient(client); err != nil {
		logf("erro ao conectar: %v", err)
		os.Exit(1)
	}

	logf("daemon iniciado")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logf("encerrando — enviando lotes pendentes...")
	batcher.FlushAll()
	client.Disconnect()
	logf("encerrado")
}

// ---------------------------------------------------------------------------
// Send
// ---------------------------------------------------------------------------

func runSend() {
	contacts := loadAbook()
	var lines []string
	for _, c := range contacts {
		lines = append(lines, fmt.Sprintf("%s\t%s", c.Name, c.Mobile))
	}
	cmd := exec.Command("fzf", "--prompt=para: ", "--delimiter=\t", "--with-nth=1")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	out, err := cmd.Output()
	if err != nil {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		fmt.Fprintln(os.Stderr, "contato inválido")
		return
	}
	jid, _ := types.ParseJID(cleanNumber(parts[1]) + "@s.whatsapp.net")
	client := newClient()
	if err := connectClient(client); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao conectar:", err)
		return
	}
	defer client.Disconnect()

	fmt.Printf("Para: %s\n", parts[0])
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		t := strings.TrimSpace(scanner.Text())
		if t == "" {
			break
		}
		client.SendMessage(context.Background(), jid, &waE2E.Message{Conversation: proto.String(t)})
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	// Calcula exeDir uma vez — usado por loadConfig, newClient, initLog, acquireLock
	exe, _ := os.Executable()
	real, _ := filepath.EvalSymlinks(exe)
	exeDir = filepath.Dir(real)

	if len(os.Args) < 2 {
		fmt.Println("uso: zap <daemon|send>")
		return
	}
	switch os.Args[1] {
	case "daemon":
		runDaemon()
	case "send":
		runSend()
	}
}
