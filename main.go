package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
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

var (
	senderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	receiverStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	systemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	boxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).BorderForeground(lipgloss.Color("63"))
)

type chatMsg struct {
	sender  string
	content string
	isMe    bool
}

type model struct {
	client    *whatsmeow.Client
	history   []chatMsg
	textInput textinput.Model
	target    string
	name      string
	mode      string
}

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.mode == "send" && msg.Type == tea.KeyEnter {
			input := strings.TrimSpace(m.textInput.Value())
			if input != "" && m.target != "" {
				jid, _ := types.ParseJID(m.target + "@s.whatsapp.net")
				go m.client.SendMessage(context.Background(), jid, &waE2E.Message{Conversation: proto.String(input)})
				m.history = append(m.history, chatMsg{sender: "Você", content: input, isMe: true})
				m.textInput.Reset()
			}
		}
	case chatMsg:
		m.history = append(m.history, msg)
	}
	var cmd tea.Cmd
	if m.mode == "send" {
		m.textInput, cmd = m.textInput.Update(msg)
	}
	return m, cmd
}

func (m model) View() string {
	var body strings.Builder
	for _, c := range m.history {
		style := receiverStyle
		if c.isMe {
			style = senderStyle
		}
		body.WriteString(fmt.Sprintf("%s %s\n", style.Render(c.sender+":"), c.content))
	}

	header := "[ Monitor de Mensagens ]"
	if m.mode == "send" {
		header = fmt.Sprintf("[ Chat: %s ]", m.name)
	}

	view := systemStyle.Render(header) + "\n" + boxStyle.Width(65).Height(15).Render(body.String())
	footer := systemStyle.Render("\n Ctrl+C para encerrar")
	
	if m.mode == "send" {
		return view + footer + "\n" + m.textInput.View()
	}
	return view + footer
}

func getContactViaFzf() (string, string) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".local/share/contatos")
	
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte("Nome,Telefone,Email\nExemplo,+5500000000000,\n"), 0644)
	}

	cmdStr := fmt.Sprintf("tail -n +2 %s | fzf --header 'Selecionar Contato' --delimiter ',' --with-nth 1", path)
	out, err := exec.Command("sh", "-c", cmdStr).Output()
	if err != nil { return "", "" }

	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) < 2 { return "", "" }
	
	num := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' { return r }
		return -1
	}, parts[1])

	return parts[0], num
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Uso: zap-tui [send|receive]")
		return
	}

	mode := os.Args[1]
	var contactName, contactPhone string

	if mode == "send" {
		contactName, contactPhone = getContactViaFzf()
		if contactPhone == "" { return }
	}

	// Localiza o diretório real do binário para persistência do DB
	exePath, _ := os.Executable()
	realPath, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		realPath = exePath
	}
	dbPath := filepath.Join(filepath.Dir(realPath), "whatsapp.db")
	dbURI := fmt.Sprintf("file:%s?_foreign_keys=on", dbPath)

	container, err := sqlstore.New(context.Background(), "sqlite3", dbURI, waLog.Stdout("Database", "ERROR", true))
	if err != nil {
		panic(fmt.Errorf("erro ao abrir banco de dados (%s): %w", dbPath, err))
	}

	device, _ := container.GetFirstDevice(context.Background())
	client := whatsmeow.NewClient(device, waLog.Stdout("Client", "ERROR", true))

	ti := textinput.New()
	ti.Placeholder = "Mensagem..."
	ti.Focus()

	p := tea.NewProgram(model{
		client:    client,
		textInput: ti,
		target:    contactPhone,
		name:      contactName,
		mode:      mode,
	})

	client.AddEventHandler(func(evt interface{}) {
		if v, ok := evt.(*events.Message); ok {
			txt := v.Message.GetConversation()
			if txt == "" {
				txt = v.Message.GetExtendedTextMessage().GetText()
			}
			if txt != "" {
				p.Send(chatMsg{sender: v.Info.Sender.User, content: txt, isMe: false})
			}
		}
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
		fmt.Fprintf(os.Stderr, "Erro: %v\n", err)
		os.Exit(1)
	}
	
	client.Disconnect()
}
