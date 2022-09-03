# Whitelist selected namespaces, drop the rest.
[transforms.route_logs]
type = "route"
inputs = ["transform_nomad_alloc*"]
# Route conditions
route."nginx" = '.nomad.job_name == "nginx"'

[sinks.stdout_nomad]
type = "console"
inputs = ["route_logs.nginx"]
target = "stdout"
encoding.codec = "json"
