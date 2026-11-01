package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Images without /bin/sh (distroless, scratch) can't run the launcher, so
// ramekind does its work itself: it reads the image's environment files and
// runscript and execs the command directly. It understands what Apptainer
// writes for simple images (export lines, a one-command %runscript, an OCI
// image's ENTRYPOINT and CMD) and refuses anything that needs a real shell.

// errNeedsShell reports image content that only a shell can run.
var errNeedsShell = errors.New("needs a shell")

// apptainerBoilerplate are environment files Apptainer adds to every image;
// they only matter for its own features (SCIF apps, PS1, PATH overrides from
// its command line), except 99-base.sh's library path, applied below.
var apptainerBoilerplate = map[string]bool{
	"01-base.sh": true, "94-appsbase.sh": true, "95-apps.sh": true,
	"99-base.sh": true, "99-runtimevars.sh": true,
}

const libsDir = "/.singularity.d/libs"

// ShellFreeLaunch returns the command and environment for the image rooted
// at root, as the launcher would run them, without a shell.
func ShellFreeLaunch(root string, env []string) (argv, outEnv []string, err error) {
	vars := newEnvMap(env)
	dir := filepath.Join(root, ".singularity.d", "env")
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sh") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if apptainerBoilerplate[n] {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, nil, err
		}
		if err := applyEnvScript(string(b), vars); err != nil {
			return nil, nil, fmt.Errorf("environment %s: %w", n, err)
		}
	}
	if v, ok := vars.get("LD_LIBRARY_PATH"); ok && v != "" {
		vars.set("LD_LIBRARY_PATH", v+":"+libsDir)
	} else {
		vars.set("LD_LIBRARY_PATH", libsDir)
	}

	b, err := os.ReadFile(filepath.Join(root, ".singularity.d", "runscript"))
	if err != nil {
		return nil, nil, fmt.Errorf("the image has no runscript: %w", err)
	}
	if argv, err = parseRunscript(string(b), vars); err != nil {
		return nil, nil, fmt.Errorf("runscript: %w", err)
	}
	if len(argv) == 0 {
		return nil, nil, errors.New("the image has no command to run (no %runscript, ENTRYPOINT or CMD)")
	}
	return argv, vars.list(), nil
}

// applyEnvScript runs the subset of sh that environment files use:
// comments, NAME=value, export NAME=value, export NAME and unset NAME.
func applyEnvScript(script string, vars *envMap) error {
	for _, line := range logicalLines(script) {
		words, err := splitWords(line, vars, true)
		if err != nil {
			return fmt.Errorf("%q: %w", line, err)
		}
		if len(words) == 0 {
			continue
		}
		switch words[0].raw {
		case "export":
			words = words[1:]
		case "unset":
			for _, w := range words[1:] {
				vars.unset(w.val)
			}
			continue
		}
		for _, w := range words {
			if w.assign == "" {
				if !validName(w.val) {
					return fmt.Errorf("%q: %w", line, errNeedsShell)
				}
				continue // export NAME: everything here is exported already
			}
			vars.set(w.assign, w.val)
		}
	}
	return nil
}

// parseRunscript returns the command a runscript runs with no arguments.
func parseRunscript(script string, vars *envMap) ([]string, error) {
	if strings.Contains(script, `eval "set ${SINGULARITY_OCI_RUN}"`) {
		return parseOCIRunscript(script, vars)
	}
	var cmd []string
	for _, line := range logicalLines(script) {
		if cmd != nil {
			return nil, fmt.Errorf("it runs more than one command: %w", errNeedsShell)
		}
		words, err := splitWords(line, vars, false)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", line, err)
		}
		if len(words) > 0 && words[0].raw == "exec" {
			words = words[1:]
		}
		cmd = []string{}
		for _, w := range words {
			if w.assign != "" && len(cmd) == 0 {
				return nil, fmt.Errorf("%q: variable assignments before a command: %w", line, errNeedsShell)
			}
			switch {
			case w.dropped:
			case w.assign != "":
				cmd = append(cmd, w.assign+"="+w.val) // an argument, such as FOO=bar
			default:
				cmd = append(cmd, w.val)
			}
		}
	}
	return cmd, nil
}

