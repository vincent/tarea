package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// Lookup resolves a variable name; ok=false means "unset".
type Lookup func(name string) (value string, ok bool)

var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand replaces every ${VAR} in s using lookup. An unset variable is an
// error, so a typo never silently turns into an empty secret.
func Expand(s string, lookup Lookup) (string, error) {
	var missing []string
	out := varPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return out, fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// EnvLookup returns a Lookup that prefers the process environment and falls
// back to the given values (typically parsed from a .env file).
func EnvLookup(fallback map[string]string) Lookup {
	return func(name string) (string, bool) {
		if v, ok := os.LookupEnv(name); ok {
			return v, true
		}
		v, ok := fallback[name]
		return v, ok
	}
}

// LoadDotEnv parses KEY=VALUE lines (with optional "export ", quotes and #
// comments). A missing file yields an empty map.
func LoadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		out[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
	}
	if err = sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}
