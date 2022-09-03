[app]
log_level = "debug" # `debug` for verbose logs. `info` otherwise.
env = "dev" # dev|prod.
refresh_interval = "10s" # Interval at which list of allocations is updated.
remove_alloc_interval = "30s" # If the alloc is completed or stopped, the allocation isn't removed immediately from vector's config. You can delay the removal of alloc by `n` duration to ensure that vector has finished collecting all logs till then.
nomad_data_dir = "/opt/nomad/data/alloc" # Nomad data directory where alloc logs are stored.
vector_config_dir = "{{ env "NOMAD_ALLOC_DIR" }}/vector_gen_configs" # Path to the generated vector config file.
extra_templates_dir = "{{ env "NOMAD_TASK_DIR" }}/static/" #  Extra templates that can be given. They will be rendered in `$vector_config_dir`. You can use variables mentioned in vector.tmpl if required.
