// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/agent"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/api"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/klog/v2"
)

const logo = `
 _          _               _   _             _
| | ___   _| |__   ___  ___| |_| |       __ _(_)
| |/ / | | | '_ \ / _ \/ __| __| |_____ / _  | |
|   <| |_| | |_) |  __/ (__| |_| |_____| (_| | |
|_|\_\\__,_|_.__/ \___|\___|\__|_|      \__,_|_|
`

// Color palette - Google Material Design colors
var (
	colorPrimary   = lipgloss.Color("#8AB4F8") // Blue 200
	colorSecondary = lipgloss.Color("#81C995") // Green 200
	colorError     = lipgloss.Color("#F28B82") // Red 200
	colorWarning   = lipgloss.Color("#FDD663") // Yellow 200
	colorText      = lipgloss.Color("#E8EAED") // Grey 200
	colorMuted     = lipgloss.Color("#9AA0A6") // Grey 500
	colorDim       = lipgloss.Color("#5F6368") // Grey 700
	colorBgSubtle  = lipgloss.Color("#303134") // Surface variant
	colorBgCode    = lipgloss.Color("#1E1E1E") // Code background
)

// Styles - consolidated for reuse
var (
	textStyle   = lipgloss.NewStyle().Foreground(colorText)
	mutedStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	dimStyle    = lipgloss.NewStyle().Foreground(colorDim)
	primaryText = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	successText = lipgloss.NewStyle().Foreground(colorSecondary).Bold(true)
	errorText   = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	warnText    = lipgloss.NewStyle().Foreground(colorWarning).Bold(true)

	statusBar = lipgloss.NewStyle().Background(colorBgSubtle).Foreground(colorText)

	userMsg = lipgloss.NewStyle().
		BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(colorPrimary).PaddingLeft(1).MarginBottom(1)
	agentMsg = lipgloss.NewStyle().
			BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(colorSecondary).PaddingLeft(1).MarginBottom(1)

	toolBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colorSecondary).
		Padding(0, 1).MarginBottom(1)
	errorBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(colorError).
			Padding(0, 1).MarginBottom(1)
	inputBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPrimary).Padding(0, 1)
	inputBoxDim = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorDim).Padding(0, 1)
	codeStyle   = lipgloss.NewStyle().Foreground(colorText).Background(colorBgCode).Padding(0, 1)
)

// List item for choice selection
type item string

func (i item) FilterValue() string { return "" }

type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 1 }
func (d itemDelegate) Spacing() int                            { return 0 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, idx int, li list.Item) {
	s, ok := li.(item)
	if !ok {
		return
	}
	if idx == m.Index() {
		fmt.Fprint(w, primaryText.Render("> "+string(s)))
	} else {
		fmt.Fprint(w, mutedStyle.PaddingLeft(2).Render(string(s)))
	}
}

// TUI is the terminal user interface for the agent.
type TUI struct {
	program *tea.Program
	agent   *agent.Agent
}

func NewTUI(agent *agent.Agent) *TUI {
	return &TUI{
		program: tea.NewProgram(newModel(agent), tea.WithAltScreen(), tea.WithMouseAllMotion()),
		agent:   agent,
	}
}

func (u *TUI) Run(ctx context.Context) error {
	// Suppress stderr to prevent klog from breaking TUI
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		orig := os.Stderr
		os.Stderr = devNull
		defer func() { os.Stderr = orig; devNull.Close() }()
	}
	klog.SetOutput(io.Discard)
	klog.LogToStderr(false)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-u.agent.Output:
				if !ok {
					return
				}
				u.program.Send(msg)
			}
		}
	}()

	_, err := u.program.Run()
	return err
}

func (u *TUI) ClearScreen() {}

type sessionListMsg []api.SessionInfo

func (m *model) fetchSessions() tea.Msg {
	sessions, err := m.agent.ListSessions()
	if err != nil {
		return api.Message{
			Type:    api.MessageTypeError,
			Payload: fmt.Sprintf("Failed to list sessions: %v", err),
		}
	}
	return sessionListMsg(sessions)
}

type tickMsg time.Time

// Render cache for markdown
type renderCache struct {
	mu       sync.RWMutex
	cache    map[string]string
	width    int
	renderer *glamour.TermRenderer
}

