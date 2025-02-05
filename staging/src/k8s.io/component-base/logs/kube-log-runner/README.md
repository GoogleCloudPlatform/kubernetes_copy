# `kube-log-runner` (formerly known as go-runner)

The `kube-log-runner` is a Go based binary that can run commands and redirect stdout/stderr etc.

Why do we need this?

- Some of our images like kube-apiserver used bash output redirection for
  collecting logs, so we were not able to switch to distroless images directly
  for these images. The klog's `--log-file` parameter was supposed to fix this
  problem, but we ran into trouble with that in scalability CI jobs that never
  could get root caused and fixed. Using this binary worked.

- Windows services don't have a mechanism for redirecting output of a process.

- Nowadays, the `--log-file` parameter is deprecated for Kubernetes components
  and should not be used anymore. `kube-log-runner` is a direct replacement.

## Flags

- -enable-flush
  The `-enable-flush` flag is an optional boolean flag that controls whether the 
  log file content is periodically flushed to disk.

  Type: Boolean
  
  Default: false (flushing disabled)
  
  Usage: When set to true, the log file is flushed every 5 seconds. 
  While this may not be necessary on Linux, on Windows, it ensures that recent log 
  entries are written to disk in near real-time, which is particularly useful for 
  users who need to monitor logs as they are generated without delay.

- -log-file-size
  The `-log-file-size` flag is an optional string flag that sets a size limit for 
  the log file, triggering automatic log rotation when the specified size is reached.
  This is especially useful in production environments where the log file may become
  too large to view effectively.

  Type: String (expects a value in Resource.Quantity format, such as 10M or 500K)

  Default: "0" (disabled, no automatic rotation of log files)

  Usage: When set to a positive value, the log file will rotate upon reaching the specified 
  size limit. The current log file’s contents will be saved to a backup file, and a new log 
  file will be created at the path specified by the `-log-file` flag, ready for future log entries.

  Backup File Naming Convention:
    `<original-file-name>-<timestamp><file-extension>`.
    `<original-file-name>`: The name of the original log file, without the file extension.
    `<timestamp>`: A timestamp is added to each backup file’s name to uniquely identify it
    based on the time it was created. The timestamp follows the format "20060102-150405".
    For example, a backup created on June 2, 2006, at 3:04:05 PM would include this timestamp.
    `<file-extension>`: The original file’s extension (e.g., .log) remains unchanged.
  This naming convention ensures easy organization and retrieval of rotated log files based on their creation time.

- -log-file-age
  The `-log-file-age` flag is an optional time duration setting that defines how long 
  old backup log files are retained. This flag is used alongside log rotation (enabled 
  by setting a positive value for -log-file-size) to help manage storage by removing 
  outdated backup logs.

  Type: Duration
  
  Default: 0 (disabled, no automatic deletion of backup files)
  
  Usage: When -log-file-age is set to a positive duration (e.g., 24h for 24 hours) 
  and log rotation is enabled, backup log files will be automatically deleted if 
  their last modification time is older than the specified duration from the current time.
  
  This ensures that only recent backup logs are kept, preventing accumulation of old logs 
  and reducing storage usage.

For example instead of running kube-apiserver like this:
```bash
"/bin/sh",
  "-c",
  "exec kube-apiserver {{params}} --allow-privileged={{pillar['allow_privileged']}} 1>>/var/log/kube-apiserver.log 2>&1"
```

Or this:
```bash
kube-apiserver {{params}} --allow-privileged={{pillar['allow_privileged']}} --log-file=/var/log/kube-apiserver.log --alsologtostderr=false"
```

We would use `kube-log-runner` like so:
```bash
kube-log-runner -log-file=/var/log/kube-apiserver.log --also-stdout=false \
   kube-apiserver {{params}} --allow-privileged={{pillar['allow_privileged']}}
```

The kube-log-runner then ensures that we run the
`/usr/local/bin/kube-apiserver` with the specified parameters and redirect both
stdout and stderr ONLY to the log file specified. It will always append to the
log file.

Possible invocations:
```bash
# Merge stderr and stdout, write to stdout (same as 2>&1).
kube-log-runner echo "hello world"

# Redirect both into log file (same as 1>>/tmp/log 2>&1).
kube-log-runner -log-file=/tmp/log echo "hello world"

# Copy into log file and print to stdout (same as 2>&1 | tee -a /tmp/log).
kube-log-runner -log-file=/tmp/log -also-stdout echo "hello world"

# Copy into log file and print to stdout (same as 2>&1 | tee -a /tmp/log), 
# will flush the logging file in 5s, 
# rotate the log file when its size exceedes 10 MB
kube-log-runner -enable-flush=true -log-file=/tmp/log -log-file-size=10M -also-stdout echo "hello world"

# Copy into log file and print to stdout (same as 2>&1 | tee -a /tmp/log), 
# will flush the logging file in 5s, 
# rotate the log file when its size exceedes 10 MB, 
# and clean up old rotated log files when their age are older than 168h (7 days)
kube-log-runner -enable-flush=true -log-file=/tmp/log -log-file-size=10M -log-file-age=168h -also-stdout echo "hello world"

# Redirect only stdout into log file (same as 1>>/tmp/log).
kube-log-runner -log-file=/tmp/log -redirect-stderr=false echo "hello world"
```

# Container base image

The Kubernetes
[`registry.k8s.io/build-image/go-runner`](https://console.cloud.google.com/gcr/images/k8s-artifacts-prod/us/build-image/go-runner)
image wraps the `gcr.io/distroless/static` image and provides `kube-log-runner`
under its traditional name as `/go-runner`. It gets maintained in
https://github.com/kubernetes/release/tree/master/images/build/go-runner.

# Prebuilt binary

The Kubernetes release archives contain kube-log-runner.