// parseOCIRunscript reads the ENTRYPOINT and CMD from the runscript
// Apptainer generates for an OCI image; with no arguments it runs
// ENTRYPOINT followed by CMD.
func parseOCIRunscript(script string, vars *envMap) ([]string, error) {
	var argv []string
	for _, key := range []string{"OCI_ENTRYPOINT", "OCI_CMD"} {
		found := false
		for _, line := range strings.Split(script, "\n") {
			v, ok := strings.CutPrefix(line, key+"=")
			if !ok {
				continue
			}
			found = true
			outer, err := splitWords(v, vars, false)
			if err != nil || len(outer) > 1 {
				return nil, fmt.Errorf("%s: unexpected form %q", key, v)
			}
			if len(outer) == 0 {
				break
			}
			words, err := splitWords(outer[0].val, vars, false)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", key, outer[0].val, err)
			}
			for _, w := range words {
				argv = append(argv, w.val)
			}
			break
		}
		if !found {
			return nil, fmt.Errorf("no %s in the OCI runscript", key)
		}
	}
	return argv, nil
}

// logicalLines returns the script's lines without the shebang, comments and
// blank lines, joining lines continued with a backslash.
func logicalLines(script string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(script, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.HasSuffix(l, "\\") {
			cur += strings.TrimSuffix(l, "\\") + " "
			continue
		}
		l = strings.TrimSpace(cur + l)
		cur = ""
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	if s := strings.TrimSpace(cur); s != "" {
		out = append(out, s)
	}
	return out
}

type word struct {
	raw    string // the word as written
	val    string // after quote removal and expansion
	assign string // NAME, for a NAME=value word
	// dropped is set for "$@" and $@, which expand to nothing.
	dropped bool
}

// splitWords splits a line into words the way sh would, expanding $NAME,
// ${NAME}, ${NAME:-word} and ${NAME-word}. "$@" expands to nothing: ramekind
// passes no arguments. Anything needing more of a shell (pipes, lists, redirects,
// substitutions, globs) is an error. With assignOnly, an unquoted word
// that isn't NAME=value or a plain name is an error too.
func splitWords(line string, vars *envMap, assignOnly bool) ([]word, error) {
	var words []word
	i := 0
	for {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			return words, nil
		}
		if line[i] == '#' {
			return words, nil
		}
		start := i
		var val strings.Builder
		w := word{}
		atStart := true
		quoted := false
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			c := line[i]
			switch {
			case c == '\'':
				end := strings.IndexByte(line[i+1:], '\'')
				if end < 0 {
					return nil, errors.New("unterminated quote")
				}
				val.WriteString(line[i+1 : i+1+end])
				i += end + 2
				quoted = true
			case c == '"':
				i++
				for {
					if i >= len(line) {
						return nil, errors.New("unterminated quote")
					}
					c := line[i]
					if c == '"' {
						i++
						break
					}
					if c == '\\' && i+1 < len(line) && strings.IndexByte("$`\"\\", line[i+1]) >= 0 {
						val.WriteByte(line[i+1])
						i += 2
						continue
					}
					if c == '`' {
						return nil, fmt.Errorf("command substitution: %w", errNeedsShell)
					}
					if c == '$' {
						if strings.HasPrefix(line[i:], `$@"`) && atStart && val.Len() == 0 && i == start+1 {
							w.dropped = true
							i += 2
							continue
						}
						s, n, err := expand(line[i:], vars)
						if err != nil {
							return nil, err
						}
						val.WriteString(s)
						i += n
						continue
					}
					val.WriteByte(c)
					i++
				}
				quoted = true
			case c == '\\':
				if i+1 >= len(line) {
					return nil, errors.New("trailing backslash")
				}
				val.WriteByte(line[i+1])
				i += 2
				quoted = true
			case c == '$':
				if strings.HasPrefix(line[i:], "$@") {
					w.dropped = true
					i += 2
					continue
				}
				s, n, err := expand(line[i:], vars)
				if err != nil {
					return nil, err
				}
				val.WriteString(s)
				i += n
			case c == '=' && !quoted && w.assign == "" && validName(val.String()):
				w.assign = val.String()
				val.Reset()
				i++
			case strings.IndexByte("|&;<>()`*?[]{}~!", c) >= 0:
				return nil, fmt.Errorf("%q: %w", string(c), errNeedsShell)
			default:
				val.WriteByte(c)
				i++
			}
			atStart = false
		}
		w.raw = line[start:i]
		w.val = val.String()
		if w.dropped && w.val != "" {
			return nil, fmt.Errorf("$@ inside a word: %w", errNeedsShell)
		}
		if assignOnly && w.assign == "" && !quoted && !validName(w.val) && len(words) > 0 {
			return nil, errNeedsShell
		}
		words = append(words, w)
	}
}

