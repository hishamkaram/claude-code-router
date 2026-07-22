package executor

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	cdpModifierAlt = 1 << iota
	cdpModifierCtrl
	cdpModifierMeta
	cdpModifierShift
)

type cdpKeyStroke struct {
	Key                   string
	Code                  string
	Text                  string
	WindowsVirtualKeyCode int
	Modifiers             int
}

func parseCDPKeyStroke(value string) (cdpKeyStroke, error) {
	parts := strings.Split(strings.TrimSpace(value), "+")
	var stroke cdpKeyStroke
	var keyPart string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if modifier, ok := cdpModifier(part); ok {
			stroke.Modifiers |= modifier
			continue
		}
		if keyPart != "" {
			return cdpKeyStroke{}, fmt.Errorf("keypress action requires exactly one non-modifier key")
		}
		keyPart = part
	}
	if keyPart == "" {
		return cdpKeyStroke{}, fmt.Errorf("keypress action requires a non-modifier key")
	}
	key, err := cdpKeyDefinition(keyPart, stroke.Modifiers)
	if err != nil {
		return cdpKeyStroke{}, err
	}
	key.Modifiers = stroke.Modifiers
	return key, nil
}

func (stroke cdpKeyStroke) params(eventType string) cdpKeyEventParams {
	params := cdpKeyEventParams{
		Type:                  eventType,
		Key:                   stroke.Key,
		Code:                  stroke.Code,
		WindowsVirtualKeyCode: stroke.WindowsVirtualKeyCode,
		NativeVirtualKeyCode:  stroke.WindowsVirtualKeyCode,
		Modifiers:             stroke.Modifiers,
	}
	if eventType == "keyDown" && stroke.Text != "" {
		params.Text = stroke.Text
		params.UnmodifiedText = stroke.Text
	}
	return params
}

type cdpKeyEventParams struct {
	Type                  string `json:"type"`
	Key                   string `json:"key,omitempty"`
	Code                  string `json:"code,omitempty"`
	Text                  string `json:"text,omitempty"`
	UnmodifiedText        string `json:"unmodifiedText,omitempty"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode,omitempty"`
	NativeVirtualKeyCode  int    `json:"nativeVirtualKeyCode,omitempty"`
	Modifiers             int    `json:"modifiers,omitempty"`
}

func cdpModifier(value string) (int, bool) {
	switch normalizeKeyName(value) {
	case "alt", "option":
		return cdpModifierAlt, true
	case "ctrl", "control":
		return cdpModifierCtrl, true
	case "cmd", "command", "meta", "super":
		return cdpModifierMeta, true
	case "shift":
		return cdpModifierShift, true
	default:
		return 0, false
	}
}

func cdpMouseModifiers(keys []string) (int, error) {
	modifiers := 0
	for _, key := range keys {
		modifier, ok := cdpModifier(key)
		if !ok {
			return 0, fmt.Errorf("mouse action modifier %q is not supported", key)
		}
		modifiers |= modifier
	}
	return modifiers, nil
}

func cdpKeyDefinition(value string, modifiers int) (cdpKeyStroke, error) {
	if key, ok := namedCDPKey(value); ok {
		return key, nil
	}
	runeValue, size := utf8.DecodeRuneInString(value)
	if (runeValue == utf8.RuneError && size == 0) || size != len(value) {
		return cdpKeyStroke{}, fmt.Errorf("keypress key %q is not supported", value)
	}
	key := cdpKeyStroke{Key: string(runeValue)}
	if unicode.IsLetter(runeValue) {
		upper := unicode.ToUpper(runeValue)
		key.Code = "Key" + string(upper)
		key.WindowsVirtualKeyCode = int(upper)
	} else if unicode.IsDigit(runeValue) {
		key.Code = "Digit" + string(runeValue)
		key.WindowsVirtualKeyCode = int(runeValue)
	}
	if modifiers&(cdpModifierCtrl|cdpModifierAlt|cdpModifierMeta) == 0 {
		key.Text = key.Key
	}
	return key, nil
}

func namedCDPKey(value string) (cdpKeyStroke, bool) {
	switch normalizeKeyName(value) {
	case "enter", "return":
		return cdpKeyStroke{Key: "Enter", Code: "Enter", WindowsVirtualKeyCode: 13}, true
	case "tab":
		return cdpKeyStroke{Key: "Tab", Code: "Tab", WindowsVirtualKeyCode: 9}, true
	case "escape", "esc":
		return cdpKeyStroke{Key: "Escape", Code: "Escape", WindowsVirtualKeyCode: 27}, true
	case "backspace":
		return cdpKeyStroke{Key: "Backspace", Code: "Backspace", WindowsVirtualKeyCode: 8}, true
	case "delete", "del":
		return cdpKeyStroke{Key: "Delete", Code: "Delete", WindowsVirtualKeyCode: 46}, true
	case "arrowleft", "left":
		return cdpKeyStroke{Key: "ArrowLeft", Code: "ArrowLeft", WindowsVirtualKeyCode: 37}, true
	case "arrowup", "up":
		return cdpKeyStroke{Key: "ArrowUp", Code: "ArrowUp", WindowsVirtualKeyCode: 38}, true
	case "arrowright", "right":
		return cdpKeyStroke{Key: "ArrowRight", Code: "ArrowRight", WindowsVirtualKeyCode: 39}, true
	case "arrowdown", "down":
		return cdpKeyStroke{Key: "ArrowDown", Code: "ArrowDown", WindowsVirtualKeyCode: 40}, true
	case "home":
		return cdpKeyStroke{Key: "Home", Code: "Home", WindowsVirtualKeyCode: 36}, true
	case "end":
		return cdpKeyStroke{Key: "End", Code: "End", WindowsVirtualKeyCode: 35}, true
	case "pageup":
		return cdpKeyStroke{Key: "PageUp", Code: "PageUp", WindowsVirtualKeyCode: 33}, true
	case "pagedown":
		return cdpKeyStroke{Key: "PageDown", Code: "PageDown", WindowsVirtualKeyCode: 34}, true
	case "space":
		return cdpKeyStroke{Key: " ", Code: "Space", Text: " ", WindowsVirtualKeyCode: 32}, true
	default:
		return cdpKeyStroke{}, false
	}
}

func normalizeKeyName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}
