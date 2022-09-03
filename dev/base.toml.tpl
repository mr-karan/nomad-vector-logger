# Directory for Vector data storage:
data_dir ="[[ env "NOMAD_ALLOC_DIR" ]]/data/" # Make sure the user which runs vector has R/W access to this directory.

# Vector's API for introspection
[api]
enabled = true
address = "0.0.0.0:8686"

[sources.vector_logs]
type = "internal_logs"

[sinks.vector_stdout]
type = "console"
inputs = ["vector_logs"]
target = "stdout"

[sinks.vector_stdout.encoding]
codec = "json"
