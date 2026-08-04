package onboard

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

type clearableHuhInput struct {
	*huh.Input
	value *string
	clear key.Binding
}

func newClearableHuhInput(input *huh.Input, value *string) *clearableHuhInput {
	return &clearableHuhInput{
		Input: input,
		value: value,
		clear: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "clear"),
		),
	}
}

func (i *clearableHuhInput) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && key.Matches(keyMsg, i.clear) {
		*i.value = ""
		i.Value(i.value)
	}

	model, cmd := i.Input.Update(msg)
	i.Input = model.(*huh.Input)
	return i, cmd
}

func (i *clearableHuhInput) KeyBinds() []key.Binding {
	return append([]key.Binding{i.clear}, i.Input.KeyBinds()...)
}
