package main

import (
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"errors"

	"github.com/hashicorp/nomad/api"
	"github.com/mr-karan/nomad-events-sink/pkg/stream"
	"github.com/zerodha/logf"
)

type Opts struct {
	maxReconnectAttempts int
	nomadDataDir         string
	removeAllocDelay     time.Duration
	vectorConfigDir      string
	extraTemplatesDir    string
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
	app.log.Debug("fetching existing allocs")
	if err := app.fetchExistingAllocs(); err != nil {
		app.log.Fatal("error fetching allocations", "error", err)
	}

	app.log.Info("added allocations to the map", "count", len(app.allocs))

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

	app.log.Debug("fetched existing allocs", "count", len(currentAllocs))

	for _, allocStub := range currentAllocs {
		// Skip the allocations which aren't running on this node.
		if allocStub.NodeID != app.nodeID {
			app.log.Debug("skipping alloc because it doesn't run on this node", "name", allocStub.Name, "id", allocStub.ID)
			continue
		}

		prefix := fmt.Sprintf(app.opts.nomadDataDir, allocStub.ID)
		_, err := os.Stat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			// Skip the allocation if it has been GC'ed from host but still the API returned.
			// Unlikely case to happen.
			continue
		} else if err != nil {
			app.log.Error("error checking if alloc dir exists on host: %v", err)
			continue
		}

		if alloc, _, err := app.stream.Client.Allocations().Info(allocStub.ID, &api.QueryOptions{Namespace: allocStub.Namespace}); err != nil {
			app.log.Error("unable to fetch alloc info: %v", err)
			continue
		} else {
			app.log.Debug("adding alloc to queue", "name", alloc.Name, "id", alloc.ID)
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

	// Create a config dir to store templates.
	if err := os.MkdirAll(app.opts.vectorConfigDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating dir %s: %v", app.opts.vectorConfigDir, err)
	}

	// Load the vector config template.
	tpl, err := template.ParseFS(vectorTmpl, "vector.tmpl")
	if err != nil {
		return fmt.Errorf("unable to parse template: %v", err)
	}

	// Special case to handle where there are no allocs.
	// If this is the case, the user supplie config which relies on sources/transforms
	// as the input for the sink can fail as the generated `nomad.toml` will be empty.
	// To avoid this, remove all files inside the existing config dir and exit the function.
	if len(app.allocs) == 0 {
		app.log.Info("no current alloc is running, cleaning up config dir", "vector_dir", app.opts.vectorConfigDir)
		dir, err := ioutil.ReadDir(app.opts.vectorConfigDir)
		if err != nil {
			return fmt.Errorf("error reading vector config dir")
		}
		for _, d := range dir {
			if err := os.RemoveAll(path.Join([]string{app.opts.vectorConfigDir, d.Name()}...)); err != nil {
				return fmt.Errorf("error cleaning up config dir")
			}
		}
		return nil
	}

	data := make([]AllocMeta, 0)

	// Iterate on allocs in the map.
	for _, alloc := range app.allocs {
		// Add metadata for each task in the alloc.
		for task := range alloc.TaskResources {
			// Add task to the data.
			data = append(data, AllocMeta{
				Key:       fmt.Sprintf("nomad_alloc_%s_%s", alloc.ID, task),
				ID:        alloc.ID,
				LogDir:    filepath.Join(fmt.Sprintf("%s/%s", app.opts.nomadDataDir, alloc.ID), "alloc/logs/"+task+"*"),
				Namespace: alloc.Namespace,
				Group:     alloc.TaskGroup,
				Node:      alloc.NodeName,
				Task:      task,
				Job:       alloc.JobID,
			})
		}
	}

	app.log.Info("generating config with total tasks", "count", len(data))
	file, err := os.Create(filepath.Join(app.opts.vectorConfigDir, "nomad.toml"))
	if err != nil {
		return fmt.Errorf("error creating vector config file: %v", err)
	}
	defer file.Close()

	if err := tpl.Execute(file, data); err != nil {
		return fmt.Errorf("error executing template: %v", err)
	}

	// Load all user provided templates.
	if app.opts.extraTemplatesDir != "" {
		// Loop over all files mentioned in the templates dir.
		files, err := ioutil.ReadDir(app.opts.extraTemplatesDir)
		if err != nil {
			return fmt.Errorf("error opening extra template file: %v", err)
		}

		// For all files, template it out and store in vector config dir.
		for _, file := range files {
			// Load the vector config template.
			t, err := template.ParseFiles(filepath.Join(app.opts.extraTemplatesDir, file.Name()))
			if err != nil {
				return fmt.Errorf("unable to parse template: %v", err)
			}

			// Create the underlying file.
			f, err := os.Create(filepath.Join(app.opts.vectorConfigDir, file.Name()))
			if err != nil {
				return fmt.Errorf("error creating extra template file: %v", err)
			}
			defer f.Close()

			if err := t.Execute(f, data); err != nil {
				return fmt.Errorf("error executing extra template: %v", err)
			}
		}
	}

	return nil
}
