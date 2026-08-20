package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const startPort = 8101

func normalizeFlags(args []string, takesValue map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if idx := strings.Index(name, "="); idx != -1 {
			name = name[:idx]
		}
		if takesValue[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

type addResult struct {
	Name  string   `json:"name"`
	Dir   string   `json:"dir"`
	Port  int      `json:"port"`
	Files []string `json:"files"`
}

func cmdAdd(ctx context.Context, args []string) error {
	_ = ctx

	args = normalizeFlags(args, map[string]bool{"port": true})
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	portFlag := fs.Int("port", 0, "")
	jsonFlag := fs.Bool("json", false, "")
	forceFlag := fs.Bool("force", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	name := strings.TrimSpace(fs.Arg(0))
	if name == "" {
		return fmt.Errorf("missing app name; usage: hive add <name>")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid app name %q", name)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	dir := filepath.Join(cwd, name)

	if !*forceFlag {
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("directory %s already exists (use --force to overwrite)", dir)
		}
	}

	reserved, err := reservedPorts(cwd)
	if err != nil {
		return fmt.Errorf("scan existing apps: %w", err)
	}

	port := *portFlag
	if port == 0 {
		port, err = allocatePort(reserved)
		if err != nil {
			return fmt.Errorf("allocate port: %w", err)
		}
	} else {
		if reserved[port] {
			return fmt.Errorf("port %d is already used by an existing app", port)
		}
		if !portFree(port) {
			return fmt.Errorf("port %d is already in use", port)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	createdFiles := make([]string, 0, len(scaffoldFiles))
	for _, f := range scaffoldFiles {
		content, err := renderScaffold(f.name, name, port)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		createdFiles = append(createdFiles, f.name)
	}

	result := addResult{Name: name, Dir: dir, Port: port, Files: createdFiles}
	if *jsonFlag {
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(b))
	} else {
		fmt.Printf("Created app %s in %s (port %d)\n", name, dir, port)
	}
	return nil
}

func reservedPorts(root string) (map[int]bool, error) {
	used := map[int]bool{}
	check := func(dir string) {
		app, err := LoadApp(dir)
		if err == nil && app.Hive.Port != 0 {
			used[app.Hive.Port] = true
		}
	}
	check(root)

	if hasPnpmWorkspace(root) || hasPackageWorkspaces(filepath.Join(root, "package.json")) {
		apps, err := discoverWorkspaceApps(root, nil)
		if err != nil {
			return nil, fmt.Errorf("discover workspace apps: %w", err)
		}
		for _, app := range apps {
			if app.Hive.Port != 0 {
				used[app.Hive.Port] = true
			}
		}
		return used, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules" {
			continue
		}
		check(filepath.Join(root, e.Name()))
	}
	return used, nil
}

func allocatePort(reserved map[int]bool) (int, error) {
	for p := startPort; p <= 65535; p++ {
		if reserved[p] {
			continue
		}
		if !portFree(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free port found in range %d+", startPort)
}

func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

var scaffoldFiles = []struct {
	name string
	tmpl string
}{
	{"wrangler.jsonc", wranglerTemplate},
	{"index.ts", indexTemplate},
	{"tsconfig.json", tsconfigTemplate},
	{"package.json", packageTemplate},
}

func renderScaffold(file, name string, port int) ([]byte, error) {
	switch file {
	case "wrangler.jsonc":
		return []byte(fmt.Sprintf(wranglerTemplate, name)), nil
	case "package.json":
		return []byte(fmt.Sprintf(packageTemplate, name, port)), nil
	case "index.ts", "tsconfig.json":
		return []byte(scaffoldFilesMap[file]), nil
	default:
		return nil, fmt.Errorf("unknown scaffold file %q", file)
	}
}

var scaffoldFilesMap = map[string]string{
	"index.ts":     indexTemplate,
	"tsconfig.json": tsconfigTemplate,
}

const indexTemplate = `type Env = {
  APP: DurableObjectNamespace;
};

export class App implements DurableObject {
  constructor(
    private state: DurableObjectState,
    private env: Env,
  ) {}

  async fetch(request: Request): Promise<Response> {
    return Response.json({ ok: true, url: request.url });
  }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const id = env.APP.idFromName("default");
    return env.APP.get(id).fetch(request);
  },
} satisfies ExportedHandler<Env>;
`

const tsconfigTemplate = `{
  "compilerOptions": {
    "target": "es2022",
    "module": "es2022",
    "moduleResolution": "bundler",
    "lib": ["es2022"],
    "types": ["@cloudflare/workers-types"],
    "strict": true,
    "noEmit": true,
    "skipLibCheck": true
  },
  "include": ["index.ts"]
}
`

const wranglerTemplate = `{
  "name": %q,
  "main": "index.ts",
  "compatibility_date": "2026-01-01",
  "durable_objects": { "bindings": [{ "name": "APP", "class_name": "App" }] },
  "migrations": [{ "tag": "v1", "new_sqlite_classes": ["App"] }]
}
`

const packageTemplate = `{
  "name": %q,
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "scripts": {
    "type-check": "tsc -b"
  },
  "devDependencies": {
    "@cloudflare/workers-types": "^4.20250809.0",
    "typescript": "^5.9.2"
  },
  "hive": {
    "port": %d
  }
}
`
