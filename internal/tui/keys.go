package tui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Select  key.Binding
	Back    key.Binding
	Quit    key.Binding
	Confirm key.Binding
	Decline key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
		),
		Select: key.NewBinding(
			key.WithKeys("enter"),
		),
		Back: key.NewBinding(
			key.WithKeys("b", "esc"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
		),
		Confirm: key.NewBinding(
			key.WithKeys("y", "Y"),
		),
		Decline: key.NewBinding(
			key.WithKeys("n", "N", "esc", "b"),
		),
	}
}