func newRenderCache() *renderCache {
	return &renderCache{cache: make(map[string]string)}
}

func (rc *renderCache) get(id string) (string, bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	v, ok := rc.cache[id]
	return v, ok
}

func (rc *renderCache) set(id, content string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.cache[id] = content
}

func (rc *renderCache) getRenderer(width int) (*glamour.TermRenderer, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.width != width {
		rc.cache = make(map[string]string)
		rc.width = width
		rc.renderer = nil
	}
	if rc.renderer == nil {
		r, err := glamour.NewTermRenderer(glamour.WithStylePath("dark"), glamour.WithWordWrap(width))
		if err != nil {
			return nil, err
		}
		rc.renderer = r
	}
	return rc.renderer, nil
}

// Model state
type model struct {
	agent      *agent.Agent
	viewport   viewport.Model
	input      textinput.Model
	spinner    spinner.Model
	list       list.Model
	cache      *renderCache
	messages   []*api.Message
	width      int
	height     int
	dirty      bool
	quitting   bool
	thinkStart time.Time
	// Choice mode tracking
	inChoiceMode   bool
	choicePrompt   string
	choiceOptionID string // Track which choice request we initialized for
	choiceType     string // "confirm" or "session"
	sessionIDs     []string
}

func newModel(agent *agent.Agent) model {
	ti := textinput.New()
	ti.Placeholder = "Ask kubectl-ai anything..."
	ti.Focus()
	ti.Prompt = ""
	ti.CharLimit = 4096
	ti.Width = 80
	ti.TextStyle = textStyle
	ti.PlaceholderStyle = dimStyle
	ti.Cursor.Style = primaryText

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = primaryText

	l := list.New(nil, itemDelegate{}, 40, 5)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetShowTitle(false)

	vp := viewport.New(80, 20)
	vp.MouseWheelEnabled = true

	return model{
		agent:    agent,
		input:    ti,
		viewport: vp,
		spinner:  sp,
		list:     l,
		cache:    newRenderCache(),
		dirty:    true,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.spinner.Tick, m.tick())
}

func (m model) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.dirty = true
		m.resize()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.viewport.ScrollUp(3)
		case tea.MouseButtonWheelDown:
			m.viewport.ScrollDown(3)
		}
		return m, nil

	case *api.Message:
		return m.handleAgentMsg(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tickMsg:
		return m, m.tick()

	case sessionListMsg:
		if len(msg) == 0 {
			m.messages = append(m.messages, &api.Message{
				Source:    api.MessageSourceAgent,
				Type:      api.MessageTypeText,
				Payload:   "No sessions found.",
				Timestamp: time.Now(),
			})
			m.dirty = true
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}

		items := make([]list.Item, len(msg))
		ids := make([]string, len(msg))
		for i, s := range msg {
			label := fmt.Sprintf("%s (%s) • %d msgs", s.ID, s.ModelID, s.MessageCount)
			if s.Name != "" {
				label = fmt.Sprintf("%s (%s) • %s • %d msgs", s.Name, s.ModelID, s.ID, s.MessageCount)
			}
			items[i] = item(label)
			ids[i] = s.ID
		}
		m.list.SetItems(items)
		m.list.Select(0)
		m.inChoiceMode = true
		m.choicePrompt = "Select a session to resume"
		m.choiceOptionID = "manual-session-picker"
		m.choiceType = "session"
		m.sessionIDs = ids
		m.dirty = true
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m, nil
}

func (m *model) resize() {
	m.viewport.Width = m.width - 2
	m.input.Width = m.width - 6
	m.list.SetWidth(m.width - 4)
	m.updateViewportHeight()
	m.refresh()
	m.viewport.GotoBottom()
}

func (m *model) updateViewportHeight() {
	// Layout: status(1) + 2 dividers(2) + input(3) + help(1) + bottom padding(1) = 8
	contentH := m.height - 8

	contentH = max(contentH, 5)
	m.viewport.Height = contentH
}

