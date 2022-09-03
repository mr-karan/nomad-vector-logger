package main

import (
	"context"
	"sync"
	"time"

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
	configUpdated chan bool
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

	// Start a background worker for updating config.
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.ConfigUpdater(ctx)
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

			// Copy the map of allocs so we don't have to hold the lock longer.
			app.RLock()
			presentAllocs := app.allocs
			app.RUnlock()

			updateCnt := 0
			if len(presentAllocs) > 0 {
				for _, a := range runningAllocs {
					// If an alloc is present in the running list but missing in our map, that means we should add it to our map.
					if _, ok := presentAllocs[a.ID]; !ok {
						app.log.Info("adding new alloc to map", "id", a.ID, "namespace", a.Namespace, "job", a.Job.Name, "group", a.TaskGroup)
						app.Lock()
						app.allocs[a.ID] = a
						app.Unlock()
						updateCnt++
					}
				}
			} else {
				// If this is the first run of the program, `allocs` will be empty.
				// This ideally only happens once when the program boots up.
				app.Lock()
				app.allocs = runningAllocs
				app.Unlock()
				app.configUpdated <- true
				break
			}

			// Only generate config if there were additions.
			if updateCnt > 0 {
				app.configUpdated <- true
			}

			// Now check if present allocs include allocs which aren't running anymore.
			for _, r := range presentAllocs {
				// This means that the alloc id we have in our map isn't running anymore, so enqueue it for deletion.
				if _, ok := runningAllocs[r.ID]; !ok {
					app.log.Info("enqueing non running alloc for deletion", "id", r.ID, "namespace", r.Namespace, "job", r.Job.Name, "group", r.TaskGroup)
					app.expiredAllocs = append(app.expiredAllocs, r.ID)
				}
			}

		case <-ctx.Done():
			app.log.Warn("context cancellation received, quitting update worker")
			return
		}
	}
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
			deleteCnt := 0
			app.Lock()
			for _, id := range app.expiredAllocs {
				app.log.Info("cleaning up alloc", "id", id)
				delete(app.allocs, id)
				deleteCnt++
			}
			// Reset the expired allocs to nil.
			app.expiredAllocs = nil
			app.Unlock()

			// Only generate config if there were deletions.
			if deleteCnt > 0 {
				app.configUpdated <- true
			}

		case <-ctx.Done():
			app.log.Warn("context cancellation received, quitting cleanup worker")
			return
		}
	}
}

func (app *App) ConfigUpdater(ctx context.Context) {
	for {
		select {
		case <-app.configUpdated:
			app.RLock()
			allocs := app.allocs
			app.RUnlock()
			err := app.generateConfig(allocs)
			if err != nil {
				app.log.Error("error generating config", "error", err)
			}

		case <-ctx.Done():
			app.log.Warn("context cancellation received, quitting config worker")
			return
		}
	}
}
