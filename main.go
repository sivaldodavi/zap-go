package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// ─── Estilos (aerc-like) ──────────────────────────────────────────────────────

var (
	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("238")).
			Padding(0, 1)

	styleStatus = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Background(lipgloss.Color("236")).
			Padding(0, 1)

	styleDivider = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	styleMyName = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")).
			Bold(true)

	styleTheirName = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12")).
			Bold(true)

	styleTimestamp = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	styleMyMsg = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	styleTheirMsg = lipgloss.NewStyle().
			Foreground(lipgloss.Color("255"))

	styleInputPrompt = lipgloss.NewStyle().
				Foreground(lipgloss.Color("10")).
				Bold(true)

	styleInputBar = lipgloss.NewStyle().
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240"))

	styleUnreadBadge = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(lipgloss.Color("9")).
				Bold(true).
				Padding(0, 1)
)

// ─── Histórico persistente ────────────────────────────────────────────────────

type storedMsg struct {
	Sender  string    `json:"sender"`
	Content string    `json:"content"`
	IsMe    bool      `json:"is_me"`
	Time    time.Time `json:"time"`
}

type chatMetadata struct {
	LastRead time.Time `json:"last_read"`
}

func dataDir() string {
	dir := filepath.Join(os.Getenv("HOME"), ".local", "share", "zap-tui")
	os.MkdirAll(dir, 0700)
	return dir
}

func historyPath(phone string) string {
	return filepath.Join(dataDir(), phone+".json")
}

func metadataPath(phone string) string {
	return filepath.Join(dataDir(), phone+".meta.json")
}

func loadHistory(phone string) []storedMsg {
	data, err := os.ReadFile(historyPath(phone))
	if err != nil {
		return nil
	}
	var msgs []storedMsg
	json.Unmarshal(data, &msgs)
	return msgs
}

func saveHistory(phone string, msgs []storedMsg) {
	data, _ := json.Marshal(msgs)
	os.WriteFile(historyPath(phone), data, 0600)
}

func loadMetadata(phone string) chatMetadata {
	data, err := os.ReadFile(metadataPath(phone))
	if err != nil {
		return chatMetadata{}
	}
	var meta chatMetadata
	json.Unmarshal(data, &meta)
	return meta
}

func saveMetadata(phone string, meta chatMetadata) {
	data, _ := json.Marshal(meta)
	os.WriteFile(metadataPath(phone), data, 0600)
}

// ─── Contatos (formato abook) ─────────────────────────────────────────────────

type contact struct {
	name   string
	mobile string
}

func loadAbook() []contact {
	path := filepath.Join(os.Getenv("HOME"), ".local", "share", "abook", "addressbook")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var contacts []contact
	var cur contact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && line != "[format]" {
			if cur.name != "" && cur.mobile != "" {
				contacts = append(contacts, cur)
			}
			cur = contact{}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			switch strings.TrimSpace(k) {
			case "name":
				cur.name = strings.TrimSpace(v)
			case "mobile":
				cur.mobile = strings.TrimSpace(v)
			}
		}
	}
	if cur.name != "" && cur.mobile != "" {
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

// ─── Lista de Conversas ───────────────────────────────────────────────────────

type chatInfo struct {
	contact    contact
	phone      string
	lastMsg    time.Time
	unreadCount int
	preview    string
}

func getChats(contacts []contact) []chatInfo {
	var chats []chatInfo

	for _, c := range contacts {
		phone := cleanNumber(c.mobile)
		history := loadHistory(phone)
		if len(history) == 0 {
			continue
		}

		meta := loadMetadata(phone)
		lastMsg := history[len(history)-1]
		
		unread := 0
		preview := lastMsg.Content
		if len(preview) > 50 {
			preview = preview[:50] + "..."
		}

		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Time.After(meta.LastRead) && !history[i].IsMe {
				unread++
			}
		}

		chats = append(chats, chatInfo{
			contact:    c,
			phone:      phone,
			lastMsg:    lastMsg.Time,
			unreadCount: unread,
			preview:    preview,
		})
	}

	sort.Slice(chats, func(i, j int) bool {
		return chats[i].lastMsg.After(chats[j].lastMsg)
	})

	return chats
}

