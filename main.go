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
	//go:embed vector.toml.tmpl
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

	// Initialise a new instance of app.
	app := App{
		log:           initLogger(ko),
		opts:          initOpts(ko),
		configUpdated: make(chan bool, 1000),
		allocs:        make(map[string]*api.Allocation, 0),
		expiredAllocs: make([]string, 0),
	}

	// Initialise nomad events stream.
	client, err := initNomadClient()
	if err != nil {
		app.log.Fatal("error initialising client", "err", err)
	}
	app.nomadClient = client

	// Set the node id in app.
	self, err := app.nomadClient.Agent().Self()
	if err != nil {
		app.log.Fatal("error fetching self endpoint", "err", err)
	}
	app.nodeID = self.Stats["client"]["node_id"]
	app.log.Info("setting node id in the app", "node", app.nodeID)

	// Start an instance of app.
	app.log.Info("booting nomad-vector-logger", "version", buildString)
	app.Start(ctx)
}
