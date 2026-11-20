package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	defaultModel   = "accounts/fireworks/models/deepseek-v4p1-flash-1017"
	defaultTrigger = "#bobik"
	defaultSteps   = 25
	version        = "0.1.0"
)

const help = `bobik — a single-purpose AI agent for Markdown notes

Usage:
  bobik --prompt <path> --allow-path <glob> [options]
  bobik set-api-key <key>
  bobik --help | --version

Options:
  --prompt <path>          Markdown task instructions (required)
  --allow-path <glob>      Only files matching this glob are accessible (required)
  --model <id>             Fireworks model (default: ` + defaultModel + `)
  --vision-model <id>      Image inspection model (defaults to --model)
  --max-steps <n>          Step limit (default: 25)
  --watch                  Re-run on qualifying note changes
  --trigger <string>       Watch trigger (default: #bobik)
  --watch-delay <seconds>  Quiet period (default: 60; 0 disables it)

FIREWORKS_API_KEY overrides the key saved by set-api-key.
Runs are limited to five minutes. Bash searches require Bubblewrap.
`

type options struct {
	prompt, allow, model, visionModel, trigger string
	steps                                      int
	quietPeriod                                time.Duration
	watch                                      bool
}

func parseOptions(args []string) (options, error) {
	var o options
	var delay, steps string
	flags := flag.NewFlagSet("bobik", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.prompt, "prompt", "", "")
	flags.StringVar(&o.allow, "allow-path", "", "")
	flags.StringVar(&o.model, "model", defaultModel, "")
	flags.StringVar(&o.visionModel, "vision-model", "", "")
	flags.StringVar(&o.trigger, "trigger", defaultTrigger, "")
	flags.StringVar(&delay, "watch-delay", "60", "")
	flags.StringVar(&steps, "max-steps", strconv.Itoa(defaultSteps), "")
	flags.BoolVar(&o.watch, "watch", false, "")
	for _, arg := range args {
		if arg == "--deny" || strings.HasPrefix(arg, "--deny=") {
			return o, errors.New("--deny was removed: Bash is now isolated and read-only for notes; remove this flag")
		}
	}
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if flags.NArg() != 0 {
		return o, fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	for _, required := range []struct{ name, value string }{
		{"prompt", o.prompt}, {"allow-path", o.allow}, {"model", o.model}, {"trigger", o.trigger},
	} {
		if strings.TrimSpace(required.value) == "" || strings.HasPrefix(required.value, "--") {
			return o, fmt.Errorf("--%s requires a value", required.name)
		}
	}
	visionSet := false
	flags.Visit(func(f *flag.Flag) { visionSet = visionSet || f.Name == "vision-model" })
	if visionSet && (strings.TrimSpace(o.visionModel) == "" || strings.HasPrefix(o.visionModel, "--")) {
		return o, errors.New("--vision-model requires a value")
	}
	if !visionSet {
		o.visionModel = o.model
	}
	n, err := strconv.Atoi(steps)
	if err != nil || n < 1 {
		return o, errors.New("--max-steps requires a positive integer")
	}
	o.steps = n
	seconds, err := strconv.ParseFloat(delay, 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 || seconds > 2147483.647 {
		return o, errors.New("--watch-delay requires non-negative seconds (at most 2147483.647)")
	}
	o.quietPeriod = time.Duration(seconds * float64(time.Second))
	return o, nil
}
