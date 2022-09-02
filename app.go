package main

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mr-karan/nomad-events-sink/pkg/stream"
	"github.com/zerodha/logf"
)

type Opts struct {
	maxReconnectAttempts int
	removeAllocDelay     time.Duration
	csvPath              string
}

// App is the global container that holds
// objects of various routines that run on boot.
type App struct {
	sync.RWMutex

	log    logf.Logger
	stream *stream.Stream
	opts   Opts

	// Map of Alloc ID and Allocation object running in the cluster.
	allocs map[string]*api.Allocation

	// Self NodeID where this program is running.
	nodeID string
}

type AllocMeta struct {
	Key       string
	ID        string
	LogDir    string
	Job       string
	Namespace string
	Task      string
	Node      string
	Group     string
}

// Start initialises the subscription stream in background and waits
// for context to be cancelled to exit.
func (app *App) Start(ctx context.Context) {
	wg := &sync.WaitGroup{}

	// Before we start listening to the event stream, fetch all current allocs in the cluster
	// running on this node.
	if err := app.fetchExistingAllocs(); err != nil {
		app.log.Fatal("error initialising index store", "error", err)
	}

	// Initialise index store from disk to continue reading
	// from last event which is processed.
	if err := app.stream.InitIndex(ctx); err != nil {
		app.log.Fatal("error initialising index store", "error", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		// Subscribe to "Allocation" topic.
		if err := app.stream.Subscribe(ctx, string(api.TopicAllocation), app.opts.maxReconnectAttempts); err != nil {
			app.log.Error("error subscribing to events", "topic", string(api.TopicAllocation), "error", err)
		}
	}()

	// Wait for all routines to finish.
	wg.Wait()
}

// handleEvent is the callback function that is registered with stream. This function
// is called whenever a new event comes in the stream.
func (app *App) handleEvent(e api.Event, meta stream.Meta) {
	if e.Topic == api.TopicAllocation {
		alloc, err := e.Allocation()
		if err != nil {
			app.log.Error("error fetching allocation", "error", err)
			return
		}
		if alloc.NodeID != meta.NodeID {
			app.log.Debug("ignoring the alloc because it's for a different node", "node", meta.NodeID, "event_alloc_node", alloc.NodeID)
			return
		}

		app.log.Debug("received allocation event",
			"type", e.Type,
			"id", alloc.ID,
			"name", alloc.Name,
			"namespace", alloc.Namespace,
			"group", alloc.TaskGroup,
			"job", alloc.JobID,
			"status", alloc.ClientStatus,
		)

		switch alloc.ClientStatus {
		case "pending", "running":
			// Add to the map.
			app.log.Info("adding alloc", "id", alloc.ID)
			app.AddAlloc(alloc)

			// Generate config.
			app.log.Info("generating config after adding alloc", "index", e.Index)
			err = app.generateConfig()
			if err != nil {
				app.log.Error("error generating config", "error", err)
				return
			}
		case "complete", "failed":
			app.log.Info("scheduled removing of alloc", "id", alloc.ID, "duration", app.opts.removeAllocDelay)
			// Remove from the queue, but with a delay so that all the logs are collected by that time.
			time.AfterFunc(app.opts.removeAllocDelay, func() {
				app.log.Info("removing alloc", "id", alloc.ID)
				app.RemoveAlloc(alloc)

				// Generate config.
				app.log.Info("generating config after alloc removal", "id", alloc.ID)
				err = app.generateConfig()
				if err != nil {
					app.log.Error("error generating config", "error", err)
					return
				}
			})
		default:
			app.log.Warn("unable to handle event with this status",
				"status", alloc.ClientStatus,
				"desc", alloc.ClientDescription,
				"id", alloc.ID)
		}
	}
}
