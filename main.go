package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
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
	"github.com/gabriel-vasile/mimetype"
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
// Globals
// ---------------------------------------------------------------------------

var (
	exeDir  string
	logFile *os.File
	logMu   sync.Mutex
)

// ---------------------------------------------------------------------------
// Log
// ---------------------------------------------------------------------------

func logf(format string, args ...interface{}) {
	msg := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	fmt.Print(msg)
	if logFile != nil {
		logMu.Lock()
		logFile.WriteString(msg)
		logMu.Unlock()
	}
}

func initLog() {
	path := filepath.Join(exeDir, "zap.log")
	if info, err := os.Stat(path); err == nil && info.Size() >= 1<<20 {
		os.Rename(path, path+".1")
	}
	logFile, _ = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	SMTP struct {
		Host, User, Password string
		Port                 int
	} `toml:"smtp"`
}

func loadConfig() (cfg Config) {
	if _, err := toml.DecodeFile(filepath.Join(exeDir, "config.toml"), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "config.toml:", err)
		os.Exit(1)
	}
	return
}

// ---------------------------------------------------------------------------
// Lock
// ---------------------------------------------------------------------------

func acquireLock() (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(exeDir, "zap.lock"), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, false
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
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
// Abook
// ---------------------------------------------------------------------------

type Contact struct{ Name, Mobile string }

func loadAbook() []Contact {
	f, err := os.Open(filepath.Join(os.Getenv("HOME"), ".local/share/abook/addressbook"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var contacts []Contact
	var cur Contact
	for s := bufio.NewScanner(f); s.Scan(); {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "[") && line != "[format]" {
			if cur.Name != "" && cur.Mobile != "" {
				contacts = append(contacts, cur)
			}
			cur = Contact{}
		} else if k, v, ok := strings.Cut(line, "="); ok {
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

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// Resolve nome: tenta sufixo de 8 dígitos, com/sem 9º dígito, fallback pushName.
func lookupName(jidUser, pushName string, idx map[string]string) string {
	n := digitsOnly(jidUser)
	for _, v := range []string{n, func() string {
		if len(n) == 13 && strings.HasPrefix(n, "55") {
			return n[:4] + n[5:]
		}
		return ""
	}()} {
		if len(v) >= 8 {
			if name, ok := idx[v[len(v)-8:]]; ok {
				return name
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return jidUser
}

func buildIndex(contacts []Contact) map[string]string {
	idx := make(map[string]string, len(contacts))
	for _, c := range contacts {
		n := digitsOnly(c.Mobile)
		if len(n) >= 8 {
			idx[n[len(n)-8:]] = c.Name
		}
	}
	return idx
}

// ---------------------------------------------------------------------------
// Batcher
// ---------------------------------------------------------------------------

const flushDelay = 2 * time.Minute

type PendingMsg struct {
	msgType, txt string
	files        []struct{ Name, Mime string; Data []byte }
	ts           time.Time
}

type Batcher struct {
	mu      sync.Mutex
	pending map[string][]PendingMsg
	timers  map[string]*time.Timer
	names   map[string]string
	cfg     Config
	idx     map[string]string
}

func newBatcher(cfg Config, idx map[string]string) *Batcher {
	return &Batcher{cfg: cfg, idx: idx,
		pending: map[string][]PendingMsg{},
		timers:  map[string]*time.Timer{},
		names:   map[string]string{},
	}
}

func (b *Batcher) Add(jid, pushName string, msg PendingMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[jid] = append(b.pending[jid], msg)
	if b.names[jid] == "" {
		b.names[jid] = lookupName(jid, pushName, b.idx)
	}
	if t := b.timers[jid]; t != nil {
		t.Stop()
	}
	b.timers[jid] = time.AfterFunc(flushDelay, func() { b.flush(jid) })
}

func (b *Batcher) flush(jid string) {
	b.mu.Lock()
	msgs, name := b.pending[jid], b.names[jid]
	delete(b.pending, jid)
	delete(b.timers, jid)
	delete(b.names, jid)
	b.mu.Unlock()
	if len(msgs) == 0 {
		return
	}

	e := email.NewEmail()
	e.From = fmt.Sprintf("Zap <%s>", b.cfg.SMTP.User)
	e.To = []string{b.cfg.SMTP.User}
	e.Subject = fmt.Sprintf("Zap de %s [%d msg]", name, len(msgs))

	weekday := [...]string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sab"}
	var sb strings.Builder
	fmt.Fprintf(&sb, "De: %s\n%d mensagem(ns)\n\n", name, len(msgs))
	for i, m := range msgs {
		fmt.Fprintf(&sb, "── %d  [%s] %s %s ──\n%s\n\n",
			i+1, m.msgType, weekday[m.ts.Weekday()], m.ts.Format("02/01 15:04:05"), m.txt)
		for _, fa := range m.files {
			if len(fa.Data) > 0 {
				mime := fa.Mime
				if mime == "" {
					mime = "application/octet-stream"
				}
				e.Attach(bytes.NewReader(fa.Data), fa.Name, mime)
			}
		}
	}
	e.Text = []byte(sb.String())

	addr := fmt.Sprintf("%s:%d", b.cfg.SMTP.Host, b.cfg.SMTP.Port)
	auth := smtp.PlainAuth("", b.cfg.SMTP.User, b.cfg.SMTP.Password, b.cfg.SMTP.Host)
	var err error
	if b.cfg.SMTP.Port == 465 {
		err = e.SendWithTLS(addr, auth, &tls.Config{ServerName: b.cfg.SMTP.Host})
	} else {
		err = e.Send(addr, auth)
	}
	if err != nil {
		logf("erro ao enviar email: %v", err)
	} else {
		logf("email enviado: %s", e.Subject)
	}
}

func (b *Batcher) FlushAll() {
	b.mu.Lock()
	jids := make([]string, 0, len(b.pending))
	for jid, t := range b.timers {
		t.Stop()
		jids = append(jids, jid)
	}
	b.mu.Unlock()
	for _, jid := range jids {
		b.flush(jid)
	}
}

// ---------------------------------------------------------------------------
// WhatsApp
// ---------------------------------------------------------------------------

func newClient() *whatsmeow.Client {
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(exeDir, "whatsapp.db"))
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
			return err
		}
		if err := client.Connect(); err != nil {
			return err
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		return client.Connect()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handler de mensagens
// ---------------------------------------------------------------------------

func buildHandler(client *whatsmeow.Client, batcher *Batcher) func(interface{}) {
	type msgSpec struct {
		msgType, txt, fileName, mime string
		media                        whatsmeow.DownloadableMessage
	}

	classify := func(r *waE2E.Message) (msgSpec, bool) {
		switch {
		case r.GetConversation() != "":
			return msgSpec{msgType: "Texto", txt: r.GetConversation()}, true
		case r.GetExtendedTextMessage() != nil:
			return msgSpec{msgType: "Texto", txt: r.GetExtendedTextMessage().GetText()}, true
		case r.GetImageMessage() != nil:
			m := r.GetImageMessage()
			return msgSpec{"Imagem", m.GetCaption(), "imagem.jpg", m.GetMimetype(), m}, true
		case r.GetStickerMessage() != nil:
			return msgSpec{"Figurinha", "", "sticker.webp", "image/webp", r.GetStickerMessage()}, true
		case r.GetAudioMessage() != nil:
			m := r.GetAudioMessage()
			return msgSpec{"Áudio", fmt.Sprintf("Duração: %ds", m.GetSeconds()), "audio.ogg", m.GetMimetype(), m}, true
		case r.GetVideoMessage() != nil:
			m := r.GetVideoMessage()
			return msgSpec{"Vídeo", m.GetCaption(), "video.mp4", m.GetMimetype(), m}, true
		case r.GetDocumentMessage() != nil:
			m := r.GetDocumentMessage()
			name := m.GetFileName()
			if name == "" {
				name = "arquivo_recebido"
			}
			return msgSpec{"Documento", name, name, m.GetMimetype(), m}, true
		}
		return msgSpec{}, false
	}

	return func(evt interface{}) {
		v, ok := evt.(*events.Message)
		if !ok || v.Info.IsFromMe || v.Info.Chat.Server == "g.us" || v.Info.Chat.User == "status" {
			return
		}
		raw := v.Message
		if raw.GetDeviceSentMessage() != nil {
			raw = raw.GetDeviceSentMessage().GetMessage()
		}
		spec, ok := classify(raw)
		if !ok {
			return
		}
		msg := PendingMsg{msgType: spec.msgType, txt: spec.txt, ts: v.Info.Timestamp}
		if spec.media != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if data, err := client.Download(ctx, spec.media); err == nil && len(data) > 0 {
				msg.files = []struct{ Name, Mime string; Data []byte }{{spec.fileName, spec.mime, data}}
			} else if err != nil {
				logf("erro download: %v", err)
			}
		}
		batcher.Add(v.Info.Sender.User, v.Info.PushName, msg)
	}
}

// ---------------------------------------------------------------------------
// Send file
// ---------------------------------------------------------------------------

func sendFile(client *whatsmeow.Client, jid types.JID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	mime := mimetype.Detect(data).String()
	ctx := context.Background()
	base := filepath.Base(path)

	switch {
	case strings.HasPrefix(mime, "image/"):
		up, err := client.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return err
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: proto.String(caption), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(uint64(len(data))),
		}})
		return err

	case strings.HasPrefix(mime, "video/"):
		up, err := client.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return err
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			Caption: proto.String(caption), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(uint64(len(data))),
		}})
		return err

	default:
		up, err := client.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return err
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName: proto.String(base), Mimetype: proto.String(mime),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: proto.Uint64(uint64(len(data))),
		}})
		return err
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
	initLog()
	idx := buildIndex(loadAbook())
	client := newClient()
	batcher := newBatcher(cfg, idx)
	client.AddEventHandler(buildHandler(client, batcher))
	client.AddEventHandler(func(evt interface{}) {
		if _, ok := evt.(*events.Disconnected); !ok {
			return
		}
		logf("desconectado — tentando reconectar...")
		for backoff := 5 * time.Second; ; backoff = min(backoff*2, 5*time.Minute) {
			time.Sleep(backoff + time.Duration(rand.Intn(3000))*time.Millisecond)
			if err := connectClient(client); err == nil {
				logf("reconectado")
				return
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

	logf("encerrando...")
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
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		fmt.Fprintln(os.Stderr, "contato inválido")
		return
	}
	jid, _ := types.ParseJID(digitsOnly(parts[1]) + "@s.whatsapp.net")
	client := newClient()
	if err := connectClient(client); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao conectar:", err)
		return
	}
	defer client.Disconnect()
	fmt.Printf("Para: %s\n", parts[0])

	if len(os.Args) >= 3 {
		caption := ""
		if len(os.Args) >= 4 {
			caption = os.Args[3]
		}
		if err := sendFile(client, jid, os.Args[2], caption); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao enviar arquivo:", err)
		}
		return
	}

	for s := bufio.NewScanner(os.Stdin); ; {
		fmt.Print("> ")
		if !s.Scan() {
			break
		}
		if t := strings.TrimSpace(s.Text()); t != "" {
			client.SendMessage(context.Background(), jid, &waE2E.Message{Conversation: proto.String(t)})
		} else {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	exe, _ := os.Executable()
	real, _ := filepath.EvalSymlinks(exe)
	exeDir = filepath.Dir(real)

	if len(os.Args) < 2 {
		fmt.Println("uso: zap <daemon|send> [arquivo] [legenda]")
		return
	}
	switch os.Args[1] {
	case "daemon":
		runDaemon()
	case "send":
		runSend()
	default:
		fmt.Println("uso: zap <daemon|send> [arquivo] [legenda]")
	}
}
