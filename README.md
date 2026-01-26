# initd

**Systemd-compatible service management for environments without PID 1**

`initd` is a lightweight init daemon that provides systemd-style service management
without requiring systemd or PID 1.

It allows unmodified systemd service files to run normally in environments where
systemd cannot run, such as Docker containers, Android chroot/proot, embedded systems,
and other restricted Linux environments.

---

## Why initd

In many environments, systemd is unavailable or impractical:

- Docker containers (no PID 1 or restricted init)
- Android chroot / proot
- Embedded Linux systems
- Minimal or constrained Linux environments

As a result, users are often forced to:
- Write custom startup scripts
- Manually run daemons in separate terminals
- Use ad-hoc supervisors instead of systemd services

`initd` solves this by **keeping the systemd service model** while removing systemd’s
heavy runtime requirements.

You write normal `*.service` files.
You use familiar `systemctl` commands.
Services start automatically with the container or chroot.

---

## Features

- Runs **unmodified systemd service files**
- `systemctl` interface compatible with systemd usage
- Does **not** require PID 1
- Works in:
  - Docker containers
  - Android chroot / proot
  - Embedded Linux
- Uses a Unix domain socket (`/run/initd.sock`) instead of D-Bus
- Can run as:
  - a **service manager daemon**
  - a **full init / supervisor**

---

## Architecture

- `initd` runs as a daemon and manages service lifecycles
- `systemctl` communicates with `initd` via a Unix socket
- No dependency on:
  - systemd
  - D-Bus
  - cgroups
- File permissions control access to the control socket

---

## Usage

### initd

```sh
./initd -h
Usage of ./initd:
  -init
        run as init/supervisor
  -socket string
        path to initd unix socket (default "/run/initd.sock")
```

Modes:

- **Socket-only mode (default)**
   Starts the control daemon only
   Does NOT automatically start services
   (not recommended for normal use)
- **Init mode (`--init`)**
   Starts initd as a supervisor
   Automatically starts all enabled services
   Recommended for containers and chroot environments

### systemctl

```
systemctl [OPTIONS...] COMMAND [UNIT...]
Query or send control commands to the initd system manager.

Options:
  --socket=PATH        Path to initd control socket
  -h, --help           Show this help
  -V, --version        Show version

Commands:
  start UNIT...        Start (activate) one or more units
  stop UNIT...         Stop (deactivate) one or more units
  restart UNIT...      Restart one or more units
  status UNIT...       Show runtime status of one or more units
  is-active UNIT...    Check whether units are active
  is-enabled UNIT...   Check whether unit files are enabled
  enable UNIT...       Enable one or more unit files
  disable UNIT...      Disable one or more unit files
  list-units           List loaded units
  list-unit-files      List installed unit files
  daemon-reload        Reload unit files
```

The usage is intentionally very close to systemd’s `systemctl`.

## Examples

### Nginx

```
sudo systemctl start nginx
sudo systemctl status nginx
● nginx.service - A high performance web server and a reverse proxy server
   Loaded: loaded (nginx.service; enabled)
   Active: active (running) since Mon, 26 Jan 2026 22:57:51 PST
 Main PID: 12717
```

### SSH

```
sudo systemctl daemon-reload
sudo systemctl status ssh
● ssh.service - OpenBSD Secure Shell server
   Loaded: loaded (ssh.service; enabled)
   Active: active (running) since Mon, 26 Jan 2026 22:57:51 PST
 Main PID: 12739

Logs:
  INFO: Service type notify not supported; treating as simple
```

### list-units

```
systemctl list-units
UNIT    LOAD    ACTIVE  DESCRIPTION
ipp-usb.service loaded  inactive        Daemon for IPP over USB printer support
systemd-journald.service        loaded  inactive        Journal Service
systemd-suspend-then-hibernate.service  loaded  inactive        System Suspend then Hibernate
......
```

------

## When to Use initd

Recommended when:

- systemd cannot run (no PID 1)
- systemd is too heavy or restricted
- You want systemd-style service management without systemd

Especially useful for:

- Docker containers
- Android chroot / proot
- Embedded Linux systems

------

## Important Notes

- Do **NOT** run `initd --init` together with systemd
   Running two init systems simultaneously will cause conflicts
- In environments where systemd is already active (e.g. WSL with systemd),
   only use one init system at a time

------

## Build

### Requirements

- Go (recent version)

### Build commands

```
go build ./cmd/initd
go build ./cmd/systemctl
```

------

## Installation

Recommended installation path:

```
/usr/local/bin
```

This avoids conflicts with system-provided systemd binaries on Debian/Ubuntu systems.

After installation, verify that `systemctl` points to the initd version.

------

## Recommended Startup

Use init mode for full service management:

```
/usr/local/bin/initd --init
```

All enabled services will start automatically.

------

## License

MIT License
