// incident-explainer watches a Kubernetes cluster for broken pods, opens an
// incident for each one, and explains it in plain English with an open-weight
// model running in Ollama. Logs are redacted first and never leave your
// infrastructure.
//
//	incident-explainer serve                # run the incident manager + web UI
//	incident-explainer explain <file|->     # explain one log file or stdin
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: incident-explainer serve | explain <file|->")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "explain":
		explainFile(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve() {
	interval, _ := time.ParseDuration(env("POLL_INTERVAL", "15s"))
	resolveAfter, _ := strconv.Atoi(env("RESOLVE_AFTER", "4"))
	ignore := map[string]bool{}
	for _, ns := range strings.Split(env("IGNORE_NAMESPACES", "kube-system,kube-public,kube-node-lease,local-path-storage,incident-explainer"), ",") {
		ignore[strings.TrimSpace(ns)] = true
	}

	kube, err := NewKube()
	if err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(env("DATA_FILE", "incidents.json"))
	if err != nil {
		log.Fatal(err)
	}
	m := &Manager{
		kube: kube, store: store,
		llm:          NewLLM(env("OLLAMA_HOST", "http://localhost:11434"), env("MODEL", "qwen3:8b")),
		interval:     interval,
		resolveAfter: max(resolveAfter, 1),
		ignoreNS:     ignore,
		webhook:      os.Getenv("WEBHOOK_URL"),
		uiURL:        os.Getenv("UI_URL"),
		keepIPs:      os.Getenv("KEEP_IPS") == "true",
		queue:        make(chan string, 256),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)

	addr := env("LISTEN_ADDR", ":8080")
	srv := &http.Server{Addr: addr, Handler: m.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("watching pods every %s, model %s at %s, UI on %s", interval, m.llm.Model, m.llm.URL, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func explainFile(args []string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	model := fs.String("model", env("MODEL", "qwen3:8b"), "Ollama model")
	dry := fs.Bool("dry-run", false, "print the redacted text that would be sent, then stop")
	keepIPs := fs.Bool("keep-ips", false, "do not mask IP addresses")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: incident-explainer explain [-dry-run] [-model m] <file|->")
		os.Exit(2)
	}
	path := fs.Arg(0)
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		log.Fatal(err)
	}
	clean, counts := Redact(string(data), *keepIPs)
	clean = Trim(clean, maxPromptChars)
	if *dry {
		fmt.Println(clean)
		fmt.Fprintf(os.Stderr, "\n-- %d chars would be sent to %s. Redacted: %s\n", len(clean), *model, CountsString(counts))
		return
	}
	llm := NewLLM(env("OLLAMA_HOST", "http://localhost:11434"), *model)
	ex, err := llm.Explain(context.Background(), path, clean)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nSUMMARY (confidence: %s)\n  %s\n\nLIKELY CAUSE\n  %s\n", ex.Confidence, ex.Summary, ex.LikelyCause)
	if len(ex.Evidence) > 0 {
		fmt.Println("\nEVIDENCE")
		for _, e := range ex.Evidence {
			fmt.Printf("  > %s\n", strings.TrimSpace(e))
		}
	}
	if len(ex.NextSteps) > 0 {
		fmt.Println("\nCHECK NEXT")
		for i, s := range ex.NextSteps {
			fmt.Printf("  %d. %s\n", i+1, s)
		}
	}
	fmt.Printf("\n-- %s on localhost, %.1fs, %d tokens in / %d out. Redacted: %s.\n",
		ex.Model, ex.Seconds, ex.PromptTokens, ex.OutputTokens, CountsString(counts))
}
