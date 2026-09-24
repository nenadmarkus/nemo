// Command nemo is a turn-based terminal coding assistant built on the
// nemo agent library: it reads prompts, streams the model's reasoning,
// output, and tool activity, and applies the workspace tools in the
// current directory. Model and endpoint come from a flat JSON config
// passed with -config: either a path to a JSON file or the JSON text
// itself; the flag is required, and there are no implicit defaults.
// Ctrl+C aborts the running turn; "exit" or Ctrl-D quits. With -task <string>
// nemo runs one shot instead: the string is a path to an instruction file
// if it exists on disk, otherwise the instruction text itself; a single
// turn runs and the process exits.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/deepteams/webp"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"nemo"
)

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

type config struct {
	BaseURL     string         `json:"base_url"` // e.g. "https://api.openai.com/v1"
	Model       string         `json:"model"`
	APIKey      string         `json:"api_key"`
	Provider    map[string]any `json:"provider"`    // routing hints
	Temperature *float64       `json:"temperature"` // nil = provider default
	MaxTokens   int            `json:"max_tokens"`  // 0 = provider default
}

// loadConfig parses the -config value: a path to a JSON config file if one
// exists on disk, otherwise the JSON text itself (mirroring -task). Inline
// parse errors never echo the value, which may hold the API key.
func loadConfig(value string) (config, error) {
	data, fromFile := []byte(value), false
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		b, err := os.ReadFile(value)
		if err != nil {
			return config{}, fmt.Errorf("config %s: %w", value, err)
		}
		data, fromFile = b, true
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		if fromFile {
			return config{}, fmt.Errorf("config %s: %w", value, err)
		}
		return config{}, fmt.Errorf("config: invalid JSON: %w", err)
	}
	return cfg, nil
}

// buildAgent assembles the root agent from a resolved config; all model
// and endpoint data enters the library through Agent fields.
func buildAgent(cfg config) *nemo.Agent {
	return &nemo.Agent{
		Name:         "root",
		Endpoint:     cfg.BaseURL,
		APIKey:       cfg.APIKey,
		Model:        cfg.Model,
		Tools:        nemo.DefaultTools(webpImageURL),
		Provider:     cfg.Provider,
		Temperature:  cfg.Temperature,
		MaxTokens:    cfg.MaxTokens,
		SystemPrompt: nemo.GroundedPrompt(nemo.SystemPrompt),
	}
}

// ---------------------------------------------------------------------------
// Image transport: resize + WebP re-encode.
// ---------------------------------------------------------------------------

