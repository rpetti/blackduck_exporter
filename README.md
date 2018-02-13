# BlackDuck Exporter

This is a relatively simple Prometheus exporter for exposing job statistics.

## Basic Usage

```
docker run rpetti/blackduck_exporter \
    -blackduck.url=https://yourblackduckserver.com \
    -blackduck.username=blackduck_user \
    -blackduck.password.file=/file/containing/password \
    -telemetry.address=:9125 \
    -telemetry.endpoint=/metrics
```

Run with -h to see all options.

Default port is `9125`, and the default endpoint is `/metrics`.