func pickChatFzf(chats []chatInfo) (contact, bool) {
	if len(chats) == 0 {
		return contact{}, false
	}

	var lines []string
	for _, c := range chats {
		badge := ""
		if c.unreadCount > 0 {
			badge = fmt.Sprintf("[%d] ", c.unreadCount)
		}
		when := c.lastMsg.Format("02/01 15:04")
		line := fmt.Sprintf("%s%s\t%s\t%s\t%s",
			badge, c.contact.name, c.contact.mobile, when, c.preview)
		lines = append(lines, line)
	}

	cmd := exec.Command("fzf",
		"--prompt=Conversa: ",
		"--delimiter=\t",
		"--with-nth=1",
		"--header=Enter para abrir  Tab para novo  Esc para sair",
		"--bind=tab:abort",
	)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	
	if err != nil {
		if err.Error() == "exit status 130" { // Tab pressed
			return pickContactFzf(loadAbook())
		}
		return contact{}, false
	}

	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 4)
	if len(parts) < 2 {
		return contact{}, false
	}
	
	name := strings.TrimLeft(parts[0], "0123456789[] ")
	mobile := parts[1]
	
	return contact{name: name, mobile: mobile}, true
}

func pickContactFzf(contacts []contact) (contact, bool) {
	if len(contacts) == 0 {
		return contact{}, false
	}
	var lines []string
	for _, c := range contacts {
		lines = append(lines, fmt.Sprintf("%s\t%s", c.name, c.mobile))
	}

	cmd := exec.Command("fzf",
		"--prompt=Novo contato: ",
		"--delimiter=\t",
		"--with-nth=1",
		"--header=Enter para abrir  Esc para sair",
	)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return contact{}, false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		return contact{}, false
	}
	return contact{name: parts[0], mobile: parts[1]}, true
}

// ─── Modelo Bubble Tea ────────────────────────────────────────────────────────

type incomingMsg struct {
	sender  string
	content string
	isMe    bool
	ts      time.Time
}

type model struct {
	client    *whatsmeow.Client
	target    contact
	phone     string
	history   []storedMsg
	viewport  viewport.Model
	textInput textinput.Model
	width     int
	height    int
	ready     bool
}

func newModel(client *whatsmeow.Client, target contact) model {
	phone := cleanNumber(target.mobile)

	ti := textinput.New()
	ti.Placeholder = "mensagem..."
	ti.Focus()
	ti.CharLimit = 1000

	// Marcar como lido ao abrir
	meta := loadMetadata(phone)
	meta.LastRead = time.Now()
	saveMetadata(phone, meta)

	return model{
		client:    client,
		target:    target,
		phone:     phone,
		history:   loadHistory(phone),
		textInput: ti,
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		vpH := m.height - 4
		if vpH < 1 {
			vpH = 1
		}
		if !m.ready {
			m.viewport = viewport.New(m.width, vpH)
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = vpH
		}
		m.viewport.SetContent(m.renderHistory())
		m.viewport.GotoBottom()

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			saveHistory(m.phone, m.history)
			// Atualizar última leitura ao sair
			meta := loadMetadata(m.phone)
			meta.LastRead = time.Now()
			saveMetadata(m.phone, meta)
			return m, tea.Quit

		case tea.KeyEnter:
			text := strings.TrimSpace(m.textInput.Value())
			if text == "" {
				break
			}
			jid, err := types.ParseJID(m.phone + "@s.whatsapp.net")
			if err == nil {
				go m.client.SendMessage(context.Background(), jid, &waE2E.Message{
					Conversation: proto.String(text),
				})
			}
			entry := storedMsg{Sender: "eu", Content: text, IsMe: true, Time: time.Now()}
			m.history = append(m.history, entry)
			saveHistory(m.phone, m.history)
			m.textInput.Reset()
			m.viewport.SetContent(m.renderHistory())
			m.viewport.GotoBottom()
		}

	case incomingMsg:
		entry := storedMsg{
			Sender:  msg.sender,
			Content: msg.content,
			IsMe:    msg.isMe,
			Time:    msg.ts,
		}
		m.history = append(m.history, entry)
		saveHistory(m.phone, m.history)
		m.viewport.SetContent(m.renderHistory())
		m.viewport.GotoBottom()
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	var tiCmd tea.Cmd
	m.textInput, tiCmd = m.textInput.Update(msg)
	cmds = append(cmds, tiCmd)

	return m, tea.Batch(cmds...)
}

