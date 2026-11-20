package common

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
)

// ActionKind is what an action does, for its colour: the footer, the
// action menu and the command palette colour a key by its kind, so the main
// action and the ones that destroy something stand out the same way
// everywhere.
type ActionKind int

const (
	// ActionNormal is any other action.
	ActionNormal ActionKind = iota
	// ActionPrimary is the main action of what is shown: the one enter
	// runs (attach, open, create, run).
	ActionPrimary
	// ActionDanger deletes or ends something.
	ActionDanger
)

// dangerWords are the first words of the help of actions that delete or
// end something.
var dangerWords = map[string]bool{
	"delete": true, "remove": true, "prune": true,
	"destroy": true, "kill": true, "drop": true, "revoke": true,
}

// KindOf returns b's ActionKind: ActionDanger when its help starts with a
// destructive word, ActionPrimary when its first key is enter, else
// ActionNormal.
func KindOf(b key.Binding) ActionKind {
	desc := strings.ToLower(strings.TrimSpace(b.Help().Desc))
	if first, _, _ := strings.Cut(desc, " "); dangerWords[first] {
		return ActionDanger
	}
	if ks := b.Keys(); len(ks) > 0 && ks[0] == "enter" {
		return ActionPrimary
	}
	return ActionNormal
}

// KeyStyle returns the style of b's key in help lines and menus.
func (c *Common) KeyStyle(b key.Binding) lipgloss.Style {
	switch KindOf(b) {
	case ActionPrimary:
		return c.Styles.Action.PrimaryKey
	case ActionDanger:
		return c.Styles.Action.DangerKey
	}
	return c.Styles.HelpKey
}

// DescStyle returns the style of b's help text in help lines and menus.
func (c *Common) DescStyle(b key.Binding) lipgloss.Style {
	if KindOf(b) == ActionDanger {
		return c.Styles.Action.DangerDesc
	}
	return c.Styles.HelpValue
}
