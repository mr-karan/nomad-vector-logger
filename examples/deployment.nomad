job "vector" {
  datacenters = ["dc1"]
  namespace   = "default"
  type        = "system"

  group "vector" {
    count = 1
    restart {
      attempts = 60
      interval = "1m"
      delay    = "1s"
      mode     = "fail"
    }

    network {
      port "metrics" {
        to = 9598
      }
      mode = "bridge"
    }

    service {
      name     = "vector-agent-monitoring"
      port     = "metrics"
      provider = "nomad"
      tags = [
        "monitoring",
      ]
    }

    # For vector's data directory.
    ephemeral_disk {
      size   = 500
      sticky = true
    }

    # Sidecar job to template out CSVs.
    task "nomad_vector_logger" {
      driver = "docker"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      config {
        # This image should be built with goreleaser for local development.
        image   = "mr-karan/nomad-vector-logger:local"
        command = "/app/nomad-vector-logger.bin"
        args    = ["--config", "$${NOMAD_TASK_DIR}/config.toml"]
      }


      env {
        # Address of local Nomad running on `0.0.0.0/0`
        NOMAD_ADDR = "http://192.168.29.76:4646"
      }

      resources {
        cores  = 1
        memory = 1000
      }

      # Template with Vector's configuration
      template {
        data        = file(abspath("./examples/config.tpl.toml"))
        destination = "$${NOMAD_TASK_DIR}/config.toml"
        change_mode = "restart"
      }

    }

    # Main vector app.
    task "vector" {
      driver = "raw_exec"

      config {
        command = "vector"
      }
      # Vector won't start unless the sinks(backends) configured are healthy
      env {
        VECTOR_CONFIG          = "$${NOMAD_TASK_DIR}/vector.toml"
        VECTOR_REQUIRE_HEALTHY = "true"
        VECTOR_THREADS         = 2
      }

      resources {
        cores  = 2
        memory = 2000
      }

      # Template with Vector's configuration
      template {
        data          = file(abspath("./examples/vector.tpl.toml"))
        destination   = "$${NOMAD_TASK_DIR}/vector.toml"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        # overriding the delimiters to [[ ]] to avoid conflicts with Vector's native templating, which also uses {{ }}
        left_delimiter  = "[["
        right_delimiter = "]]"
      }

      kill_timeout = "60s"
    }


    # Task to reload vector.
    # Issue: https://github.com/vectordotdev/vector/issues/13229#issuecomment-1160589834
    # Until this is resolved, the current SIGHUP will end up restarting Vector itself.
    task "vector_reloader" {
      driver = "raw_exec"

      lifecycle {
        hook    = "poststart"
        sidecar = true
      }

      config {
        command = "$${NOMAD_TASK_DIR}/reload.sh"
        args    = ["$${NOMAD_ALLOC_DIR}/nomad_remap.csv"]
      }
      template {
        data        = <<EOH
#!/usr/bin/env bash

set -Eeuo pipefail

filename="$1"

m1=$(md5sum "$filename")

while true; do

  # md5sum is computationally expensive, so check only once every 10 seconds
  sleep 10

  m2=$(md5sum "$filename")

  if [ "$m1" != "$m2" ] ; then
    echo $(date -u) "INFO: old hash is $m1"
    echo $(date -u) "INFO: new hash is $m2"
    echo $(date -u) "INFO: File has changed!"
    killall -s SIGHUP vector || true
    echo $(date -u) "INFO: Reloaded vector"

    # Update the hash for next reconcilation.
    echo $(date -u) "INFO: Updating old hash as $m2 from $m1"
    m1=$m2

  fi
done
  EOH
        destination = "$${NOMAD_TASK_DIR}/reload.sh"
        change_mode = "restart"
      }
    }
  }
}
