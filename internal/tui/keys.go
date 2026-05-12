package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap centralizes every binding the TUI honors. Adding a new key is a
// matter of registering it here and handling it in app.go's Update.
type KeyMap struct {
	Quit        key.Binding
	Help        key.Binding
	Up          key.Binding
	Down        key.Binding
	PageUp      key.Binding
	PageDown    key.Binding
	Home        key.Binding
	End         key.Binding
	PrevKind    key.Binding
	NextKind    key.Binding
	Filter      key.Binding
	CancelInput key.Binding
	Describe    key.Binding
	Events      key.Binding
	Timeline    key.Binding
	Logs        key.Binding
	YAML        key.Binding
	Back        key.Binding
	PrevSnap    key.Binding
	NextSnap    key.Binding
	JumpStart   key.Binding
	JumpEnd     key.Binding
	Live        key.Binding
	NamespaceSw key.Binding
}

// DefaultKeyMap returns the standard bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:      key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("PgUp", "page up")),
		PageDown:    key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("PgDn", "page down")),
		Home:        key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),
		PrevKind:    key.NewBinding(key.WithKeys("shift+tab", "["), key.WithHelp("[ / S-tab", "prev kind")),
		NextKind:    key.NewBinding(key.WithKeys("tab", "]"), key.WithHelp("] / tab", "next kind")),
		Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		CancelInput: key.NewBinding(key.WithKeys("esc")),
		Describe:    key.NewBinding(key.WithKeys("enter", "d"), key.WithHelp("enter/d", "describe")),
		Events:      key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "events")),
		Timeline:    key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "changes timeline")),
		Logs:        key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "pod logs")),
		YAML:        key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "yaml of resource")),
		Back:        key.NewBinding(key.WithKeys("esc", "backspace"), key.WithHelp("esc", "back")),
		PrevSnap:    key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "prev snap")),
		NextSnap:    key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "next snap")),
		JumpStart:   key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("C-a", "first snap")),
		JumpEnd:     key.NewBinding(key.WithKeys("ctrl+e"), key.WithHelp("C-e", "last snap")),
		Live:        key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "jump to live")),
		NamespaceSw: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "namespace")),
	}
}