func (m model) renderHistory() string {
	if len(m.history) == 0 {
		return styleTimestamp.Render("  sem mensagens ainda")
	}

	var sb strings.Builder
	lastDate := ""

	for _, msg := range m.history {
		date := msg.Time.Format("02 Jan 2006")
		if date != lastDate {
			lastDate = date
			pad := (m.width - len(date) - 4) / 2
			if pad < 1 {
				pad = 1
			}
			line := strings.Repeat("─", pad) + "  " + date + "  " + strings.Repeat("─", pad)
			sb.WriteString("\n" + styleDivider.Render(line) + "\n\n")
		}

		ts := styleTimestamp.Render(msg.Time.Format("15:04"))

		if msg.IsMe {
			name := styleMyName.Render("eu")
			content := styleMyMsg.Render(msg.Content)
			sb.WriteString(fmt.Sprintf(" %s %s  %s\n", name, ts, content))
		} else {
			name := styleTheirName.Render(msg.Sender)
			content := styleTheirMsg.Render(msg.Content)
			sb.WriteString(fmt.Sprintf(" %s %s  %s\n", name, ts, content))
		}
	}
	return sb.String()
}

func (m model) View() string {
	if !m.ready {
		return ""
	}

	header := styleHeader.Width(m.width).Render(
		fmt.Sprintf("zap  ─  %s  (%s)", m.target.name, m.target.mobile),
	)

	chat := m.viewport.View()

	prompt := styleInputPrompt.Render(" > ")
	inputArea := styleInputBar.Width(m.width).Render(prompt + m.textInput.View())

	status := styleStatus.Width(m.width).Render(
		fmt.Sprintf(" %d mensagens  │  PgUp/PgDn rolar  │  Ctrl+C sair", len(m.history)),
	)

	return lipgloss.JoinVertical(lipgloss.Left, header, chat, inputArea, status)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	contacts := loadAbook()
	chats := getChats(contacts)

	var chosen contact
	var ok bool

	if len(chats) > 0 {
		chosen, ok = pickChatFzf(chats)
	} else {
		chosen, ok = pickContactFzf(contacts)
	}

	if !ok {
		return
	}

	exePath, _ := os.Executable()
	realPath, _ := filepath.EvalSymlinks(exePath)
	dbPath := filepath.Join(filepath.Dir(realPath), "whatsapp.db")
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)

	container, err := sqlstore.New(
		context.Background(), "sqlite3", dbURI,
		waLog.Stdout("Database", "ERROR", true),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro ao abrir db:", err)
		os.Exit(1)
	}

	device, _ := container.GetFirstDevice(context.Background())
	client := whatsmeow.NewClient(device, waLog.Stdout("Client", "ERROR", true))

	m := newModel(client, chosen)
	p := tea.NewProgram(m, tea.WithAltScreen())

	phoneClean := cleanNumber(chosen.mobile)

	client.AddEventHandler(func(evt interface{}) {
		v, ok := evt.(*events.Message)
		if !ok {
			return
		}

		txt := v.Message.GetConversation()
		if txt == "" {
			txt = v.Message.GetExtendedTextMessage().GetText()
		}
		if txt == "" {
			return
		}

		senderClean := cleanNumber(v.Info.Sender.User)
		mine := v.Info.IsFromMe

		relevant := mine
		if !mine && len(senderClean) >= 8 && len(phoneClean) >= 8 {
			relevant = senderClean[len(senderClean)-8:] == phoneClean[len(phoneClean)-8:]
		}
		if !relevant {
			return
		}

		senderName := chosen.name
		if mine {
			senderName = "eu"
		}

		p.Send(incomingMsg{
			sender:  senderName,
			content: txt,
			isMe:    mine,
			ts:      v.Info.Timestamp,
		})
	})

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

	if _, err := p.Run(); err != nil {
		os.Exit(1)
	}
	client.Disconnect()
}