func (m *model) navigateList(keyType tea.KeyType) tea.Cmd {
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(tea.KeyMsg{Type: keyType})
	m.dirty = true
	m.refresh()
	return cmd
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		m.quitting = true
		return m, tea.Quit
	case tea.KeyEsc:
		m.input.Reset()
		return m, nil
	case tea.KeyEnter:
		return m.handleEnter()
	case tea.KeyUp:
		if m.inChoiceMode {
			return m, m.navigateList(tea.KeyUp)
		}
		m.viewport.ScrollUp(1)
	case tea.KeyDown:
		if m.inChoiceMode {
			return m, m.navigateList(tea.KeyDown)
		}
		m.viewport.ScrollDown(1)
	case tea.KeyPgUp:
		m.viewport.ScrollUp(m.viewport.Height / 2)
	case tea.KeyPgDown:
		m.viewport.ScrollDown(m.viewport.Height / 2)
	default:
		switch msg.String() {
		case "ctrl+u":
			m.viewport.ScrollUp(m.viewport.Height / 2)
		case "ctrl+d":
			m.viewport.ScrollDown(m.viewport.Height / 2)
		case "j":
			if m.inChoiceMode {
				return m, m.navigateList(tea.KeyDown)
			}
		case "k":
			if m.inChoiceMode {
				return m, m.navigateList(tea.KeyUp)
			}
		}
		// Default: send to text input
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) handleEnter() (tea.Model, tea.Cmd) {
	// Handle choice selection
	if m.inChoiceMode {
		if _, ok := m.list.SelectedItem().(item); ok {
			if m.choiceType == "session" {
				idx := m.list.Index()
				if idx >= 0 && idx < len(m.sessionIDs) {
					selectedID := m.sessionIDs[idx]
					m.inChoiceMode = false
					m.choicePrompt = ""
					m.choiceOptionID = ""
					// Don't reset choiceType/sessionIDs yet or it might race, but actually we are done.
					m.dirty = true
					m.refresh()
					return m, func() tea.Msg {
						m.agent.Input <- &api.SessionPickerResponse{SessionID: selectedID}
						return nil
					}
				}
			} else {
				choice := m.list.Index() + 1
				m.inChoiceMode = false
				m.choicePrompt = ""
				m.choiceOptionID = ""
				m.dirty = true
				m.refresh()
				return m, func() tea.Msg {
					m.agent.Input <- &api.UserChoiceResponse{Choice: choice}
					return nil
				}
			}
		}
		return m, nil
	}

	value := strings.TrimSpace(m.input.Value())
	if value == "" {
		return m, nil
	}

	// Add user message
	m.messages = append(m.messages, &api.Message{
		Source:    api.MessageSourceUser,
		Type:      api.MessageTypeText,
		Payload:   value,
		Timestamp: time.Now(),
	})
	m.input.Reset()
	m.dirty = true
	m.refresh()
	m.viewport.GotoBottom()

	// Intercept "sessions" command
	if value == "sessions" {
		return m, m.fetchSessions
	}

	m.thinkStart = time.Now()

	return m, func() tea.Msg {
		m.agent.Input <- &api.UserInputResponse{Query: value}
		return nil
	}
}

func (m *model) handleAgentMsg(msg *api.Message) (tea.Model, tea.Cmd) {
	session := m.agent.GetSession()
	m.messages = session.AllMessages()
	m.dirty = true

	// Check if we're entering choice mode - use the incoming message directly
	// to avoid race conditions where the message isn't yet in AllMessages()
	if msg.Type == api.MessageTypeUserChoiceRequest {
		if req, ok := msg.Payload.(*api.UserChoiceRequest); ok {
			items := make([]list.Item, len(req.Options))
			for i, opt := range req.Options {
				items[i] = item(opt.Label)
			}
			m.list.SetItems(items)
			m.list.Select(0)
			m.inChoiceMode = true
			m.choicePrompt = req.Prompt
			m.choiceOptionID = msg.ID
			m.choiceType = "confirm"
		}
	} else if msg.Type == api.MessageTypeSessionPickerRequest {
		if req, ok := msg.Payload.(*api.SessionPickerRequest); ok {
			items := make([]list.Item, len(req.Sessions))
			ids := make([]string, len(req.Sessions))
			for i, s := range req.Sessions {
				label := fmt.Sprintf("%s (%s) • %d msgs", s.ID, s.ModelID, s.MessageCount)
				if s.Name != "" {
					label = fmt.Sprintf("%s (%s) • %s • %d msgs", s.Name, s.ModelID, s.ID, s.MessageCount)
				}
				items[i] = item(label)
				ids[i] = s.ID
			}
			m.list.SetItems(items)
			m.list.Select(0)
			m.inChoiceMode = true
			m.choicePrompt = "Select a session to resume"
			m.choiceOptionID = msg.ID
			m.choiceType = "session"
			m.sessionIDs = ids
		}
	} else if session.AgentState == api.AgentStateDone || session.AgentState == api.AgentStateExited {
		// Clear choice mode if we're done or exited
		m.inChoiceMode = false
		m.choicePrompt = ""
		m.choiceOptionID = ""
	}

	m.refresh()
	m.viewport.GotoBottom()

	if session.AgentState == api.AgentStateRunning || session.AgentState == api.AgentStateInitializing {
		return m, m.spinner.Tick
	}
	return m, nil
}

