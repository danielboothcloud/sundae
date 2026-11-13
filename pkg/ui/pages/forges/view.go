package forges

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
)

// Repository row columns (wide layout).
const (
	aheadWidth = 12 // "not imported"
	lastWidth  = 15 // "✗ import failed"
)

// View implements tea.Model.
func (m *Forges) View() string {
	if m.src == nil {
		return ""
	}
	w, h := m.common.Width, m.common.Height
	desc := m.common.Styles.RepoSelector.Normal.Desc
	lines := []string{""}
	switch {
	case m.typing:
		m.filter.SetWidth(max(w-4, 8))
		lines = append(lines, " "+m.filter.View(), "")
	case m.query != "":
		shown, total := 0, 0
		for _, it := range m.items() {
			if it.kind == repoRow {
				shown++
			}
		}
		for _, g := range m.groups {
			total += len(g.repos)
		}
		lines = append(lines, " "+desc.Render(common.TruncateString(fmt.Sprintf("/%s · %d of %d repositories", m.query, shown, total), w-1)), "")
	}
	bottom := m.bottomLine(w)
	body := h - len(lines)
	if bottom != "" {
		body -= 2
	}
	lines = append(lines, m.listView(w, max(body, 1))...)
	if bottom != "" {
		for len(lines) < h-2 {
			lines = append(lines, "")
		}
		lines = append(lines, "", bottom)
	}
	if len(lines) > h {
		lines = lines[:max(h, 0)]
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// bottomLine is the notice, else the error of the selected row.
func (m *Forges) bottomLine(w int) string {
	st := m.common.Styles
	if m.notice.text != "" {
		style := st.RepoSelector.Normal.Desc
		if m.notice.err {
			style = st.State.Error
		}
		return " " + style.Render(common.TruncateString(m.notice.text, w-1))
	}
	it, ok := m.selected()
	if !ok {
		return ""
	}
	g := m.groups[it.g]
	if it.kind == headerRow {
		// The whole of what went wrong with the forge.
		if text, ok := m.forgeError(g.status); ok {
			return " " + st.State.Error.Render(common.TruncateString("✗ "+g.status.Name+": "+text, w-1))
		}
		return ""
	}
	r := g.repos[it.r]
	text := ""
	if res, ok := m.results[rowKey(g.status.Name, r.Path)]; ok && res.err {
		text = "✗ " + res.detail
	} else if r.LastError != "" && m.busy[rowKey(g.status.Name, r.Path)] == "" {
		text = "✗ Last sync failed: " + r.LastError
	}
	if text == "" {
		return ""
	}
	return " " + st.State.Error.Render(common.TruncateString(oneLine(text), w-1))
}

func (m *Forges) listView(w, h int) []string {
	st := m.common.Styles
	desc := st.RepoSelector.Normal.Desc
	switch {
	case m.statusErr != nil:
		return []string{" " + st.State.Error.Render(common.TruncateString("✗ Cannot read the forges (r to retry): "+oneLine(m.statusErr.Error()), w-1))}
	case !m.loaded:
		return []string{" " + desc.Render("Loading forges…")}
	case len(m.groups) == 0:
		return []string{" " + desc.Render("No forges are configured.")}
	}
	its := m.items()
	if len(its) == 0 {
		return []string{" " + desc.Render(common.TruncateString("No repositories match “"+m.query+"”.", w-1))}
	}
	var all []string
	start, end := 0, 0
	for i, it := range its {
		if it.kind == headerRow && i > 0 {
			all = append(all, "")
		}
		if i == m.cursor {
			start = len(all)
		}
		if it.kind == headerRow {
			all = append(all, m.headerRow(m.groups[it.g], i == m.cursor, w)...)
		} else {
			g := m.groups[it.g]
			all = append(all, m.repoRow(g, g.repos[it.r], i == m.cursor, w)...)
		}
		if i == m.cursor {
			end = len(all)
		}
	}
	// Scroll so the selected row shows.
	if start < m.offset {
		m.offset = start
	}
	if end > m.offset+h {
		m.offset = end - h
	}
	m.offset = clamp(m.offset, 0, max(len(all)-h, 0))
	return all[m.offset:min(m.offset+h, len(all))]
}

// headerRow renders a forge's header: name and host on the left; counts,
// last sync or error on the right (a second line when narrow).
func (m *Forges) headerRow(g group, selected bool, w int) []string {
	st := m.common.Styles
	sel := st.RepoSelector.Normal
	if selected {
		sel = st.RepoSelector.Active
	}
	base := sel.Base.UnsetHeight()
	cw := max(w-base.GetHorizontalFrameSize(), 10)
	s := g.status
	name := s.Name
	if host := hostOf(s.URL); host != "" && host != name {
		name += " · " + host
	}

	// What it is doing, its counts, and its last sync or error.
	var doing, counts, last string
	hk := headerKey(s.Name)
	switch {
	case m.busy[hk] != "":
		doing = m.busy[hk] + "…"
		if p := m.progress[s.Name]; p != nil && p.total > 0 {
			doing += fmt.Sprintf(" %d of %d", min(p.done, p.total), p.total)
		}
		doing = st.State.Working.Render(doing)
	case s.Syncing:
		doing = st.State.Working.Render("syncing…")
	case m.listing[s.Name]:
		doing = st.State.Working.Render("listing…")
	}
	if s.Matching != nil {
		counts = sel.Desc.Render(plural(*s.Matching, "repo", "repos") + " · ")
	}
	counts += sel.Desc.Render(fmt.Sprintf("%d imported", s.Imported))
	if text, ok := m.forgeError(s); ok {
		last = st.State.Error.Render("✗ " + text)
	} else if s.LastSync != nil {
		last = sel.Desc.Render("synced " + ago(*s.LastSync, m.now()))
	} else {
		last = sel.Desc.Render("never synced")
	}
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, sel.Desc.Render(" · "))
	}

	title := sel.Title.Render(common.TruncateString(name, cw))
	if m.narrow() {
		right := ansi.Truncate(join(doing, counts, last), cw-2, "…")
		lines := []string{title, "  " + right}
		lines = append(lines, m.groupNote(g, cw)...)
		return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
	}
	right := join(doing, counts, last)
	room := cw - lipgloss.Width(title) - 2
	if lipgloss.Width(right) > room {
		// Drop the counts before cutting what went wrong.
		right = join(doing, last)
	}
	if lipgloss.Width(right) > room {
		right = ansi.Truncate(right, max(room, 0), "…")
	}
	gap := max(cw-lipgloss.Width(title)-lipgloss.Width(right), 1)
	lines := []string{title + strings.Repeat(" ", gap) + right}
	lines = append(lines, m.groupNote(g, cw)...)
	return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
}

