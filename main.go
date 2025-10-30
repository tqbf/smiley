package main

import (
	_ "embed"

	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/superfly/contextwindow"

	"smiley/agent"
)

//go:embed systemprompt.default.md
var defaultSystemPrompt string

var optLogTools = flag.Bool("log-tools", false, "Record tool invocations/responses in the transcript")

func init() {
	var (
		f   io.Writer = io.Discard
		err error
	)

	if os.Getenv("DEBUG") != "" {
		f, err = os.OpenFile("/tmp/app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("error opening log file: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)
}

func main() {
	var (
		systemMd      = flag.String("system", "", "Path to system.md")
		toolConfig    = flag.String("tools", "", "Path to tools.toml")
		contextDb     = flag.String("db", "", "Path to contextwindow.db")
		contextName   = flag.String("name", "", "Optional conversation name")
		maxTokens     = flag.Int("maxtokens", 60_000, "Maximum tokens before compacting")
		forkFrom      = flag.String("fork", "", "Conversation to fork")
		modelProvider = flag.String("model", "openai", "LLM provider: openai or claude")
		modelName     = flag.String("model-name", "", "Specific model name (e.g., claude-haiku-4-5, claude-sonnet-4-5, gpt-5-mini-2025-08-07)")
	)

	flag.Usage = func() {
		fmt.Println("smiley [options]")
		flag.PrintDefaults()
		os.Exit(0)
	}

	flag.Parse()

	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))

	cfgdir, err := agent.EnsureCtxAgentDir()
	if err != nil {
		eprintf("Find ~/.ctxagent: %v", err)
	}

	toolConfigPath := filepath.Join(cfgdir, "tools.toml")

	if *toolConfig != "" {
		toolConfigPath = *toolConfig
	}

	systemPrompt := defaultSystemPrompt
	systemPromptPath := filepath.Join(cfgdir, "system.md")
	if *systemMd != "" {
		systemPromptPath = *systemMd
	}
	pbuf, err := os.ReadFile(systemPromptPath)
	if err != nil {
		if *systemMd != "" {
			eprintf("Loading %s: %v", systemPromptPath, err)
		}
	} else {
		systemPrompt = string(pbuf)
	}

	path := filepath.Join(cfgdir, "contextwindow.db")
	if *contextDb != "" {
		path = *contextDb
	}
	db, err := contextwindow.NewContextDB(path)
	if err != nil {
		eprintf("Open %s: %v", path, err)
	}

	var model contextwindow.Model
	switch *modelProvider {
	case "openai":
		defaultModel := contextwindow.ResponsesModelGPT5Mini
		if *modelName != "" {
			model, err = contextwindow.NewOpenAIResponsesModel(*modelName)
		} else {
			model, err = contextwindow.NewOpenAIResponsesModel(defaultModel)
		}
		if err != nil {
			eprintf("Connect to OpenAI: %v", err)
		}
	case "claude":
		defaultModel := contextwindow.ModelClaudeHaiku45
		selectedModel := defaultModel
		if *modelName != "" {
			selectedModel = *modelName
		}
		model, err = contextwindow.NewClaudeModel(selectedModel)
		if err != nil {
			eprintf("Connect to Claude: %v", err)
		}
	default:
		eprintf("Unknown model provider: %s (use 'openai' or 'claude')", *modelProvider)
	}

	var ag *agent.Agent

	if *forkFrom != "" {
		ag, err = agent.NewAgentForked(db, model, *contextName, *forkFrom)
	} else {
		ag, err = agent.NewAgent(db, model, *contextName)
	}
	if err != nil {
		eprintf("Create agent: %v", err)
	}

	ag.RegisterBuiltinTool("todo", &agent.Todo{})
	ag.RegisterBuiltinTool("review", &agent.Review{})
	ag.RegisterBuiltinTool("lobotomize", &agent.Lobotomize{})
	ag.SetSystemPrompt(systemPrompt)
	ag.SetMaxTokens(*maxTokens)

	if err := ag.LoadTools(toolConfigPath); err != nil {
		// Only error if explicit tool config was provided
		if *toolConfig != "" {
			eprintf("Loading tool definitions from %s: %v", toolConfigPath, err)
		}
	}

	m := newRootWindow("", ag.GetContextWindow(), prompt, *contextName)
	m.db = db

	controllers := Controllers{}
	controllers = append(controllers, &TextAreaInput{})
	controllers = append(controllers, &SlashCommandController{
		cw: ag.GetContextWindow(),
	})

	tuiAgent := &TUIAgentController{
		agent: ag,
	}
	controllers = append(controllers, tuiAgent)
	m.controllers = controllers

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	m.p = p

	ag.OnEvent = func(msg agent.Message) {
		switch msg := msg.(type) {
		case agent.ToolCallMsg:
			p.Send(msgToolCall{
				name:     msg.Name,
				complete: msg.Complete,
				err:      msg.Err,
				size:     msg.Size,
				msg:      msg.Msg,
			})
		case agent.ModelResponseMsg:
			p.Send(msgModelResponse(msg.Response))
		case agent.TokenUsageMsg:
			p.Send(msgTokenUsage(msg.Usage))
		case agent.WorkingMsg:
			p.Send(msgWorking(msg.Working))
		case agent.ErrorMsg:
			p.Send(msgViewportLog{
				Msg:   msg.Msg,
				Style: styleErrorText,
			})
		}
	}

	if _, err := p.Run(); err != nil {
		eprintf("main program error: %v", err)
	}

}

func round(tot, pct int) int {
	if tot <= 0 || pct <= 0 {
		return 0
	}

	rows := float64(tot) * float64(pct) / 100.0
	return int(math.Round(rows))
}

func eprintf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
	if len(format) == 0 || format[len(format)-1] != '\n' {
		fmt.Fprintln(os.Stderr)
	}
	os.Exit(1)
}

func mustGetenv(v string) string {
	val := os.Getenv(v)
	if val == "" {
		eprintf("%s not set", v)
	}

	return val
}