func webpImageURL(ctx context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if nemo.SniffImage(data) == "" {
		return "", fmt.Errorf("%s: not a supported image (png, jpg, gif, webp, bmp)", path)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%s: decode image: %w", path, err)
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	const maxImageDimension = 2000
	if w > maxImageDimension || h > maxImageDimension {
		scale := float64(maxImageDimension) / float64(max(w, h))
		dst := image.NewRGBA(image.Rect(0, 0,
			int(float64(w)*scale), int(float64(h)*scale)))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
		src = dst
	}

	var out bytes.Buffer
	if err := webp.Encode(&out, src, &webp.EncoderOptions{
		Quality: 80,
		Method:  4,
	}); err != nil {
		return "", fmt.Errorf("%s: encode webp: %w", path, err)
	}

	return "data:image/webp;base64," +
		base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

func main() {
	configPath := flag.String("config", "", "path to a JSON config file, or the JSON config text itself")
	task := flag.String("task", "", "one-shot mode: run a single instruction and exit; "+
		"<string> is a path to an instruction file if it exists on disk, otherwise the instruction text itself")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	root := buildAgent(cfg)

	// Stream callbacks: render reasoning/output/tool/system blocks, each
	// starting on a fresh line with its own header so phases are unmistakable.
	const (
		blockNone      = 0
		blockReasoning = 1
		blockOutput    = 2
		blockTool      = 3
		blockSystem    = 4
	)
	currentBlock := blockNone
	printedAny := false
	lineOpen := false

	// stdout primitives; lineOpen tracks whether the last stdout write ended
	// with a newline.
	emitLine := func(s string) {
		fmt.Fprintln(os.Stdout, s)
		lineOpen = true
	}
	closeLine := func() {
		if !lineOpen {
			emitLine("")
		}
	}

	// beginStdout starts a reasoning/output/tool block on stdout.
	beginStdout := func(kind int, header string) {
		if currentBlock == kind {
			return
		}
		closeLine()
		if printedAny {
			emitLine("")
		}
		emitLine(header)
		printedAny = true
		currentBlock = kind
	}

	onReasoning := func(s string) {
		beginStdout(blockReasoning, "[reasoning]")
		fmt.Fprint(os.Stdout, s)
		lineOpen = false
	}

	onContent := func(s string) {
		beginStdout(blockOutput, "[output]")
		fmt.Fprint(os.Stdout, s)
		lineOpen = false
	}

	onSystem := func(s string) {
		if currentBlock != blockSystem {
			closeLine()
			if printedAny {
				fmt.Fprintln(os.Stderr, "")
			}
			printedAny = true
			currentBlock = blockSystem
		}
		oneLine := strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
		fmt.Fprintf(os.Stderr, "[system] %s\n", nemo.TruncateUTF8(oneLine, 1024, " ..."))
	}

	onTool := func(callID, name, args string, ok bool, detail string) {
		label := name
		if callID != "" {
			label = callID + " " + name
		}
		beginStdout(blockTool, "[tool: "+label+"]")
		argOneLine := strings.ReplaceAll(args, "\n", " ")
		emitLine(nemo.TruncateUTF8(argOneLine, 128, " ..."))
		if ok {
			emitLine("ok:")
			emitLine(nemo.TruncateUTF8(detail, 128, " ..."))
		} else {
			emitLine("error:")
			emitLine(nemo.TruncateUTF8(detail, 512, " ..."))
		}
		// Each tool call is its own block; the next phase starts fresh.
		currentBlock = blockNone
	}

	// onDiff renders a write/edit diff block on stdout.
	onDiff := func(s string) {
		closeLine()
		if printedAny {
			fmt.Fprintln(os.Stdout, "")
		}
		fmt.Fprint(os.Stdout, s)
		if !strings.HasSuffix(s, "\n") {
			fmt.Fprintln(os.Stdout, "")
		}
		printedAny = true
		// Each diff is its own block; the next phase starts fresh.
		currentBlock = blockNone
		lineOpen = true
	}

	s := nemo.NewSession()

	// Signals: Ctrl-C/SIGTERM during a run cancels just that turn; at the
	// prompt it discards the pending line (the tty does that anyway) and a
	// second consecutive press exits. Default signal disposition is never
	// relied on, so a stray SIGINT cannot kill the session. One-shot mode
	// reuses the channel to cancel its single turn.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// reportUsage prints the token/cost delta for a turn that started when
	// session usage stood at uBefore, plus session totals; silent when the
	// provider reports no usage.
	reportUsage := func(uBefore nemo.Usage) {
		if turn := s.Usage.Delta(uBefore); turn.PromptTokens > 0 || turn.CompletionTokens > 0 {
			fmt.Fprintf(os.Stderr, "[usage] turn: %s | session: %s\n", turn, s.Usage)
		}
	}

	// One-shot mode: -task <string> names a file on disk (instruction read
	// from it) or is the instruction text itself. A single turn runs, then
	// nemo exits: 0 on success, non-zero on error or interrupt.
	if *task != "" {
		instruction := *task
		if _, err := os.Stat(*task); err == nil {
			data, err := os.ReadFile(*task)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			instruction = strings.TrimSpace(string(data))
		}
		if instruction == "" {
			fmt.Fprintln(os.Stderr, "error: --task: instruction is empty")
			os.Exit(1)
		}

		baseCtx := nemo.WithDisplay(context.Background(), onDiff)
		turnCtx, cancel := context.WithTimeout(baseCtx, nemo.RunTimeout)
		before := len(s.Messages)
		uBefore := s.Usage

		runDone := make(chan error, 1)
		go func() {
			runDone <- root.Run(turnCtx, s, instruction,
				onReasoning, onContent, onSystem, onTool)
		}()

		var err error
		interrupted := false
		select {
		case err = <-runDone:
		case <-sigCh:
			cancel()
			interrupted = true
			err = <-runDone // wait for the run to unwind
		}
		cancel()

		if err != nil || interrupted {
			// Drop a partial exchange (e.g. tool_calls without their
			// results); the process exits anyway, but nothing half-
			// finished is left in the session.
			s.Messages = s.Messages[:before]
			if interrupted {
				fmt.Fprintln(os.Stderr, "\n[interrupted; turn dropped]")
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			os.Exit(1)
		}
		reportUsage(uBefore)
		fmt.Println()
		os.Exit(0)
	}

	fmt.Println("nemo: turn-based coding assistant (Ctrl+C aborts a turn; exit or Ctrl-D to quit)")
	fmt.Printf("nemo: model=%s base=%s\n", root.Model, root.Endpoint)

	// stdin is read from a goroutine so the prompt can select on input
	// and signals at once.
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	idleInterrupts := 0
	for {
		fmt.Print("\n> ")
		var input string
		select {
		case line, ok := <-lines:
			if !ok { // EOF (Ctrl-D)
				fmt.Println()
				return
			}
			input = strings.TrimSpace(line)
		case <-sigCh:
			idleInterrupts++
			if idleInterrupts >= 2 {
				fmt.Println("\nbye")
				return
			}
			fmt.Fprintln(os.Stderr, "\n(to quit: Ctrl+D or \"exit\"; during a run, Ctrl+C aborts the turn)")
			continue
		}
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			return
		}
		idleInterrupts = 0

		// The turn runs on its own context: RunTimeout bounds it, and the
		// select below can cancel it on a signal without touching the
		// session or the process. The display emitter rides in the ctx so
		// write/edit can print diffs without a special result channel.
		baseCtx := nemo.WithDisplay(context.Background(), onDiff)
		turnCtx, cancel := context.WithTimeout(baseCtx, nemo.RunTimeout)
		before := len(s.Messages)
		uBefore := s.Usage

		runDone := make(chan error, 1)
		go func() {
			runDone <- root.Run(turnCtx, s, input,
				onReasoning, onContent, onSystem, onTool)
		}()

		var err error
		interrupted := false
		select {
		case err = <-runDone:
		case <-sigCh:
			cancel()
			interrupted = true
			err = <-runDone // wait for the run to unwind
			// A second press while unwinding means "really quit".
			select {
			case <-sigCh:
				fmt.Println("\nbye")
				return
			default:
			}
		}
		cancel()

		if err != nil {
			// Drop a partial exchange (e.g. tool_calls without their
			// results) so the history stays valid for the next turn.
			s.Messages = s.Messages[:before]
			if interrupted {
				fmt.Fprintln(os.Stderr, "\n[interrupted; turn dropped]")
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			reportUsage(uBefore)
			continue
		}
		if interrupted {
			// Race: the turn completed before the cancel took effect.
			fmt.Fprintln(os.Stderr, "\n[turn completed before interrupt; kept]")
		}
		reportUsage(uBefore)

		fmt.Println()
	}
}
