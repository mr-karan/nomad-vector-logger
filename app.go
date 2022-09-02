package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

type SortedAlloc [][]string

func (s SortedAlloc) Len() int {
	return len(s)
}

func (s SortedAlloc) Less(i, j int) bool {
	d1 := s[i]
	d2 := s[j]
	s1 := strings.Join(d1, "")
	s2 := strings.Join(d2, "")

	return s1 < s2
}

func (s SortedAlloc) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type Interface interface {
	// Len is the number of elements in the collection.
	Len() int

	// Less reports whether the element with index i must sort before the element with index j.
	// If both Less(i, j) and Less(j, i) are false, then the elements at index i and j are considered equal.
	Less(i, j int) bool

	// Swap swaps the elements with indexes i and j.
	Swap(i, j int)
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

// AddAlloc adds an allocation to the map.
func (app *App) AddAlloc(a *api.Allocation) {
	app.Lock()
	defer app.Unlock()
	app.log.Info("adding alloc to map", "id", a.ID)
	app.allocs[a.ID] = a
}

// RemoveAlloc removes the alloc from the map.
func (app *App) RemoveAlloc(a *api.Allocation) {
	app.Lock()
	defer app.Unlock()
	app.log.Info("removing alloc to map", "id", a.ID)
	delete(app.allocs, a.ID)
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
			// Add to the queue.
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

// fetchExistingAllocs fetches all the current allocations in the cluster.
// This is executed once before listening to events stream.
// In case events stream doesn't have information about the existing allocs running on cluster,
// calling this function ensures that we have an upto-date map of allocations in the cluster.
func (app *App) fetchExistingAllocs() error {
	currentAllocs, _, err := app.stream.Client.Allocations().List(&api.QueryOptions{Namespace: "*"})
	if err != nil {
		return err
	}

	for _, allocStub := range currentAllocs {
		// Skip the allocations which aren't running on this node.
		if allocStub.NodeID != app.nodeID {
			continue
		}

		if alloc, _, err := app.stream.Client.Allocations().Info(allocStub.ID, &api.QueryOptions{Namespace: allocStub.Namespace}); err != nil {
			app.log.Error("unable to fetch alloc info: %w", err)
			continue
		} else {
			app.AddAlloc(alloc)
			switch allocStub.ClientStatus {
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
					}
				})
			}
		}
	}

	// Generate a config once all allocs are added to the map.
	err = app.generateConfig()
	if err != nil {
		app.log.Error("error generating config", "error", err)
		return err
	}
	return nil
}

// generateConfig generates a vector config file by iterating on a
// map of allocations in the cluster and adding some extra metadata about the alloc.
// It creates a config file on the disk which vector is _live_ watching and reloading
// whenever it changes.
func (app *App) generateConfig() error {
	// Iterate through map of allocs.
	app.RLock()
	defer app.RUnlock()

	// Collect the metdata for writing each row in CSV in a slice.
	data := make(SortedAlloc, 0)
	// Add header for the CSV.
	data = append(data, []string{"alloc_id", "namespace", "job", "group", "task", "node"})

	// Iterate on allocs in the map.
	for _, alloc := range app.allocs {
		// Add metadata for each task in the alloc.
		for task := range alloc.TaskResources {
			// Add task to the data.
			data = append(data, []string{alloc.ID, alloc.Namespace, alloc.JobID, alloc.TaskGroup, task, alloc.NodeName})
		}
	}

	app.log.Info("generating config with total tasks", "count", len(data))
	file, err := os.Create(filepath.Join(app.opts.csvPath))
	if err != nil {
		return fmt.Errorf("error creating csv file: %w", err)
	}
	defer file.Close()

	sort.Sort(data)

	w := csv.NewWriter(file)
	if err := w.WriteAll(data); err != nil {
		return fmt.Errorf("error writing csv: %w", err)
	}

	if err := w.Error(); err != nil {
		return fmt.Errorf("error writing csv: %w", err)
	}

	return nil
}
