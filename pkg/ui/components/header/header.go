package header

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Header represents a header component.
type Header struct {
	common common.Common
	text   string
	right  string
}

// SetRight sets what the header shows at its right end, e.g. the
// attention summary; "" shows nothing.
func (h *Header) SetRight(s string) { h.right = s }

// New creates a new header component.
func New(c common.Common, text string) *Header {
	return &Header{
		common: c,
		text:   text,
	}
}

// SetSize implements common.Component.
func (h *Header) SetSize(width, height int) {
	h.common.SetSize(width, height)
}

// Init implements tea.Model.
func (h *Header) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (h *Header) Update(_ tea.Msg) (common.Model, tea.Cmd) {
	return h, nil
}

// View implements tea.Model.
func (h *Header) View() string {
	st := h.common.Styles.ServerName
	if h.right == "" {
		return st.Render(strings.TrimSpace(h.text))
	}
	name := st.UnsetMarginBottom().Render(strings.TrimSpace(h.text))
	right := h.right
	gap := h.common.Width - lipgloss.Width(name) - lipgloss.Width(right)
	if gap < 1 {
		right = ""
		gap = 0
	}
	return lipgloss.NewStyle().
		MarginBottom(st.GetMarginBottom()).
		Render(name + strings.Repeat(" ", gap) + right)
}
