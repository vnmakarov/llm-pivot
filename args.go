package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// SplitArgs splits a command line the way a shell would, honouring single
// quotes, double quotes and backslash escapes. No expansion is performed:
// the engine is executed directly, never through a shell.
func SplitArgs(s string) ([]string, error) {
	var (
		args  []string
		cur   []rune
		inTok bool
		quote rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur = append(cur, c)
			}
		case '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[i+1]):
				i++
				cur = append(cur, rs[i])
			default:
				cur = append(cur, c)
			}
		default:
			switch {
			case c == '\'' || c == '"':
				quote = c
				inTok = true
			case c == '\\':
				if i+1 >= len(rs) {
					return nil, errors.New("command ends with a dangling backslash")
				}
				i++
				cur = append(cur, rs[i])
				inTok = true
			case unicode.IsSpace(c):
				if inTok {
					args = append(args, string(cur))
					cur = cur[:0]
					inTok = false
				}
			default:
				cur = append(cur, c)
				inTok = true
			}
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inTok {
		args = append(args, string(cur))
	}
	return args, nil
}

// QuoteArgs renders an argument vector back into a copy-pasteable command
// line. Used by the monitor page to show what was actually executed.
func QuoteArgs(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteArg(a))
	}
	return b.String()
}

func quoteArg(a string) string {
	if a == "" {
		return "''"
	}
	safe := true
	for _, r := range a {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if !strings.ContainsRune("-_./:=+@,%", r) {
			safe = false
			break
		}
	}
	if safe {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// argGroup is a flag together with the operands that follow it. A group with
// an empty flag holds free-standing operands.
type argGroup struct {
	flag   string
	toks   []string
	closed bool // --flag=value takes no further operands
}

// isFlag reports whether tok introduces an option. A leading "-" followed by
// a digit or a dot is treated as a negative number, i.e. as an operand.
func isFlag(tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	c := tok[1]
	if (c >= '0' && c <= '9') || c == '.' {
		return false
	}
	return true
}

// flagName returns the option name of a token, stripping an inline "=value".
func flagName(tok string) (name string, inline bool) {
	if i := strings.IndexByte(tok, '='); i > 0 {
		return tok[:i], true
	}
	return tok, false
}

// groupArgs turns a flat argument vector into flag groups, so that a flag and
// the values belonging to it can be replaced as a unit.
func groupArgs(args []string) []argGroup {
	var gs []argGroup
	for _, a := range args {
		if isFlag(a) {
			name, inline := flagName(a)
			gs = append(gs, argGroup{flag: name, toks: []string{a}, closed: inline})
			continue
		}
		if len(gs) == 0 || gs[len(gs)-1].closed {
			gs = append(gs, argGroup{})
		}
		g := &gs[len(gs)-1]
		g.toks = append(g.toks, a)
	}
	return gs
}

// MergeArgs layers model options on top of engine options.
//
// A flag present in over replaces every occurrence of the same flag in base,
// keeping the position of the first occurrence; a flag that base does not have
// is appended. Free-standing operands of over are appended as well.
func MergeArgs(base, over []string) []string {
	baseGroups := groupArgs(base)
	overGroups := groupArgs(over)

	overByFlag := map[string][]argGroup{}
	for _, g := range overGroups {
		if g.flag != "" {
			overByFlag[g.flag] = append(overByFlag[g.flag], g)
		}
	}

	emitted := map[string]bool{}
	var merged []argGroup
	for _, g := range baseGroups {
		if g.flag != "" {
			if repl, ok := overByFlag[g.flag]; ok {
				if !emitted[g.flag] {
					emitted[g.flag] = true
					merged = append(merged, repl...)
				}
				continue // drop the engine's own occurrence
			}
		}
		merged = append(merged, g)
	}
	for _, g := range overGroups {
		if g.flag == "" {
			merged = append(merged, g) // operands always appended
			continue
		}
		if !emitted[g.flag] {
			emitted[g.flag] = true
			merged = append(merged, overByFlag[g.flag]...)
		}
	}

	var out []string
	for _, g := range merged {
		out = append(out, g.toks...)
	}
	return out
}

// HasFlag reports whether args already set the given option, in either the
// "--flag value" or the "--flag=value" form.
func HasFlag(args []string, flag string) bool {
	for _, a := range args {
		if !isFlag(a) {
			continue
		}
		if name, _ := flagName(a); name == flag {
			return true
		}
	}
	return false
}
