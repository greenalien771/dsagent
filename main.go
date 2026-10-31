package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

//go:embed ui/*
var uiFS embed.FS

// preferredPort keeps the UI on a stable origin so the chat history stored in
// localStorage survives a restart. It is loopback-only either way.
const preferredPort = "8787"

func main() {
	cfg := loadConfig()
	ag := NewAgent(cfg)

	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/chat", postOnly(ag.handleChat))
	mux.HandleFunc("/api/stop", postOnly(ag.handleStop))
	mux.HandleFunc("/api/abort", postOnly(ag.handleAbort))
	mux.HandleFunc("/api/status", ag.handleStatus)
	mux.HandleFunc("/api/approve", postOnly(ag.handleApprove))
	mux.HandleFunc("/api/clear", postOnly(ag.handleClear))
	mux.HandleFunc("/api/config", handleConfig(cfg))
	mux.HandleFunc("/api/key", handleKey(cfg))

	// Bind to loopback only and remember the exact authorities we can legitimately
	// be reached on. The guard rejects anything else, which is what keeps a
	// hostile web page (DNS rebinding) from talking to this server on the user's
	// behalf.
	//
	// The port is a fixed one by preference: the browser partitions localStorage
	// by scheme+host+port, so chat history saved under one port is invisible - and
	// left behind - if the next launch picks a different one. When the port is
	// taken, a free one is used and only that run's history is available.
	ln, err := net.Listen("tcp", "127.0.0.1:"+preferredPort)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		panic(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		addAllowedHost(h + ":" + port)
	}

	url := "http://" + ln.Addr().String()
	go http.Serve(ln, guard(mux))

	// The banner ends with the bound URL, which is what scripts and headless runs
	// scrape to learn the port. It is printed from the same config the agent was
	// built with, so what it says about the workspace, the key and the approval
	// gate is what this process will actually do.
	printBanner(os.Stdout, url, cfg)

	// -no-browser is for automated checks; a normal launch opens the UI.
	if !hasArg("-no-browser") {
		open(url)
	}
	select {}
}

func hasArg(want string) bool {
	for _, a := range os.Args[1:] {
		if a == want {
			return true
		}
	}
	return false
}

// handleConfig serves and patches the non-secret configuration. The API key is
// intentionally not part of this payload in either direction.
func handleConfig(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			writeJSON(w, http.StatusOK, viewOf(cfg))
		case http.MethodPost:
			var p configPatch
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&p); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := applyPatch(cfg, p); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, viewOf(cfg))
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleKey is the only endpoint that accepts key material, and it never echoes
// it back: the response is the same masked view the config endpoint returns.
func handleKey(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var p struct {
				APIKey string `json:"api_key"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&p); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			if err := applyPatch(cfg, configPatch{APIKey: &p.APIKey}); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, viewOf(cfg))
		case http.MethodDelete:
			if err := applyPatch(cfg, configPatch{ClearAPIKey: boolPtr(true)}); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, viewOf(cfg))
		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func open(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
