# initd

**A lightweight, systemd-compatible init system and service manager**

`initd` is a modern, lightweight init system and service manager designed as a
practical replacement for systemd in constrained, containerized, and embedded
Linux environments.

It preserves the familiar systemd service model and `systemctl` workflow,
while removing systemd’s heavy runtime dependencies and assumptions.

`initd` can run either as a standalone service manager or as a full init
process (PID 1).

---

## What is initd

`initd` is an init system and service supervisor that runs unmodified
systemd `*.service` files without requiring systemd itself.

It is designed for environments where systemd is unavailable, restricted,
or unnecessarily heavy, while still providing a clean and familiar
operational experience.

`initd` supports two primary modes of operation:

- **Service-manager mode**  
  Run as a daemon managing services, without PID 1 responsibilities.

- **Init mode (PID 1)**  
  Run as the system init process, performing essential system initialization
  and full lifecycle management.

---

## Why initd

Systemd is powerful, but it is not always suitable.

In many real-world Linux environments, systemd cannot run reliably or at all:

- Containers (Docker, unshare, rootless environments)
- Android chroot / proot
- Embedded Linux and IoT devices
- Minimal systems with limited memory or storage
- Systems that prefer a simpler, more transparent init model

In these environments, users are often forced to:

- Write ad-hoc startup scripts
- Manually launch and supervise daemons
- Give up `systemctl` entirely
- Reimplement basic init behavior

`initd` solves this by keeping the **systemd service model and operator
experience**, while removing systemd’s heavy runtime stack.

You write normal `*.service` files.  
You use familiar `systemctl` commands.  
Services behave as expected.

---

## Key Capabilities

### Full init system (PID 1)

When running as PID 1, `initd` provides core init functionality:

- Acts as the system init process
- Reaps zombie processes
- Handles init-specific signal semantics
- Remounts the root filesystem read-write
- Applies system hostname
- Spawns console login (getty or fallback shell)
- Starts all enabled services automatically
- Supports clean reboot, poweroff, and halt

This makes `initd` suitable as a real init system, not just a supervisor.

---

### Systemd-compatible service management

- Runs **unmodified systemd `*.service` files**
- Familiar `systemctl` interface
- Supports:
  - start / stop / restart
  - enable / disable
  - status / is-active / is-enabled
- Tracks:
  - service state
  - main PID
  - start/stop timestamps
  - last error
  - service logs

The goal is operational familiarity without systemd internals.

---

### System control commands

`initd` supports system-level commands via `systemctl`:

- `systemctl reboot`
- `systemctl poweroff`
- `systemctl halt`

Shutdown is performed in a controlled manner:

- New logins are disabled
- Services are stopped gracefully
- Filesystems are synchronized
- Final system action is executed

---

### Minimal and dependency-free

`initd` intentionally avoids complex subsystems:

- No systemd
- No D-Bus
- No cgroups
- No journald

Communication uses a simple Unix domain socket.
Access control relies on filesystem permissions.

The design favors clarity, auditability, and predictability.

---

## Architecture

`initd` follows a simple and explicit architecture:

- `initd` manages system and service lifecycle
- `systemctl` is a thin client communicating over a Unix socket
- No background buses or hidden dependencies
- Clear separation of responsibilities

Unlike systemd, `initd` does not attempt to be a monolithic userspace platform.

---

## Usage

### initd

```sh
Usage: initd [OPTIONS...]

Default behavior:
  Running initd with NO arguments defaults to init/supervisor mode (equivalent to --init).

Options:
  --init               Run as init/supervisor (autostart enabled units).
  --socket[=PATH]      Run as a pure daemon/service manager without init/PID1 behaviors.
                       If PATH omitted, defaults to /run/initd.sock.
  -h, --help           Show this help.
  -V, --version        Show version.
```

### Modes of operation

#### Init mode (recommended)

- Runs as a full init system
- Automatically starts all enabled services
- Performs essential system initialization
- Suitable for:
  - Containers
  - Chroot / proot
  - Embedded Linux
  - Minimal systems

```
initd (or initd --init)
```

#### Service-manager-only mode

- Runs without PID 1 responsibilities
- Manages services only
- Useful when integrating into existing systems

```
initd --socket
```

### systemctl

```
systemctl [OPTIONS...] COMMAND [UNIT...]

Query or send control commands to the initd system manager.

Options:
  --socket=PATH        Path to initd control socket
  -h, --help           Show this help
  -V, --version        Show version

Unit Commands:
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
System Commands:
  reboot               Reboot the system
  poweroff             Power off the system
  halt                 Halt the system
```

The interface is intentionally close to systemd’s `systemctl`.

## Examples

### Starting nginx

```
sudo systemctl start nginx
sudo systemctl status nginx
● nginx.service - A high performance web server and a reverse proxy server
   Loaded: loaded (nginx.service; enabled)
   Active: active (running) since Mon, 26 Jan 2026 22:57:51 PST
 Main PID: 12717
```

------

### SSH service

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

------

### Listing units

```
systemctl list-units
UNIT                                    LOAD    ACTIVE    DESCRIPTION
nginx.service                           loaded  active    A high performance web server
ssh.service                             loaded  active    OpenBSD Secure Shell server
systemd-journald.service                loaded  inactive  Journal Service
```

------

## When to Use initd

`initd` is recommended when:

- systemd cannot run or is restricted
- systemd is too heavy for the environment
- You want systemd-style service management without systemd
- You need a real init system with minimal overhead

It is especially useful for:

- Docker containers
- Android chroot / proot
- Embedded Linux and IoT devices
- Minimal or custom Linux systems

------

## Important Notes

- Do **not** run `initd --init` alongside systemd
   Running two init systems simultaneously will cause conflicts.
- On systems where systemd is already active, only one init system
   should manage services at a time.

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

This avoids conflicts with system-provided systemd binaries on
 Debian and Ubuntu systems.

After installation, verify that `systemctl` refers to the initd version.

------

## Recommended Startup

For full functionality, run initd in init mode:

```
/usr/local/bin/initd
```

All enabled services will start automatically.

------

## License

MIT License