func (m *model) refresh() {
	if !m.dirty {
		return
	}
	m.viewport.SetContent(m.renderMessages())
	m.dirty = false
}

func (m model) renderMessages() string {
	var sb strings.Builder

	if len(m.messages) == 0 {
		sb.WriteString(fmt.Sprintf("\n%s\n\n%s\n%s\n",
			primaryText.Render(logo),
			mutedStyle.PaddingLeft(1).Render("Your AI-powered Kubernetes assistant"),
			dimStyle.PaddingLeft(1).Render("Type a message to get started")))
	} else {
		width := min(m.viewport.Width-6, 90)
		if width < 40 {
			width = 40
		}

		renderer, err := m.cache.getRenderer(width)
		if err != nil {
			return "Error rendering messages"
		}

		for _, msg := range m.messages {
			if s := m.renderMessage(msg, renderer, width); s != "" {
				sb.WriteString(s)
			}
		}
	}

	// Render choice picker inline at the end of messages
	if m.inChoiceMode {
		sb.WriteString("\n")
		sb.WriteString(warnText.Render("? " + m.choicePrompt))
		sb.WriteString("\n\n")
		sb.WriteString(m.list.View())
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m model) renderMessage(msg *api.Message, r *glamour.TermRenderer, w int) string {
	// Skip certain message types
	if msg.Type == api.MessageTypeUserInputRequest {
		if p, ok := msg.Payload.(string); ok && p == ">>>" {
			return ""
		}
	}
	if msg.Type == api.MessageTypeToolCallResponse {
		return ""
	}
	// Skip choice requests - they're rendered in the input area instead
	if msg.Type == api.MessageTypeUserChoiceRequest || msg.Type == api.MessageTypeSessionPickerRequest {
		return ""
	}

	// Check cache (except tool calls which show status)
	if msg.ID != "" && msg.Type != api.MessageTypeToolCallRequest {
		if cached, ok := m.cache.get(msg.ID); ok {
			return cached
		}
	}

	var result string
	switch msg.Type {
	case api.MessageTypeToolCallRequest:
		result = m.renderToolCall(msg, w)
	case api.MessageTypeError:
		result = m.renderError(msg, w)
	default:
		result = m.renderTextMsg(msg, r, w)
	}

	// Cache result
	if msg.ID != "" && result != "" && msg.Type != api.MessageTypeToolCallRequest {
		m.cache.set(msg.ID, result)
	}
	return result
}

func (m model) renderTextMsg(msg *api.Message, r *glamour.TermRenderer, w int) string {
	payload, ok := msg.Payload.(string)
	if !ok {
		return ""
	}

	ts := ""
	if !msg.Timestamp.IsZero() {
		ts = dimStyle.Italic(true).Render(" " + msg.Timestamp.Format("15:04"))
	}

	switch msg.Source {
	case api.MessageSourceUser:
		label := primaryText.Render("You") + ts
		content := textStyle.Width(w).Render(payload)
		return userMsg.Width(w+2).Render(label+"\n"+content) + "\n"
	case api.MessageSourceModel, api.MessageSourceAgent:
		label := successText.Render("kubectl-ai") + ts
		rendered, _ := r.Render(payload)
		return agentMsg.Width(w+2).Render(label+"\n"+strings.TrimSpace(rendered)) + "\n"
	}
	return ""
}

func (m model) renderToolCall(msg *api.Message, w int) string {
	payload, ok := msg.Payload.(string)
	if !ok {
		return ""
	}
	content := successText.Render("⚡ Running") + "\n" + codeStyle.Render(payload)
	return toolBox.Width(w).Render(content) + "\n"
}

func (m model) renderError(msg *api.Message, w int) string {
	payload, ok := msg.Payload.(string)
	if !ok {
		return ""
	}
	content := errorText.Render("✗ Error") + "\n" + errorText.Render(payload)
	return errorBox.Width(w).Render(content) + "\n"
}

func (m model) View() string {
	if m.quitting {
		return mutedStyle.Padding(1).Render("Goodbye!")
	}

	session := m.agent.GetSession()
	return lipgloss.JoinVertical(lipgloss.Left,
		m.viewStatus(session),
		m.viewDivider(),
		lipgloss.NewStyle().PaddingLeft(1).Render(m.viewport.View()),
		m.viewDivider(),
		m.viewInput(session.AgentState),
		m.viewHelp(session.AgentState),
	)
}

func (m model) viewStatus(session *api.Session) string {
	sep := dimStyle.Render(" | ")

	name := session.Name
	if name == "" {
		name = session.ID
	}
	left := primaryText.Render("kubectl-ai") + sep + mutedStyle.Render(name) + sep + m.viewState(session.AgentState)

	model := session.ModelID
	if model == "" {
		model = "unknown"
	}
	right := lipgloss.NewStyle().Foreground(colorSecondary).Render(model)

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}
	return statusBar.Width(m.width).Render(" " + left + strings.Repeat(" ", gap) + right + " ")
}

