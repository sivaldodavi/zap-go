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
// Log em arquivo
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
	if os.IsNotExist(err) || info == nil || info.Size() < 1<<20 {
		return
	}
	if err := os.Rename(path, path+".1"); err != nil {
		fmt.Fprintln(os.Stderr, "rotate log:", err)
	}
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

var exeDir string

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
// Lock file
// ---------------------------------------------------------------------------

func acquireLock() (*os.File, bool) {
	path := filepath.Join(exeDir, "zap.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, false
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
// Abook & índice
// ---------------------------------------------------------------------------

type Contact struct {
	Name, Mobile string
}

type AbookIndex struct {
	bySuffix map[string]string // últimos 8 dígitos → nome
}

func buildIndex(contacts []Contact) AbookIndex {
	idx := AbookIndex{bySuffix: make(map[string]string, len(contacts))}
	for _, c := range contacts {
		n := cleanNumber(c.Mobile)
		if len(n) >= 8 {
			idx.bySuffix[n[len(n)-8:]] = c.Name
		}
	}
	return idx
}

func (idx AbookIndex) Lookup(jidUser, pushName string) string {
	clean := cleanNumber(jidUser)
	variants := []string{clean}
	if alt := removeNinthDigit(clean); alt != "" {
		variants = append(variants, alt)
	}
	for _, v := range variants {
		if len(v) >= 8 {
			if name, ok := idx.bySuffix[v[len(v)-8:]]; ok {
				return name
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return jidUser
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

func removeNinthDigit(n string) string {
	if len(n) == 13 && strings.HasPrefix(n, "55") {
		return n[:4] + n[5:]
	}
	return ""
}

func portugueseWeekday(d time.Weekday) string {
	return [...]string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sab"}[d]
}

// ---------------------------------------------------------------------------
// Batcher
// ---------------------------------------------------------------------------

const flushDelay = 2 * time.Minute

type FileAttachment struct {
	Name string
	Data []byte
	Mime string
}

type PendingMsg struct {
	msgType string
	txt     string
	files   []FileAttachment
	ts      time.Time
}

type Batcher struct {
	mu      sync.Mutex
	pending map[string][]PendingMsg
	timers  map[string]*time.Timer
	names   map[string]string
	cfg     Config
	idx     AbookIndex
}

func newBatcher(cfg Config, idx AbookIndex) *Batcher {
	return &Batcher{
		pending: make(map[string][]PendingMsg),
		timers:  make(map[string]*time.Timer),
		names:   make(map[string]string),
		cfg:     cfg,
		idx:     idx,
	}
}

func (b *Batcher) Add(jid, pushName string, msg PendingMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending[jid] = append(b.pending[jid], msg)

	if b.names[jid] == "" || b.names[jid] == jid {
		b.names[jid] = b.idx.Lookup(jid, pushName)
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
	b.timers[jid] = time.AfterFunc(flushDelay, func() { b.flush(jidCopy) })
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

	e := email.NewEmail()
	e.From = fmt.Sprintf("Zap <%s>", b.cfg.SMTP.User)
	e.To = []string{b.cfg.SMTP.User}
	e.Subject = fmt.Sprintf("Zap de %s [%d msg]", name, len(msgs))

	var sb strings.Builder
	fmt.Fprintf(&sb, "De: %s\n%d mensagem(ns)\n\n", name, len(msgs))

	for i, m := range msgs {
		fmt.Fprintf(&sb, "── %d  [%s] %s %s ──\n",
			i+1, m.msgType,
			portugueseWeekday(m.ts.Weekday()),
			m.ts.Format("02/01 15:04:05"))
		if m.txt != "" {
			fmt.Fprintf(&sb, "%s\n", m.txt)
		}
		for _, fa := range m.files {
			if len(fa.Data) > 0 {
				mime := fa.Mime
				if mime == "" {
					mime = "application/octet-stream"
				}
				e.Attach(bytes.NewReader(fa.Data), fa.Name, mime)
			}
		}
		sb.WriteString("\n")
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
// Handler de mensagens recebidas
// ---------------------------------------------------------------------------

type msgInfo struct {
	msgType string
	txt     string
	fileName string
	mimeType string
	media   whatsmeow.DownloadableMessage
}

func classifyMessage(raw *waE2E.Message) (msgInfo, bool) {
	switch {
	case raw.GetConversation() != "":
		return msgInfo{msgType: "Texto", txt: raw.GetConversation()}, true

	case raw.GetExtendedTextMessage() != nil:
		return msgInfo{msgType: "Texto", txt: raw.GetExtendedTextMessage().GetText()}, true

	case raw.GetImageMessage() != nil:
		m := raw.GetImageMessage()
		return msgInfo{
			msgType:  "Imagem",
			txt:      m.GetCaption(),
			fileName: "imagem.jpg",
			mimeType: m.GetMimetype(),
			media:    m,
		}, true

	case raw.GetStickerMessage() != nil:
		return msgInfo{
			msgType:  "Figurinha",
			fileName: "sticker.webp",
			mimeType: "image/webp",
			media:    raw.GetStickerMessage(),
		}, true

	case raw.GetAudioMessage() != nil:
		m := raw.GetAudioMessage()
		return msgInfo{
			msgType:  "Áudio",
			txt:      fmt.Sprintf("Duração: %ds", m.GetSeconds()),
			fileName: "audio.ogg",
			mimeType: m.GetMimetype(),
			media:    m,
		}, true

	case raw.GetVideoMessage() != nil:
		m := raw.GetVideoMessage()
		return msgInfo{
			msgType:  "Vídeo",
			txt:      m.GetCaption(),
			fileName: "video.mp4",
			mimeType: m.GetMimetype(),
			media:    m,
		}, true

	case raw.GetDocumentMessage() != nil:
		m := raw.GetDocumentMessage()
		name := m.GetFileName()
		if name == "" {
			name = "arquivo_recebido"
		}
		return msgInfo{
			msgType:  "Documento",
			txt:      name,
			fileName: name,
			mimeType: m.GetMimetype(),
			media:    m,
		}, true
	}

	return msgInfo{}, false
}

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

		info, ok := classifyMessage(rawMsg)
		if !ok {
			return
		}

		msg := PendingMsg{
			msgType: info.msgType,
			txt:     info.txt,
			ts:      v.Info.Timestamp,
		}

		if info.media != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			data, err := client.Download(ctx, info.media)
			if err != nil {
				logf("erro download: %v", err)
			} else if len(data) > 0 {
				msg.files = []FileAttachment{{
					Name: info.fileName,
					Data: data,
					Mime: info.mimeType,
				}}
			}
		}

		batcher.Add(v.Info.Sender.User, v.Info.PushName, msg)
	}
}

// ---------------------------------------------------------------------------
// Envio de arquivos
// ---------------------------------------------------------------------------

func sendFile(client *whatsmeow.Client, jid types.JID, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("leitura: %w", err)
	}

	mime := mimetype.Detect(data).String()
	ctx := context.Background()

	switch {
	case strings.HasPrefix(mime, "image/"):
		uploaded, err := client.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mime),
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		})
		return err

	case strings.HasPrefix(mime, "video/"):
		uploaded, err := client.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mime),
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		})
		return err

	default: // PDF, DOCX e qualquer outro documento
		uploaded, err := client.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		_, err = client.SendMessage(ctx, jid, &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName:      proto.String(filepath.Base(path)),
				Mimetype:      proto.String(mime),
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		})
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
	initLog(exeDir)
	contacts := loadAbook()
	idx := buildIndex(contacts)
	client := newClient()
	batcher := newBatcher(cfg, idx)

	client.AddEventHandler(buildHandler(client, batcher))

	client.AddEventHandler(func(evt interface{}) {
		if _, ok := evt.(*events.Disconnected); !ok {
			return
		}
		logf("desconectado — tentando reconectar...")
		backoff := 5 * time.Second
		for {
			jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
			time.Sleep(backoff + jitter)
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
//
// Uso:
//   zap send                        → escolhe contato via fzf, loop de texto
//   zap send foto.jpg               → envia imagem
//   zap send doc.pdf "legenda"      → envia documento com legenda/caption
//   zap send video.mp4 "legenda"    → envia vídeo com legenda
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

	jid, _ := types.ParseJID(cleanNumber(parts[1]) + "@s.whatsapp.net")
	client := newClient()
	if err := connectClient(client); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao conectar:", err)
		return
	}
	defer client.Disconnect()

	fmt.Printf("Para: %s\n", parts[0])

	// Envio de arquivo passado como argumento
	if len(os.Args) >= 3 {
		filePath := os.Args[2]
		caption := ""
		if len(os.Args) >= 4 {
			caption = os.Args[3]
		}
		if err := sendFile(client, jid, filePath, caption); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao enviar arquivo:", err)
		}
		return
	}

	// Loop interativo de texto
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
		client.SendMessage(context.Background(), jid, &waE2E.Message{
			Conversation: proto.String(t),
		})
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
