package forges

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/danielboothcloud/sundae/pkg/forge"
	"github.com/danielboothcloud/sundae/pkg/ui/common"
	"github.com/danielboothcloud/sundae/pkg/ui/components/prompt"
)

// Repository row columns (wide layout).
const (
	importWidth = 14 // "imported ↑3 ↓0"
	lastWidth   = 15 // "✗ import failed"
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
	lines = append(lines, m.listView(w, max(h-len(lines), 1))...)
	if len(lines) > h {
		lines = lines[:max(h, 0)]
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// Message implements common.Messenger: the notice, else the error of the
// selected row, so why a row failed shows without another key.
func (m *Forges) Message() (string, common.MessageKind) {
	if m.notice.text != "" {
		return m.notice.text, m.notice.kind
	}
	it, ok := m.selected()
	if !ok {
		return "", common.MessageInfo
	}
	g := m.groups[it.g]
	if it.kind == headerRow {
		// The whole of what went wrong with the forge.
		if text, ok := m.forgeError(g.status); ok {
			return withFix(g.status.Name + ": " + text), common.MessageError
		}
		return "", common.MessageInfo
	}
	r := g.repos[it.r]
	if res, ok := m.results[rowKey(g.status.Name, r.Path)]; ok && res.err {
		return withFix(res.detail), common.MessageError
	}
	if r.LastError != "" && m.busy[rowKey(g.status.Name, r.Path)] == "" {
		return withFix("Last sync failed: " + common.ShortErrorText(r.LastError)), common.MessageError
	}
	return "", common.MessageInfo
}

func (m *Forges) listView(w, h int) []string {
	st := m.common.Styles
	desc := st.RepoSelector.Normal.Desc
	switch {
	case m.statusErr != nil:
		return common.ErrorState(m.common, "Cannot read the forges", m.statusErr, "r retry", w)
	case !m.loaded:
		return []string{" " + desc.Render(common.Busy("Loading forges"))}
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
		doing = common.Busy(m.busy[hk])
		if p := m.progress[s.Name]; p != nil && p.total > 0 {
			doing += fmt.Sprintf(" %d of %d", min(p.done, p.total), p.total)
		}
		doing = sel.Updated.Render(doing)
	case s.Syncing:
		doing = sel.Updated.Render(common.Busy("syncing"))
	case m.listing[s.Name]:
		doing = sel.Updated.Render(common.Busy("listing"))
	}
	if s.Matching != nil {
		n := plural(*s.Matching, "repo", "repos")
		if listingOutdated(s) {
			n += " listed " + ago(*s.ListedAt, m.now())
		}
		counts = sel.Desc.Render(n + " · ")
	}
	counts += sel.Desc.Render(fmt.Sprintf("%d imported", s.Imported))
	if text, ok := m.forgeError(s); ok {
		last = st.Prompt.Error.Render("✗ " + text)
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

	// An import-all confirm asked here ends the first line, as in repoRow.
	var confirm *prompt.Confirm
	lw := cw
	if m.confirm != nil && m.confirmKey == hk {
		confirm = m.confirm
		lw -= confirm.AskWidth()
	}
	withConfirm := func(lines []string) []string {
		if confirm != nil {
			lines[0] = confirm.EndLine(lines[0], cw)
			for _, l := range confirm.Lines(cw-2, 0) {
				lines = append(lines, "  "+l)
			}
		}
		return append(lines, m.groupNote(g, cw)...)
	}

	title := sel.Title.Render(common.TruncateString(name, lw))
	if m.narrow() {
		right := ansi.Truncate(join(doing, counts, last), cw-2, "…")
		lines := withConfirm([]string{title, "  " + right})
		return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
	}
	right := join(doing, counts, last)
	room := lw - lipgloss.Width(title) - 2
	if lipgloss.Width(right) > room {
		// Drop the counts before cutting what went wrong.
		right = join(doing, last)
	}
	if lipgloss.Width(right) > room {
		right = ansi.Truncate(right, max(room, 0), "…")
	}
	gap := max(lw-lipgloss.Width(title)-lipgloss.Width(right), 1)
	lines := withConfirm([]string{title + strings.Repeat(" ", gap) + right})
	return strings.Split(base.Render(strings.Join(lines, "\n")), "\n")
}

// groupNote says why a forge shows no repositories. A failed listing has
// none: the header already says what went wrong.
func (m *Forges) groupNote(g group, cw int) []string {
	st := m.common.Styles
	if m.query != "" {
		return nil
	}
	var text string
	style := st.RepoSelector.Normal.Desc
	switch {
	case g.err != nil:
		text, style = "✗ Cannot read its repositories: "+common.ShortError(g.err), st.Prompt.Error
	case !g.loaded:
		text = common.Busy("Loading")
	case len(g.repos) > 0:
		return nil
	case m.listing[g.status.Name]:
		text = common.Busy("Listing repositories")
	case g.status.ListError != "":
		return nil
	case g.status.Listed == nil:
		text = "Not listed yet · u sync"
	default:
		text = "No repositories match its include and exclude patterns."
	}
	return []string{"  " + style.Render(common.TruncateString(text, cw-2))}
}

// repoRow renders one of a forge's repositories: path, imported or not
// imported (a word, as the status glyphs always have), ahead/behind of the
// default branch, and the last sync or what the tab is doing to it; two
// lines when narrow.
func (m *Forges) repoRow(g group, r forge.RepoStatus, selected bool, w int) []string {
	st := m.common.Styles
	sel := st.RepoSelector.Normal
	if selected {
		sel = st.RepoSelector.Active
	}
	base := sel.Base.UnsetHeight()
	cw := max(w-base.GetHorizontalFrameSize(), 10)
	rk := rowKey(g.status.Name, r.Path)

	path := r.Path
	if r.Imported && !r.Listed && g.status.Listed != nil {
		path += " (no longer listed)"
	}
	imported, ab := sel.Desc.Render("not imported"), ""
	// ↑ commits on Sundae the forge lacks, ↓ the reverse.
	switch {
	case !r.Imported:
	case r.Ahead != nil && r.Behind != nil:
		imported, ab = sel.Updated.Render("imported"), fmt.Sprintf("↑%d ↓%d", *r.Ahead, *r.Behind)
	default:
		imported, ab = sel.Updated.Render("imported"), sel.Desc.Render("↑- ↓-")
	}
	var last string
	switch res, ok := m.results[rk]; {
	case m.busy[rk] != "":
		last = sel.Updated.Render(m.busy[rk])
	case ok && res.err:
		last = st.Prompt.Error.Render(res.short)
	case ok:
		last = sel.Updated.Render(res.short)
	case r.LastError != "":
		last = st.Prompt.Error.Render("✗ sync failed")
	case r.Imported && r.LastSync != nil:
		last = sel.Updated.Render("synced " + strings.TrimSuffix(ago(*r.LastSync, m.now()), " ago"))
	case r.Imported:
		last = sel.Updated.Render("never synced")
	}

	// An open confirm ends the first line with its question; its lines
	// follow the row's, which say what a push or import would do.
	var confirm *prompt.Confirm
	lw := cw
	if m.confirm != nil && m.confirmKey == rk {
		confirm = m.confirm
		lw -= confirm.AskWidth()
	}
	var lines []string
	state := imported
	if ab != "" {
		state += " " + ab
	}
	if m.narrow() {
		lines = []string{
			"    " + sel.Title.Render(common.TruncateString(path, lw-4)),
			strings.TrimRight("    "+state+"  "+last, " "),
		}
	} else {
		pathW := max(lw-4-2-importWidth-2-lastWidth, 10)
		lines = []string{
			"    " + sel.Title.Render(pad(common.TruncateString(path, pathW), pathW)) +
				"  " + pad(state, importWidth) + "  " + last,
		}
	}
	if confirm != nil {
		lines[0] = confirm.EndLine(lines[0], cw)
		for _, l := range confirm.Lines(cw-4, 0) {
			lines = append(lines, "    "+l)
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

// tidyError shortens an error for a one-line display: the forge's name,
// the request and transport details go, what went wrong stays.
func tidyError(forgeName, s string) string {
	s = common.ShortErrorText(s)
	return strings.TrimPrefix(s, "forge "+forgeName+": ")
}

// oneLine joins the lines of a message.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ago renders how long ago t was: "now" under a minute, then "3m ago",
// "5h ago", "2d ago".
// listingOutdated reports a listing older than the last sync, e.g. one
// restored from disk after a restart, so its age is worth showing.
func listingOutdated(s forge.ForgeStatus) bool {
	if s.ListedAt == nil {
		return false
	}
	return s.LastSync == nil || s.LastSync.Sub(*s.ListedAt) > 5*time.Minute
}

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
