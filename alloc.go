package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// SortedAlloc is to implement the sort interface functions.
// While writing to CSV we want to write entries in a sorted way
// to minimise the number of reloads for vector.
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
	app.log.Debug("removing alloc from map", "id", a.ID)
	delete(app.allocs, a.ID)
}

// fetchExistingAllocs fetches all the current allocations in the cluster.
// This is executed once before listening to events stream.
// In case events stream doesn't have information about the existing allocs running on cluster,
// calling this function ensures that we have an upto-date map of allocations in the cluster.
func (app *App) fetchExistingAllocs() error {
	// Only fetch the allocations running on this noe.
	params := map[string]string{}
	params["filter"] = fmt.Sprintf("NodeID==\"%s\"", app.nodeID)

	// Prepare params for listing alloc.
	query := &api.QueryOptions{
		Params:    params,
		Namespace: "*",
	}

	// Query list of allocs.
	currentAllocs, meta, err := app.stream.Client.Allocations().List(query)
	if err != nil {
		return err
	}
	app.log.Debug("fetched existing allocs", "count", len(currentAllocs), "took", meta.RequestTime)

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

	// Iterate on allocs in the map.
	for _, alloc := range app.allocs {
		// Add metadata for each task in the alloc.
		for task := range alloc.TaskResources {
			// Add task to the data.
			data = append(data, []string{alloc.ID, alloc.Namespace, alloc.JobID, alloc.TaskGroup, task, alloc.NodeName})
		}
	}
	// Sort the entries in the slice.
	sort.Sort(data)
	app.log.Info("generating config with total tasks", "count", len(data))

	// Create a CSV file and get the writer.
	file, err := os.Create(filepath.Join(app.opts.csvPath))
	if err != nil {
		return fmt.Errorf("error creating csv file: %w", err)
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	// Add header for the CSV.
	w.Write([]string{"alloc_id", "namespace", "job", "group", "task", "node"})

	// Write records to the CSV.
	if err := w.WriteAll(data); err != nil {
		return fmt.Errorf("error writing csv: %w", err)
	}

	if err := w.Error(); err != nil {
		return fmt.Errorf("error writing csv: %w", err)
	}

	return nil
}
