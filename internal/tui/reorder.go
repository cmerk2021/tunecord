package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ReorderModel is a small Bubble Tea model to reorder a list of strings.
//
// Keys: ↑/↓ (k/j) move the cursor; shift+↑/↓ (K/J) or ctrl+↑/↓ move the
// selected item; space grabs/drops the item so plain ↑/↓ move it; d/delete
// removes the item (when AllowRemove is set); enter confirms; esc/ctrl+c aborts.
type ReorderModel struct {
	Title       string
	Note        string
	Items       []string
	Labels      map[string]string // optional extra text shown next to items
	AllowRemove bool

	cursor  int
	grabbed bool
	done    bool
	aborted bool
}

// NewReorder creates a reorder model.
func NewReorder(title, note string, items []string) *ReorderModel {
	return &ReorderModel{Title: title, Note: note, Items: append([]string(nil), items...)}
}

// Done reports whether the user confirmed.
func (m *ReorderModel) Done() bool { return m.done }

// Aborted reports whether the user cancelled.
func (m *ReorderModel) Aborted() bool { return m.aborted }

// Cursor returns the cursor index.
func (m *ReorderModel) Cursor() int { return m.cursor }

// Init implements tea.Model.
func (m *ReorderModel) Init() tea.Cmd { return nil }

func (m *ReorderModel) move(delta int) {
	j := m.cursor + delta
	if j < 0 || j >= len(m.Items) {
		return
	}
	m.Items[m.cursor], m.Items[j] = m.Items[j], m.Items[m.cursor]
	m.cursor = j
}

func (m *ReorderModel) step(delta int) {
	j := m.cursor + delta
	if j >= 0 && j < len(m.Items) {
		m.cursor = j
	}
}

// Key applies a key name (as produced by tea.KeyMsg.String()). It returns
// true when the model is finished.
func (m *ReorderModel) Key(k string) bool {
	switch k {
	case "ctrl+c", "esc":
		m.aborted = true
		return true
	case "enter":
		m.done = true
		return true
	case "up", "k":
		if m.grabbed {
			m.move(-1)
		} else {
			m.step(-1)
		}
	case "down", "j":
		if m.grabbed {
			m.move(1)
		} else {
			m.step(1)
		}
	case "shift+up", "ctrl+up", "K", "alt+up":
		m.move(-1)
	case "shift+down", "ctrl+down", "J", "alt+down":
		m.move(1)
	case "home", "g":
		if m.grabbed {
			for m.cursor > 0 {
				m.move(-1)
			}
		} else {
			m.cursor = 0
		}
	case "end", "G":
		if m.grabbed {
			for m.cursor < len(m.Items)-1 {
				m.move(1)
			}
		} else if len(m.Items) > 0 {
			m.cursor = len(m.Items) - 1
		}
	case " ", "space":
		m.grabbed = !m.grabbed
	case "d", "delete", "x":
		if m.AllowRemove && len(m.Items) > 0 {
			m.Items = append(m.Items[:m.cursor], m.Items[m.cursor+1:]...)
			if m.cursor >= len(m.Items) && m.cursor > 0 {
				m.cursor--
			}
			m.grabbed = false
		}
	}
	return false
}

// Update implements tea.Model.
func (m *ReorderModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		if m.Key(k.String()) {
			return m, tea.Quit
		}
	}
	return m, nil
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	noteStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	grabStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("212")).Bold(true)
)

// View implements tea.Model.
func (m *ReorderModel) View() string {
	if m.done || m.aborted {
		return ""
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.Title) + "\n")
	if m.Note != "" {
		b.WriteString(noteStyle.Render(m.Note) + "\n")
	}
	b.WriteString("\n")
	if len(m.Items) == 0 {
		b.WriteString(noteStyle.Render("  (empty)") + "\n")
	}
	for i, it := range m.Items {
		line := fmt.Sprintf("%2d. %s", i+1, it)
		if l := m.Labels[it]; l != "" {
			line += noteStyle.Render("  " + l)
		}
		switch {
		case i == m.cursor && m.grabbed:
			b.WriteString(grabStyle.Render("⇕ "+line) + "\n")
		case i == m.cursor:
			b.WriteString(cursorStyle.Render("> "+line) + "\n")
		default:
			b.WriteString("  " + line + "\n")
		}
	}
	help := "↑/↓ select • shift+↑/↓ or K/J move • space grab/drop • enter confirm • esc cancel"
	if m.AllowRemove {
		help = "↑/↓ select • shift+↑/↓ or K/J move • space grab/drop • d remove • enter confirm • esc cancel"
	}
	b.WriteString("\n" + noteStyle.Render(help) + "\n")
	return b.String()
}

// RunReorder runs the reorder UI and returns the new order.
func RunReorder(m *ReorderModel) ([]string, error) {
	if len(m.Items) < 2 && !m.AllowRemove {
		return m.Items, nil
	}
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return nil, err
	}
	if m.aborted {
		return nil, ErrAborted
	}
	return m.Items, nil
}
