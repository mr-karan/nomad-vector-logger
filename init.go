package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/mr-karan/nomad-events-sink/pkg/stream"
	flag "github.com/spf13/pflag"
	"github.com/zerodha/logf"
)

// initLogger initializes logger.
// initLogger initializes logger instance.
func initLogger(ko *koanf.Koanf) logf.Logger {
	opts := logf.Opts{EnableCaller: true}
	if ko.String("app.log_level") == "debug" {
		opts.Level = logf.DebugLevel
	}
	if ko.String("app.env") == "dev" {
		opts.EnableColor = true
	}
	return logf.New(opts)
}

// initConfig loads config to `ko` object.
func initConfig(cfgDefault string, envPrefix string) (*koanf.Koanf, error) {
	var (
		ko = koanf.New(".")
		f  = flag.NewFlagSet("front", flag.ContinueOnError)
	)

	// Configure Flags.
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}

	// Register `--config` flag.
	cfgPath := f.String("config", cfgDefault, "Path to a config file to load.")

	// Parse and Load Flags.
	err := f.Parse(os.Args[1:])
	if err != nil {
		return nil, err
	}

	// Load the config files from the path provided.
	err = ko.Load(file.Provider(*cfgPath), toml.Parser())
	if err != nil {
		return nil, err
	}

	// Load environment variables if the key is given
	// and merge into the loaded config.
	if envPrefix != "" {
		err = ko.Load(env.Provider(envPrefix, ".", func(s string) string {
			return strings.Replace(strings.ToLower(
				strings.TrimPrefix(s, envPrefix)), "__", ".", -1)
		}), nil)
		if err != nil {
			return nil, err
		}
	}

	return ko, nil
}

func initStream(ctx context.Context, ko *koanf.Koanf, cb stream.CallbackFunc) (*stream.Stream, error) {
	s, err := stream.New(
		ko.String("app.data_dir"),
		ko.Duration("app.commit_index_interval"),
		cb,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("error initialising stream")
	}
	return s, nil
}

func initOpts(ko *koanf.Koanf) Opts {
	return Opts{
		maxReconnectAttempts: ko.Int("stream.max_reconnect_attempts"),
		removeAllocDelay:     ko.MustDuration("app.remove_alloc_delay"),
		csvPath:              ko.MustString("app.vector_remap_csv"),
	}
}
