package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
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

// --- Config ---

type SMTPConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type Config struct {
	SMTP SMTPConfig `toml:"smtp"`
}

func loadConfig() Config {
	exePath, _ := os.Executable()
	realPath, _ := filepath.EvalSymlinks(exePath)
	cfgPath := filepath.Join(filepath.Dir(realPath), "config.toml")

	var cfg Config
	if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "erro ao ler config.toml:", err)
		os.Exit(1)
	}
	return cfg
}

// --- Email com suporte a Anexo ---

func sendEmailWithAttachment(cfg Config, subject, body, fileName string, fileData []byte) {
	e := email.NewEmail()
	e.From = fmt.Sprintf("Zap <%s>", cfg.SMTP.User)
	e.To = []string{cfg.SMTP.User}
	e.Subject = subject
	e.Text = []byte(body)

	if len(fileData) > 0 {
		e.Attach(strings.NewReader(string(fileData)), fileName, "application/octet-stream")
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
		fmt.Fprintln(os.Stderr, "erro ao enviar email:", err)
	} else {
		fmt.Printf("[%s] Email enviado: %s\n", time.Now().Format("15:04:05"), subject)
	}
}

// --- WhatsApp Setup ---

func newClient() *whatsmeow.Client {
	exePath, _ := os.Executable()
	realPath, _ := filepath.EvalSymlinks(exePath)
	dbPath := filepath.Join(filepath.Dir(realPath), "whatsapp.db")
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)

	container, err := sqlstore.New(context.Background(), "sqlite3", dbURI, waLog.Stdout("Database", "ERROR", true))
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro db:", err)
		os.Exit(1)
	}
	device, _ := container.GetFirstDevice(context.Background())
	return whatsmeow.NewClient(device, waLog.Stdout("Client", "ERROR", true))
}

func connectClient(client *whatsmeow.Client) {
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		client.Connect()
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		client.Connect()
	}
}

// --- Abook & Helpers ---

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
			case "name": cur.Name = strings.TrimSpace(v)
			case "mobile": cur.Mobile = strings.TrimSpace(v)
			}
		}
	}
	if cur.Name != "" && cur.Mobile != "" { contacts = append(contacts, cur) }
	return contacts
}

func cleanNumber(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' { return r }
		return -1
	}, s)
}

func findName(jidUser string, contacts []Contact, pushName string) string {
	clean := cleanNumber(jidUser)
	if len(clean) >= 8 {
		suffix := clean[len(clean)-8:]
		for _, c := range contacts {
			s := cleanNumber(c.Mobile)
			if len(s) >= 8 && s[len(s)-8:] == suffix { return c.Name }
		}
	}
	return (func() string { if pushName != "" { return pushName }; return jidUser })()
}

func portugueseWeekday(d time.Weekday) string {
	return [...]string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sab"}[d]
}

// --- Daemon ---

func runDaemon() {
	cfg := loadConfig()
	contacts := loadAbook()
	client := newClient()

	client.AddEventHandler(func(evt interface{}) {
		v, ok := evt.(*events.Message)
		if !ok || v.Info.IsFromMe { return }

		// --- FILTROS DE SEGURANÇA ---
		// 1. Bloqueia Mensagens de Grupos (@g.us)
		if v.Info.Chat.Server == "g.us" { return }
		
		// 2. Bloqueia Mensagens de Status/Stories (status@broadcast)
		if v.Info.Chat.User == "status" { return }
		// ----------------------------

		rawMsg := v.Message
		if rawMsg.GetDeviceSentMessage() != nil {
			rawMsg = rawMsg.GetDeviceSentMessage().GetMessage()
		}

		var txt, msgType, fileName string
		var fileData []byte
		var err error
		ctx := context.Background()

		if m := rawMsg.GetConversation(); m != "" {
			txt, msgType = m, "Texto"
		} else if m := rawMsg.GetExtendedTextMessage(); m != nil {
			txt, msgType = m.GetText(), "Texto"
		} else if m := rawMsg.GetImageMessage(); m != nil {
			msgType, txt, fileName = "Imagem", "Legenda: "+m.GetCaption(), "imagem.jpg"
			fileData, err = client.Download(ctx, m)
		} else if m := rawMsg.GetStickerMessage(); m != nil {
			msgType, txt, fileName = "Figurinha", "[Figurinha]", "sticker.webp"
			fileData, err = client.Download(ctx, m)
		} else if m := rawMsg.GetAudioMessage(); m != nil {
			msgType, txt, fileName = "Áudio", fmt.Sprintf("Duração: %ds", m.GetSeconds()), "audio.ogg"
			fileData, err = client.Download(ctx, m)
		} else if m := rawMsg.GetVideoMessage(); m != nil {
			msgType, txt, fileName = "Vídeo", "Legenda: "+m.GetCaption(), "video.mp4"
			fileData, err = client.Download(ctx, m)
		}

		if msgType == "" { return }
		if err != nil { fmt.Fprintln(os.Stderr, "Erro download:", err) }

		name := findName(v.Info.Sender.User, contacts, v.Info.PushName)
		body := fmt.Sprintf("De: %s\nData: %s %s\nTipo: %s\n\n%s", 
			name, portugueseWeekday(v.Info.Timestamp.Weekday()), 
			v.Info.Timestamp.Format("02/01 15:04"), msgType, txt)

		sendEmailWithAttachment(cfg, "Zap de "+name+" ["+msgType+"]", body, fileName, fileData)
	})

	connectClient(client)
	fmt.Println("Daemon rodando (Privado apenas + Figurinha). Ctrl+C para parar.")
	
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	client.Disconnect()
}

func runSend() {
	contacts := loadAbook()
	var lines []string
	for _, c := range contacts { lines = append(lines, fmt.Sprintf("%s\t%s", c.Name, c.Mobile)) }
	cmd := exec.Command("fzf", "--prompt=para: ", "--delimiter=\t", "--with-nth=1")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	out, err := cmd.Output()
	if err != nil { return }
	
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	jid, _ := types.ParseJID(cleanNumber(parts[1]) + "@s.whatsapp.net")

	client := newClient()
	connectClient(client)
	defer client.Disconnect()

	fmt.Printf("Para: %s\n", parts[0])
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() { break }
		t := strings.TrimSpace(scanner.Text())
		if t == "" { break }
		client.SendMessage(context.Background(), jid, &waE2E.Message{Conversation: proto.String(t)})
	}
}

func main() {
	if len(os.Args) < 2 { fmt.Println("uso: zap <daemon|send>"); return }
	switch os.Args[1] {
	case "daemon": runDaemon()
	case "send": runSend()
	}
}
