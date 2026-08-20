package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// HiveConfig is the "hive" block in the app's package.json. It holds
// everything celld's wrangler.jsonc parser would reject.
type HiveConfig struct {
	Port   int    `json:"port"`
	Domain string `json:"domain,omitempty"`
	Server string `json:"server,omitempty"` // ssh host for the run backend
}

// App is one deployable celld project: a wrangler.jsonc plus a "hive" block
// in package.json, in the same directory.
type App struct {
	Dir  string
	Name string // wrangler.jsonc "name"
	Hive HiveConfig
}

func LoadApp(dir string) (*App, error) {
	pkgPath := filepath.Join(dir, "package.json")
	pkgBytes, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgPath, err)
	}
	var pkg struct {
		Hive HiveConfig `json:"hive"`
	}
	if err := json.Unmarshal(pkgBytes, &pkg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", pkgPath, err)
	}

	wranglerPath := filepath.Join(dir, "wrangler.jsonc")
	wranglerBytes, err := os.ReadFile(wranglerPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wranglerPath, err)
	}
	var wrangler struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(stripJSONC(wranglerBytes), &wrangler); err != nil {
		return nil, fmt.Errorf("parse %s: %w", wranglerPath, err)
	}
	if wrangler.Name == "" {
		return nil, fmt.Errorf("%s: missing \"name\"", wranglerPath)
	}
	if pkg.Hive.Port == 0 {
		return nil, fmt.Errorf("%s: missing \"hive\".\"port\"", pkgPath)
	}

	return &App{Dir: dir, Name: wrangler.Name, Hive: pkg.Hive}, nil
}

// stripJSONC removes // and /* */ comments so encoding/json can read
// wrangler.jsonc. Does not handle comment markers inside strings; good
// enough for config files we control.
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	i := 0
	for i < len(b) {
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '/' {
			for i < len(b) && b[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '*' {
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}
