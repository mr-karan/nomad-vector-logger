package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"text/template"

	"github.com/hashicorp/nomad/api"
)

// fetchRunningAllocs fetches all the current allocations in the cluster.
// It ignores the alloc which aren't running on the current node.
func (app *App) fetchRunningAllocs() (map[string]*api.Allocation, error) {
	allocs := make(map[string]*api.Allocation, 0)

	// Query list of allocations for this node only
	query := &api.QueryOptions{}
	currentAllocs, meta, err := app.nomadClient.Nodes().Allocations(app.nodeID, query)
	if err != nil {
		return nil, err
	}
	app.log.Debug("fetched existing allocs", "count", len(currentAllocs), "took", meta.RequestTime)

	// For each alloc, check if it's running and get the underlying alloc info.
	for _, alloc := range currentAllocs {
		if alloc.ClientStatus != "running" {
			app.log.Debug("ignoring alloc since it's not running", "name", alloc.Name, "status", alloc.ClientStatus)
			continue
		}

		prefix := path.Join(app.opts.nomadDataDir, alloc.ID)
		app.log.Debug("checking if alloc log dir exists", "name", alloc.Name, "alloc_node", alloc.NodeID, "node", app.nodeID)
		_, err := os.Stat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			app.log.Debug("log dir doesn't exist", "dir", prefix, "name", alloc.Name, "alloc_node", alloc.NodeID, "node", app.nodeID)
			// Skip the allocation if it has been GC'ed from host but still the API returned.
			// Unlikely case to happen.
			continue
		} else if err != nil {
			app.log.Error("error checking if alloc dir exists on host", "error", err)
			continue
		}

		allocs[alloc.ID] = alloc
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
