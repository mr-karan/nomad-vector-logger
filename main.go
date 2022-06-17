package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hashicorp/nomad/api"
)

var (
	//go:embed vector.tmpl
	vectorTmpl embed.FS
	// Version of the build. This is injected at build-time.
	buildString = "unknown"
)

func main() {
	// Create a new context which gets cancelled upon receiving `SIGINT`/`SIGTERM`.
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Initialise and load the config.
	ko, err := initConfig("config.sample.toml", "NOMAD_VECTOR_LOGGER")
	if err != nil {
		fmt.Println("error initialising config", err)
		os.Exit(1)
	}

	log, err := initLogger(ko)
	if err != nil {
		fmt.Println("error initialising logger", err)
		os.Exit(1)
	}

	var (
		opts = initOpts(ko)
	)

	// Initialise a new instance of app.
	app := App{
		log:    log,
		opts:   opts,
		allocs: make(map[string]*api.Allocation, 0),
	}

	// Initialise nomad events stream.
	strm, err := initStream(ctx, ko, app.handleEvent)
	if err != nil {
		app.log.Fatalw("error initialising stream", "err", err)
	}
	app.stream = strm

	// Set the node id in app.
	nodeID, err := app.stream.NodeID()
	if err != nil {
		app.log.Fatalw("error fetching node id", "err", err)
	}
	app.nodeID = nodeID

	// Start an instance of app.
	app.log.Infow("booting nomad alloc logger",
		"version", buildString,
	)
	app.Start(ctx)
}
