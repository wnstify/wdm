// Package tui contains the Bubble Tea terminal interface for wdm.
// Screens in this package talk only to pkg/engine and pkg/types. Product
// behavior stays behind the engine facade; the TUI owns presentation, input,
// progress messages, and confirmation prompts.
package tui