func (m model) viewState(state api.AgentState) string {
	states := map[api.AgentState]struct {
		icon, text string
		style      lipgloss.Style
	}{
		api.AgentStateRunning:         {"●", "Running", successText},
		api.AgentStateInitializing:    {"", "Initializing...", mutedStyle},
		api.AgentStateWaitingForInput: {"●", "Ready", successText},
		api.AgentStateIdle:            {"○", "Idle", mutedStyle},
		api.AgentStateDone:            {"✓", "Done", successText},
		api.AgentStateExited:          {"○", "Exited", mutedStyle},
	}

	if s, ok := states[state]; ok {
		txt := s.style.Render(s.icon + " " + s.text)
		if state == api.AgentStateRunning && !m.thinkStart.IsZero() {
			txt += mutedStyle.Render(" " + formatDuration(time.Since(m.thinkStart)))
		}
		return txt
	}
	return mutedStyle.Render(string(state))
}

func (m model) viewDivider() string {
	return dimStyle.Render(strings.Repeat("─", m.width))
}

func (m model) viewInput(state api.AgentState) string {
	// Show dimmed input hint when in choice mode (picker is inline above)
	if m.inChoiceMode {
		content := mutedStyle.Render("Use ↑/↓ to navigate, Enter to select")
		return lipgloss.NewStyle().Padding(0, 1).Render(inputBoxDim.Width(m.width - 4).Render(content))
	}

	// Show spinner or input
	if state == api.AgentStateRunning || state == api.AgentStateInitializing {
		elapsed := ""
		if !m.thinkStart.IsZero() {
			elapsed = " " + formatDuration(time.Since(m.thinkStart))
		}
		content := primaryText.Render(m.spinner.View()+" Thinking...") + mutedStyle.Render(elapsed)
		return lipgloss.NewStyle().Padding(0, 1).Render(inputBoxDim.Width(m.width - 4).Render(content))
	}

	return lipgloss.NewStyle().Padding(0, 1).Render(inputBox.Width(m.width - 4).Render(m.input.View()))
}

func (m model) viewHelp(state api.AgentState) string {
	var hints []string
	if m.inChoiceMode {
		hints = []string{"↑/↓: navigate", "Enter: select", "Ctrl+C: quit"}
	} else if state == api.AgentStateRunning {
		hints = []string{"Ctrl+C: cancel"}
	} else {
		hints = []string{"Enter: send", "Esc: clear", "Ctrl+C: quit"}
		if m.viewport.TotalLineCount() > m.viewport.Height {
			hints = append(hints, "↑/↓: scroll")
		}
	}
	return dimStyle.Padding(0, 2, 1, 2).Render(strings.Join(hints, " • "))
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