// expand expands the parameter at the start of s ($NAME, ${NAME},
// ${NAME:-word}, ${NAME-word}) and returns its value and length.
func expand(s string, vars *envMap) (string, int, error) {
	if len(s) < 2 {
		return "$", 1, nil
	}
	if s[1] == '(' {
		return "", 0, fmt.Errorf("command substitution: %w", errNeedsShell)
	}
	if s[1] != '{' {
		n := 1
		for n < len(s) && (isAlnum(s[n]) || s[n] == '_') {
			n++
		}
		if n == 1 {
			return "", 0, fmt.Errorf("$%c: %w", s[1], errNeedsShell)
		}
		v, _ := vars.get(s[1:n])
		return v, n, nil
	}
	// Find the matching }, skipping quoted text.
	depth, j := 0, 2
	for ; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			end := strings.IndexByte(s[j+1:], s[j])
			if end < 0 {
				return "", 0, errors.New("unterminated quote")
			}
			j += end + 1
		case '{':
			depth++
		case '}':
			if depth == 0 {
				goto found
			}
			depth--
		}
	}
	return "", 0, errors.New("unterminated ${")
found:
	inner := s[2:j]
	name, rest := inner, ""
	if k := strings.IndexAny(inner, ":-+=?#%/"); k >= 0 {
		name, rest = inner[:k], inner[k:]
	}
	if !validName(name) {
		return "", 0, fmt.Errorf("${%s}: %w", inner, errNeedsShell)
	}
	v, set := vars.get(name)
	switch {
	case rest == "":
		return v, j + 1, nil
	case strings.HasPrefix(rest, ":-"), strings.HasPrefix(rest, "-"):
		colon := strings.HasPrefix(rest, ":")
		if set && (!colon || v != "") {
			return v, j + 1, nil
		}
		def := strings.TrimPrefix(strings.TrimPrefix(rest, ":"), "-")
		ws, err := splitWords(def, vars, false)
		if err != nil {
			return "", 0, err
		}
		parts := make([]string, len(ws))
		for i, w := range ws {
			parts[i] = w.val
		}
		return strings.Join(parts, " "), j + 1, nil
	}
	return "", 0, fmt.Errorf("${%s}: %w", inner, errNeedsShell)
}

func validName(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && s[i] != '_' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// envMap is an ordered environment.
type envMap struct {
	keys []string
	m    map[string]string
}

func newEnvMap(env []string) *envMap {
	e := &envMap{m: map[string]string{}}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		e.set(k, v)
	}
	return e
}

func (e *envMap) get(k string) (string, bool) { v, ok := e.m[k]; return v, ok }

func (e *envMap) set(k, v string) {
	if _, ok := e.m[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.m[k] = v
}

func (e *envMap) unset(k string) {
	if _, ok := e.m[k]; !ok {
		return
	}
	delete(e.m, k)
	for i, x := range e.keys {
		if x == k {
			e.keys = append(e.keys[:i], e.keys[i+1:]...)
			break
		}
	}
}

func (e *envMap) list() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.m[k])
	}
	return out
}
