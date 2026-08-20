package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed ui/static/index.html
var indexHTML []byte

func cmdUI(ctx context.Context, args []string) error {
	args = normalizeFlags(args, map[string]bool{"port": true})
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	portFlag := fs.Int("port", 8977, "")
	noBrowserFlag := fs.Bool("no-browser", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	app, err := loadCwdApp()
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *portFlag)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		res, err := gatherStatus(r.Context(), app)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		serveLogs(w, app)
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	url := "http://" + addr
	fmt.Printf("hive ui for %s at %s\n", app.Name, url)
	if !*noBrowserFlag {
		openBrowser(url)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func serveLogs(w http.ResponseWriter, app *App) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	logPath := filepath.Join(app.Dir, ".hive", "node.log")
	absLog, err := filepath.Abs(logPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	absDir, err := filepath.Abs(app.Dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rel, err := filepath.Rel(absDir, absLog)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.Error(w, "invalid log path", http.StatusForbidden)
		return
	}

	f, err := os.Open(absLog)
	if err != nil {
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	lines, err := tailLines(f, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func tailLines(r io.ReadSeeker, n int) ([]string, error) {
	if n <= 0 {
		n = 200
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}

	const blockSize = 4096
	buf := make([]byte, blockSize)
	var lines []string
	pos := size
	var carry strings.Builder

	for pos > 0 && len(lines) < n {
		readLen := blockSize
		if pos < int64(blockSize) {
			readLen = int(pos)
		}
		pos -= int64(readLen)
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, buf[:readLen]); err != nil {
			return nil, err
		}

		for i := readLen - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				seg := string(buf[i+1:readLen]) + carry.String()
				if seg != "" {
					lines = append(lines, seg)
					if len(lines) == n {
						break
					}
				}
				carry.Reset()
				readLen = i
			}
		}
		if len(lines) < n {
			carry.WriteString(string(buf[:readLen]))
		}
	}

	if carry.Len() > 0 && len(lines) < n {
		lines = append(lines, carry.String())
	}

	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}
