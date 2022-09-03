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
	"github.com/zerodha/logf"
)

type Opts struct {
	refreshInterval     time.Duration
	removeAllocInterval time.Duration
	nomadDataDir        string
	vectorConfigDir     string
	extraTemplatesDir   string
}

// App is the global container that holds
// objects of various routines that run on boot.
type App struct {
	sync.RWMutex
	log           logf.Logger
	opts          Opts
	nomadClient   *api.Client
	nodeID        string                     // Self NodeID where this program is running.
	allocs        map[string]*api.Allocation // Map of Alloc ID and Allocation object running in the cluster.
	expiredAllocs []string
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

	// Start a background worker for fetching allocs and generating template.
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.UpdateAllocs(ctx, app.opts.refreshInterval)
	}()

	// Start a background worker for removing expired allocs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.CleanupAllocs(ctx, app.opts.removeAllocInterval)
	}()

	// Wait for all routines to finish.
	wg.Wait()
}

// UpdateAllocs fetches Nomad allocs from all the namespaces
// at periodic interval and generates the templates.
// This is a blocking function so the caller must invoke as a goroutine.
func (app *App) UpdateAllocs(ctx context.Context, refreshInterval time.Duration) {
	var (
		ticker = time.NewTicker(refreshInterval).C
	)

	for {
		select {
		case <-ticker:
			// Fetch the list of allocs running on this node.
			runningAllocs, err := app.fetchRunningAllocs()
			if err != nil {
				app.log.Error("error fetching allocs", "error", err)
				continue
			}

			// Compare the running allocs with the one that's already present in the map.
			app.RLock()

			// If this is the first run of the program, `app.allocs` will be empty.
			if len(app.allocs) > 0 {
				for _, a := range runningAllocs {
					// If an alloc is present in the running list but missing in our map, that means we should add it to our map.
					if _, ok := app.allocs[a.ID]; !ok {
						app.log.Info("adding new alloc to map", "id", a.ID, "namespace", a.Namespace, "job", a.Job.Name, "group", a.TaskGroup)
						app.allocs[a.ID] = a
					}
				}
			} else {
				// This ideally only happens once when the program boots up.
				app.allocs = runningAllocs
			}

			// Now check if the allocs inside our map are missing any running allocs.
			for _, r := range app.allocs {
				// This means that the alloc id we have in our map isn't running anymore, so enqueue it for deletion.
				if _, ok := runningAllocs[r.ID]; !ok {
					app.log.Info("alloc not found as running, enqueuing for deletion", "id", r.ID, "namespace", r.Namespace, "job", r.Job.Name, "group", r.TaskGroup)
					app.expiredAllocs = append(app.expiredAllocs, r.ID)
				}
			}

			// Making a copy of map so we don't have to hold the lock for longer.
			presentAllocs := app.allocs
			app.RUnlock()

			// Generate a config once all allocs are added to the map.
			err = app.generateConfig(presentAllocs)
			if err != nil {
				app.log.Error("error generating config", "error", err)
				continue
			}

		case <-ctx.Done():
			app.log.Warn("context cancellation received, quitting update worker")
			return
		}
	}
}

// fetchRunningAllocs fetches all the current allocations in the cluster.
// It ignores the alloc which aren't running on the current node.
func (app *App) fetchRunningAllocs() (map[string]*api.Allocation, error) {

	allocs := make(map[string]*api.Allocation, 0)

	// Only fetch the allocations running on this noe.
	params := map[string]string{}
	params["filter"] = fmt.Sprintf("NodeID==\"%s\"", app.nodeID)

	// Prepare params for listing alloc.
	query := &api.QueryOptions{
		Params:    params,
		Namespace: "*",
	}

	// Query list of allocs.
	currentAllocs, meta, err := app.nomadClient.Allocations().List(query)
	if err != nil {
		return nil, err
	}
	app.log.Debug("fetched existing allocs", "count", len(currentAllocs), "took", meta.RequestTime)

	// For each alloc, check if it's running and get the underlying alloc info.
	for _, allocStub := range currentAllocs {
		if allocStub.ClientStatus != "running" {
			app.log.Debug("ignoring alloc since it's not running", "name", allocStub.Name, "status", allocStub.ClientStatus)
			continue
		}
		// Skip the allocations which aren't running on this node.
		if allocStub.NodeID != app.nodeID {
			app.log.Debug("skipping alloc because it doesn't run on this node", "name", allocStub.Name, "alloc_node", allocStub.NodeID, "node", app.nodeID)
			continue
		} else {
			app.log.Debug("alloc belongs to the current node", "name", allocStub.Name, "alloc_node", allocStub.NodeID, "node", app.nodeID)
		}

		prefix := path.Join(app.opts.nomadDataDir, allocStub.ID)
		app.log.Debug("checking if alloc log dir exists", "name", allocStub.Name, "alloc_node", allocStub.NodeID, "node", app.nodeID)
		_, err := os.Stat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			app.log.Debug("log dir doesn't exist", "dir", prefix, "name", allocStub.Name, "alloc_node", allocStub.NodeID, "node", app.nodeID)
			// Skip the allocation if it has been GC'ed from host but still the API returned.
			// Unlikely case to happen.
			continue
		} else if err != nil {
			app.log.Error("error checking if alloc dir exists on host", "error", err)
			continue
		}

		if alloc, _, err := app.nomadClient.Allocations().Info(allocStub.ID, &api.QueryOptions{Namespace: allocStub.Namespace}); err != nil {
			app.log.Error("unable to fetch alloc info", "error", err)
			continue
		} else {
			allocs[alloc.ID] = alloc
		}
	}

	// Return map of allocs.
	return allocs, nil
}

// generateConfig generates a vector config file by iterating on a
// map of allocations in the cluster and adding some extra metadata about the alloc.
// It creates a config file on the disk which vector is _live_ watching and reloading
// whenever it changes.
func (app *App) generateConfig(allocs map[string]*api.Allocation) error {

	// Create a config dir to store templates.
	if err := os.MkdirAll(app.opts.vectorConfigDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating dir %s: %v", app.opts.vectorConfigDir, err)
	}

	// Load the vector config template.
	tpl, err := template.ParseFS(vectorTmpl, "vector.toml.tmpl")
	if err != nil {
		return fmt.Errorf("unable to parse template: %v", err)
	}

	// Special case to handle where there are no allocs.
	// If this is the case, the user supplie config which relies on sources/transforms
	// as the input for the sink can fail as the generated `nomad.toml` will be empty.
	// To avoid this, remove all files inside the existing config dir and exit the function.
	if len(allocs) == 0 {
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
	for _, alloc := range allocs {
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

// CleanupAllocs removes the alloc from the map which are marked for deletion.
// These could be old allocs that aren't running anymore but which need to be removed from
// the config after a delay to ensure Vector has finished pushing all logs to upstream sink.
func (app *App) CleanupAllocs(ctx context.Context, removeAllocInterval time.Duration) {
	var (
		ticker = time.NewTicker(removeAllocInterval).C
	)

	for {
		select {
		case <-ticker:
			app.Lock()
			for _, id := range app.expiredAllocs {
				app.log.Info("cleaning up alloc", "id", id)
				delete(app.allocs, id)
			}
			app.Unlock()

		case <-ctx.Done():
			app.log.Warn("context cancellation received, quitting cleanup worker")
			return
		}
	}
}
