// Command nemo is a turn-based terminal coding assistant built on the
// nemo agent library: it reads prompts, streams the model's reasoning,
// output, and tool activity, and applies the workspace tools in the
// current directory. Model and endpoint come from a flat JSON config
// file (-config path, ./.nemo/config.json, or ~/.nemo/config.json),
// falling back to OpenRouter defaults with $OPENROUTER_API_KEY. Ctrl+C
// aborts the running turn; "exit" or Ctrl-D quits.
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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/deepteams/webp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"nemo"
)

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

// config is the parsed contents of a nemo JSON config file. Every field
// is optional; unset fields keep their defaults, and a base_url without
// an explicit provider drops the OpenRouter-specific routing hints.
type config struct {
	BaseURL     string         `json:"base_url"` // e.g. "https://api.openai.com/v1"
	Model       string         `json:"model"`
	APIKey      string         `json:"api_key"`
	Provider    map[string]any `json:"provider"`    // OpenRouter routing hints
	Temperature *float64       `json:"temperature"` // nil = provider default
	MaxTokens   int            `json:"max_tokens"`  // 0 = provider default
}

// Zero-config defaults: OpenRouter with the DeepSeek flash model.
const (
	defaultBaseURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel   = "deepseek/deepseek-v4.1-flash"
)

var defaultProvider = map[string]any{"order": []string{"DeepSeek"}, "allow_fallbacks": true}

// defaults returns the zero-config configuration.
func defaults() config {
	return config{BaseURL: defaultBaseURL, Model: defaultModel, Provider: defaultProvider}
}

// configSearchPaths lists the implicit config locations in precedence
// order: the workspace's .nemo/config.json, then the user-level one.
func configSearchPaths() []string {
	paths := []string{filepath.Join(".nemo", "config.json")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".nemo", "config.json"))
	}
	return paths
}

// fileExists reports whether path is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// loadConfig reads and parses a JSON config file; errors name the file so
// a bad config is loud and actionable.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("config %s: %w", path, err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// mergeConfig overlays the fields cfg sets onto base and returns the
// result. Provider routing hints are endpoint-specific: pointing base_url
// elsewhere without naming providers drops the default OpenRouter hints.
func mergeConfig(base, cfg config) config {
	if cfg.BaseURL != "" {
		base.BaseURL = cfg.BaseURL
		if cfg.Provider == nil {
			base.Provider = nil
		}
	}
	if cfg.Model != "" {
		base.Model = cfg.Model
	}
	if cfg.APIKey != "" {
		base.APIKey = cfg.APIKey
	}
	if cfg.Provider != nil {
		base.Provider = cfg.Provider
	}
	if cfg.Temperature != nil {
		base.Temperature = cfg.Temperature
	}
	if cfg.MaxTokens > 0 {
		base.MaxTokens = cfg.MaxTokens
	}
	return base
}

// resolveConfig finds and loads the configuration. An explicit -config
// path is used as given (a missing or malformed file is an error);
// otherwise the first existing search path wins. The returned source
// labels where the configuration came from ("defaults" or a path).
func resolveConfig(flagPath string) (cfg config, source string, err error) {
	paths := []string{flagPath}
	if flagPath == "" {
		paths = configSearchPaths()
	}
	for _, p := range paths {
		if flagPath == "" && !fileExists(p) {
			continue
		}
		loaded, err := loadConfig(p)
		if err != nil {
			return config{}, "", err
		}
		return mergeConfig(defaults(), loaded), p, nil
	}
	return defaults(), "defaults", nil
}

// normalizeChatURL turns an OpenAI-compatible base URL (e.g.
// "https://api.openai.com/v1") into a chat-completions endpoint by
// appending /chat/completions when missing; an already-complete endpoint
// passes through (trailing slashes tolerated), and non-http(s) URLs are
// rejected.
func normalizeChatURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("base_url %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url %q: only http/https are supported", base)
	}
	p := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(p, "/chat/completions") {
		p += "/chat/completions"
	}
	u.Path = p
	return u.String(), nil
}

// isLocalhostURL reports whether the endpoint points at the local host,
// where API keys are usually unnecessary (Ollama, LM Studio).
func isLocalhostURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// buildAgent assembles the root agent from a resolved config; all model
// and endpoint data enters the library through Agent fields.
func buildAgent(cfg config, apiKey, endpoint string) *nemo.Agent {
	return &nemo.Agent{
		Name:         "root",
		Endpoint:     endpoint,
		APIKey:       apiKey,
		Model:        cfg.Model,
		Tools:        nemo.DefaultTools,
		Provider:     cfg.Provider,
		Temperature:  cfg.Temperature,
		MaxTokens:    cfg.MaxTokens,
		SystemPrompt: nemo.ProjectPrompt(nemo.GroundedPrompt(nemo.SystemPrompt), "."),
		// Smart transport instead of nemo.DefaultImageURL: resize + WebP.
		ImageURL: webpImageURL,
	}
}

// ---------------------------------------------------------------------------
// Image transport: resize + WebP re-encode.
// ---------------------------------------------------------------------------

// maxImageDimension caps the longest side of an attached image; vision
// models charge by area/tile, so a ≤2000px WebP keeps attachments cheap
// while staying readable.
const maxImageDimension = 2000

// webpImageURL is the CLI's image transport (Agent.ImageURL): decode any
// image SniffImage admits (blank imports above register the decoders,
// webp and bmp included), downscale to maxImageDimension on the longest
// side, and re-encode as lossy WebP (quality 80, method 4) via
// deepteams/webp — a 4 MiB phone photo becomes a few hundred KB data:
// URL. Caveats: animated GIFs contribute their first frame only, animated
// WebP input fails at decode, and EXIF rotation is not applied. ctx is
// part of the transport signature but unused: decode/encode are
// CPU-bound and quick at these sizes.
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
	configPath := flag.String("config", "", "path to a JSON config file (default: ./.nemo/config.json, then ~/.nemo/config.json)")
	flag.Parse()

	cfg, source, err := resolveConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	endpoint, err := normalizeChatURL(cfg.BaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Key resolution: a config api_key wins over the OPENROUTER_API_KEY
	// environment variable; local endpoints run keyless.
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" && !isLocalhostURL(endpoint) {
		fmt.Fprintf(os.Stderr, "error: no API key: set \"api_key\" in %s or export OPENROUTER_API_KEY\n", source)
		os.Exit(1)
	}

	root := buildAgent(cfg, apiKey, endpoint)

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

	fmt.Println("nemo: turn-based coding assistant (Ctrl+C aborts a turn; exit or Ctrl-D to quit)")
	fmt.Printf("nemo: config=%s model=%s base=%s\n", source, root.Model, root.Endpoint)

	// Signals: Ctrl-C/SIGTERM during a run cancels just that turn; at the
	// prompt it discards the pending line (the tty does that anyway) and a
	// second consecutive press exits. Default signal disposition is never
	// relied on, so a stray SIGINT cannot kill the session.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

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

		// reportUsage prints the turn's token/cost delta plus session
		// totals; silent when the provider reports no usage.
		reportUsage := func() {
			if turn := s.Usage.Delta(uBefore); turn.PromptTokens > 0 || turn.CompletionTokens > 0 {
				fmt.Fprintf(os.Stderr, "[usage] turn: %s | session: %s\n", turn, s.Usage)
			}
		}

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
			reportUsage()
			continue
		}
		if interrupted {
			// Race: the turn completed before the cancel took effect.
			fmt.Fprintln(os.Stderr, "\n[turn completed before interrupt; kept]")
		}
		reportUsage()

		fmt.Println()
	}
}