// groupNote says why a forge shows no repositories.
func (m *Forges) groupNote(g group, cw int) []string {
	st := m.common.Styles
	if m.query != "" {
		return nil
	}
	var text string
	style := st.RepoSelector.Normal.Desc
	switch {
	case g.err != nil:
		text, style = "✗ Cannot read its repositories: "+oneLine(g.err.Error()), st.State.Error
	case !g.loaded:
		text = "Loading…"
	case len(g.repos) > 0:
		return nil
	case m.listing[g.status.Name]:
		text = "Listing repositories…"
	case g.status.ListError != "":
		text = "Cannot list its repositories (r to sync and retry)."
	case g.status.Listed == nil:
		text = "Not listed yet (r to sync)."
	default:
		text = "No repositories match its include and exclude patterns."
	}
	return []string{"  " + style.Render(common.TruncateString(text, cw-2))}
}

// repoRow renders one of a forge's repositories: imported mark, path,
// ahead/behind of the default branch, and the last sync or what the tab
// is doing to it; two lines when narrow.
func (m *Forges) repoRow(g group, r forge.RepoStatus, selected bool, w int) []string {
	st := m.common.Styles
	sel := st.RepoSelector.Normal
	if selected {
		sel = st.RepoSelector.Active
	}
	base := sel.Base.UnsetHeight()
	cw := max(w-base.GetHorizontalFrameSize(), 10)
	rk := rowKey(g.status.Name, r.Path)

	mark := " "
	if r.Imported {
		mark = st.State.Done.Render("✓")
	}
	path := r.Path
	if r.Imported && !r.Listed && g.status.Listed != nil {
		path += " (no longer listed)"
	}
	var ab string
	switch {
	case !r.Imported:
		ab = sel.Desc.Render("not imported")
	case r.Ahead != nil && r.Behind != nil:
		ab = fmt.Sprintf("↑%d ↓%d", *r.Ahead, *r.Behind)
	default:
		ab = sel.Desc.Render("↑- ↓-")
	}
	var last string
	switch res, ok := m.results[rk]; {
	case m.busy[rk] != "":
		last = st.State.Working.Render(m.busy[rk])
	case ok && res.err:
		last = st.State.Error.Render(res.short)
	case ok:
		last = st.State.Done.Render(res.short)
	case r.LastError != "":
		last = st.State.Error.Render("✗ sync failed")
	case r.Imported && r.LastSync != nil:
		last = sel.Updated.Render("synced " + strings.TrimSuffix(ago(*r.LastSync, m.now()), " ago"))
	case r.Imported:
		last = sel.Updated.Render("never synced")
	}

	var lines []string
	if m.narrow() {
		lines = []string{
			"  " + mark + " " + sel.Title.Render(common.TruncateString(path, cw-4)),
			strings.TrimRight("    "+ab+"  "+last, " "),
		}
	} else {
		pathW := max(cw-4-2-aheadWidth-2-lastWidth, 10)
		lines = []string{
			"  " + mark + " " + sel.Title.Render(pad(common.TruncateString(path, pathW), pathW)) +
				"  " + pad(ab, aheadWidth) + "  " + last,
		}
	}
	return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
}

// pad pads s with spaces to n cells.
func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// forgeError is what went wrong last with a forge, if anything: its last
// forge-wide sync, else its last listing.
func (m *Forges) forgeError(s forge.ForgeStatus) (string, bool) {
	switch {
	case s.LastError != "":
		return "sync failed: " + tidyError(s.Name, s.LastError), true
	case s.ListError != "" && !m.listing[s.Name]:
		return "cannot list: " + tidyError(s.Name, s.ListError), true
	}
	return "", false
}

// requestPrefix is the `Get "https://…": ` an HTTP client error starts
// with.
var requestPrefix = regexp.MustCompile(`(?:Get|Post|Head) "[^"]*": `)

// tidyError shortens an error for a one-line display: the forge's name
// and the request URL go, what went wrong stays.
func tidyError(forgeName, s string) string {
	s = oneLine(s)
	s = strings.TrimPrefix(s, "forge "+forgeName+": ")
	return requestPrefix.ReplaceAllString(s, "")
}

// oneLine joins the lines of a message.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ago renders how long ago t was: "now" under a minute, then "3m ago",
// "5h ago", "2d ago".
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}
