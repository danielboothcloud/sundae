package header

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// Header represents a header component.
type Header struct {
	common common.Common
	text   string
	right  func(width int) string
}

// SetRight sets what the header shows at its right end, e.g. the
// attention summary, rendered in at most width cells; nil or "" shows
// nothing.
func (h *Header) SetRight(fit func(width int) string) { h.right = fit }

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
	name := st.UnsetMarginBottom().Render(strings.TrimSpace(h.text))
	right := ""
	if room := h.common.Width - lipgloss.Width(name) - 1; h.right != nil && room > 0 {
		right = h.right(room)
	}
	if right == "" {
		return st.Render(strings.TrimSpace(h.text))
	}
	gap := max(h.common.Width-lipgloss.Width(name)-lipgloss.Width(right), 1)
	return lipgloss.NewStyle().
		MarginBottom(st.GetMarginBottom()).
		Render(name + strings.Repeat(" ", gap) + right)
}
