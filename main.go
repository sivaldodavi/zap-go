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

// --- Email ---

func portugueseWeekday(d time.Weekday) string {
	return [...]string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sab"}[d]
}

func sendEmailRaw(cfg Config, subject, body string) {
	addr := fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port)
	auth := smtp.PlainAuth("", cfg.SMTP.User, cfg.SMTP.Password, cfg.SMTP.Host)

	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.SMTP.User, cfg.SMTP.User, subject, body,
	)

	var err error
	if cfg.SMTP.Port == 465 {
		conn, e := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.SMTP.Host})
		if e != nil {
			fmt.Fprintln(os.Stderr, "erro tls:", e)
			return
		}
		c, e := smtp.NewClient(conn, cfg.SMTP.Host)
		if e != nil {
			fmt.Fprintln(os.Stderr, "erro smtp:", e)
			return
		}
		defer c.Close()
		c.Auth(auth)
		c.Mail(cfg.SMTP.User)
		c.Rcpt(cfg.SMTP.User)
		w, _ := c.Data()
		fmt.Fprint(w, msg)
		err = w.Close()
	} else {
		err = smtp.SendMail(addr, auth, cfg.SMTP.User, []string{cfg.SMTP.User}, []byte(msg))
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "erro ao enviar email:", err)
	} else {
		fmt.Printf("[%s] email enviado: %s\n", time.Now().Format("15:04:05"), subject)
	}
}

// --- WhatsApp ---

func newClient() *whatsmeow.Client {
	exePath, _ := os.Executable()
	realPath, _ := filepath.EvalSymlinks(exePath)
	dbPath := filepath.Join(filepath.Dir(realPath), "whatsapp.db")
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)

	container, err := sqlstore.New(
		context.Background(), "sqlite3", dbURI,
		waLog.Stdout("Database", "ERROR", true),
	)
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

// --- Abook ---

type Contact struct {
	Name   string
	Mobile string
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

func findName(jidUser string, contacts []Contact, pushName string) string {
	clean := cleanNumber(jidUser)
	if len(clean) >= 8 {
		suffix := clean[len(clean)-8:]
		for _, c := range contacts {
			s := cleanNumber(c.Mobile)
			if len(s) >= 8 && s[len(s)-8:] == suffix {
				return c.Name
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return jidUser
}

// --- Comandos ---

func runDaemon() {
	cfg := loadConfig()
	contacts := loadAbook()
	client := newClient()

	client.AddEventHandler(func(evt interface{}) {
		v, ok := evt.(*events.Message)
		if !ok || v.Info.IsFromMe {
			return
		}

		// Tenta pegar o texto (conversa simples ou estendida)
		txt := ""
		if v.Message.Conversation != nil {
			txt = *v.Message.Conversation
		} else if v.Message.ExtendedTextMessage != nil {
			txt = *v.Message.ExtendedTextMessage.Text
		}

		if txt == "" {
			return
		}

		name := findName(v.Info.Sender.User, contacts, v.Info.PushName)
		fmt.Printf("[%s] %s -> Enviando email...\n", time.Now().Format("15:04:05"), name)

		// Formata corpo do email
		body := fmt.Sprintf("De: %s\nData: [%s %s]\n\nMensagem:\n%s",
			name,
			portugueseWeekday(v.Info.Timestamp.Weekday()),
			v.Info.Timestamp.Format("02/01 15:04"),
			txt,
		)

		subject := fmt.Sprintf("WhatsApp de %s", name)
		sendEmailRaw(cfg, subject, body)
	})

	connectClient(client)
	fmt.Println("daemon rodando (modo imediato). Ctrl+C para parar.")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	client.Disconnect()
}

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
		return
	}
	chosen := Contact{Name: parts[0], Mobile: parts[1]}
	phone := cleanNumber(chosen.Mobile)

	jid, err := types.ParseJID(phone + "@s.whatsapp.net")
	if err != nil {
		fmt.Fprintln(os.Stderr, "numero invalido:", phone)
		os.Exit(1)
	}

	client := newClient()
	connectClient(client)
	defer client.Disconnect()

	fmt.Printf("para: %s (%s)\n", chosen.Name, chosen.Mobile)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			break
		}
		client.SendMessage(context.Background(), jid, &waE2E.Message{
			Conversation: proto.String(text),
		})
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("uso: zap <daemon|send>")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "daemon":
		runDaemon()
	case "send":
		runSend()
	default:
		fmt.Println("uso: zap <daemon|send>")
		os.Exit(1)
	}
}
