<img width="300" height="300" alt="ramekin-logo" src="https://github.com/user-attachments/assets/e01a5f6e-1994-4a81-bebd-443445d43709" />

# Ramekin
IoT development, deployment, and update platform

> **ramekin** (*noun*, /ˈræm.ɪ.kɪn/): a small dish for baking and serving a single portion.
>
> That's the idea behind the name: each device is a small container that holds one app and serves it, and nothing else.

Ramekin turns a small board (a Raspberry Pi, a mini PC, or similar) into an appliance that runs one app in a container. The app can be written in any language, and it can be one you already have or one you write from scratch. You build the app on your computer, push it to the device to try it, then publish it to a whole fleet. Every device updates itself safely.

- **Safe updates.** A new version must prove it's healthy before it replaces the old one. If it doesn't, the device rolls back on its own.
- **Secure by default.** Every image is signed and scanned for known vulnerabilities, and fleet releases are encrypted.
- **Tiny devices.** Built on [gokrazy](https://gokrazy.org): no shell or package manager in the device's OS, just your app and one small agent. Your app's container holds only what you put in it, which can be just the app, with no shell either.
- **Simple tooling.** Describe your app in an [Apptainer](https://apptainer.org) definition file (a Dockerfile converts in minutes), or build it interactively in a shell. Apptainer is the only container tool you need.

For how Ramekin compares with Mender, balena, RAUC, Talos and similar tools, see [ALTERNATIVES.md](ALTERNATIVES.md).

## Fleet dashboard

`ramekin --dashboard` opens a web page showing every device in the project's `.ramekin/devices`. It refreshes every 5 seconds.

```sh
ramekin --dashboard                         # http://127.0.0.1:8099/, opened in your browser
```

<img src="docs/dashboard-demo.svg" alt="Example of the Ramekin fleet dashboard: three device cards, garage-02 unhealthy because its app isn't running, porch-03 offline with its disk nearly full, and kitchen-01 healthy. Each card lists the running release and two earlier ones with Switch buttons, plus CPU, memory and disk meters." width="100%">

*Example with sample data: a three-device fleet. Devices with problems are listed first.*

For each device it shows:

- **Health:** *Healthy*, *Warning* (update checks failing, or the app has restarted), *Updating* (a release is being health-checked), *Unhealthy* (the app isn't running), *Offline*, or *No release*.
- **Releases:** what's running and the earlier releases the device keeps, each with where it came from (a channel or a push) and when it was installed.
- **Resource use:** CPU, memory, and disk on the data partition (`/perm`). Each meter turns amber at 80% and red at 95%.
- **Details:** uptime, last update check, app restarts and last exit, and rejected releases.

**Switching versions.** Each device keeps its three most recent releases that passed their health check there: the one it's running and two earlier ones. The dashboard lists all three, and **Switch** moves the device to either earlier one, after a confirmation. From the terminal, `ramekin rollback <device>` switches to the previous release, and `ramekin rollback <device> <version>` to any kept one. The device runs the chosen release on trial with its usual health check. If that passes, it becomes current and the release it replaced goes to the top of the list, so you can switch back. If it fails, the device keeps the current release and drops the failed one from the list. A device that follows a channel stays on the version you chose until the channel publishes something newer. To roll back the whole fleet, use `ramekin promote` instead.

The dashboard is read-only apart from switching versions. It has no remote shell or other device controls.

**Security.** There's no login, so the dashboard only listens on a loopback address (`--listen 127.0.0.1:PORT`). To use it from another machine, forward the port with `ssh -L 8099:127.0.0.1:8099 workstation`. Device tokens stay in the `ramekin` process and never reach the browser. The page accepts requests only from itself: it checks the `Host` header and requires a key that changes every time the dashboard starts.

The dashboard reaches devices over the local network, the same way `ramekin status` does. If a device's address changes and its MAC is known, the dashboard searches for it at most every 2 minutes. Devices need a ramekind that reports health and resource use. For an older one, the dashboard shows a warning and you should run `ramekin device update <device>`. Devices you can't reach directly can still report to a webhook with `--status-url`, and the report includes the same health and resource data.

## Quick start: Hello World on a NanoPi R76S

Take a [NanoPi R76S](https://www.friendlyelec.com/index.php?route=product/product&product_id=303) out of the box, flash it, run your first app on it, then change the app. It takes about 10 minutes.

**You need:**
- the R76S, connected to your network, with a screen on its HDMI port;
- a microSD card of 2 GB or more;
- on your Linux computer: [Go](https://go.dev/dl/) and [Apptainer](https://apptainer.org/docs/admin/main/installation.html).

**1. Install the CLI.**

```sh
go install github.com/ascension-association/ramekin/cmd/ramekin@latest
```

**2. Flash the firmware.** Download the [official R76S firmware](https://github.com/ascension-association/firmware.r76s) and write it to the card. Replace `/dev/sdX` with your card; this erases it.

```sh
curl -LO https://github.com/ascension-association/firmware.r76s/releases/latest/download/r76s.img.gz
gunzip -c r76s.img.gz | sudo dd of=/dev/sdX bs=4M conv=fsync status=progress
```

Put the card in the R76S and power it on. After about a minute, its screen shows a pairing code. Leave it running while you create the project.

**3. Create a project on your Linux computer.** Run this and the remaining steps there, not on the R76S: the device only runs what you push to it. `ramekin init` starts you with a placeholder app, `app.sh`: every 10 seconds it prints "Hello" in large letters with [figlet](http://www.figlet.org/), then the device's name. Replace it with any app, in any language, new or existing: the examples under [Learn more](#learn-more) serve a web page with nginx, run a Go web app with a database, and build an app interactively. `example.def` packages the app into a container: it starts from [Wolfi](https://github.com/wolfi-dev), adds the `figlet` package, and comes to about 6 MiB with no known vulnerabilities.

```sh
ramekin init example && cd example
ramekin keys generate
```

**4. Claim the device.** Its screen now shows **Pairing code 123456**. This command finds the R76S on your network and asks for that code: type the 6 digits. Or pass it directly: `ramekin device claim example --code 123456`. The code proves you have the device in hand, and once claimed, the device trusts only your project's key. The device picks a new code each time it starts, so use the one on the screen now.

```sh
ramekin device claim example
```

**5. Build the app and push it to the device.**

```sh
ramekin build --no-scan
ramekin push example --no-scan
ramekin logs example -f
```

```
 _   _      _ _
| | | | ___| | | ___
| |_| |/ _ \ | |/ _ \
|  _  |  __/ | | (_) |
|_| |_|\___|_|_|\___/

from example
```

**6. Change it and push again.**

```sh
sed -i 's/figlet "Hello"/figlet "Hello World"/' app.sh
ramekin build --no-scan && ramekin push example --no-scan
ramekin logs example -f
```

```
 _   _      _ _        __        __         _     _
| | | | ___| | | ___   \ \      / /__  _ __| | __| |
| |_| |/ _ \ | |/ _ \   \ \ /\ / / _ \| '__| |/ _` |
|  _  |  __/ | | (_) |   \ V  V / (_) | |  | | (_| |
|_| |_|\___|_|_|\___/     \_/\_/ \___/|_|  |_|\__,_|

from example
```

This project is in [ramekin-app-example](https://github.com/ascension-association/ramekin-app-example).

The device keeps its firmware up to date from the official firmware's channel. A firmware update restarts the device more than once, so expect your app to be briefly unavailable a few times while it installs. A new app version runs only after it starts cleanly; otherwise the device goes back to the previous one.

**7. Optional: run without the SD card.** If your R76S has eMMC storage, install onto it. The device keeps its identity and app, then powers off; remove the card and power it on.

```sh
ramekin device install example --to emmc      # add --overwrite to erase an OS already on the eMMC
```

**Next:**
- **Scan for vulnerabilities:** install [`syft`](https://github.com/anchore/syft) and [`grype`](https://github.com/anchore/grype), then drop `--no-scan`.
- **Ship to a fleet:** publish releases that every device installs itself (see [Ship to a fleet of devices](https://github.com/ascension-association/ramekin/wiki/Ship-to-a-fleet-of-devices)).
- **Watch devices:** `ramekin --dashboard` shows every device.
- **Other boards and techniques:** see the examples under Learn more.

## Learn more

Guides and reference live in the [wiki](https://github.com/ascension-association/ramekin/wiki). You don't need any of them to get started.

**Examples**
- [Web Page Example](https://github.com/ascension-association/ramekin/wiki/Web-Page-Example)
- [Advanced Example](https://github.com/ascension-association/ramekin/wiki/Advanced-Example)
- [Persistent Overlay Example](https://github.com/ascension-association/ramekin/wiki/Persistent-Overlay-Example)

**How it works**
- [How Ramekin works](https://github.com/ascension-association/ramekin/wiki/How-Ramekin-works)
- [How updates and automatic rollback work](https://github.com/ascension-association/ramekin/wiki/How-updates-and-automatic-rollback-work)
- [Security model](https://github.com/ascension-association/ramekin/wiki/Security-model)
- [Limitations and notes](https://github.com/ascension-association/ramekin/wiki/Limitations-and-notes)

**Shipping and devices**
- [Ship to a fleet of devices](https://github.com/ascension-association/ramekin/wiki/Ship-to-a-fleet-of-devices)
- [Official firmware for your board](https://github.com/ascension-association/ramekin/wiki/Official-firmware-for-your-board)
- [Fleet image, claiming and firmware updates](https://github.com/ascension-association/ramekin/wiki/Fleet-image-claiming-and-firmware-updates)
- [Choosing a health check](https://github.com/ascension-association/ramekin/wiki/Choosing-a-health-check)
- [Changing device settings](https://github.com/ascension-association/ramekin/wiki/Changing-device-settings)
- [Ports, the gokrazy dashboard, and finding your device](https://github.com/ascension-association/ramekin/wiki/Ports-the-gokrazy-dashboard-and-finding-your-device)
- [Already have a gokrazy device?](https://github.com/ascension-association/ramekin/wiki/Already-have-a-gokrazy-device)

**Building images**
- [Building with a definition file or a persistent overlay](https://github.com/ascension-association/ramekin/wiki/Building-with-a-definition-file-or-a-persistent-overlay)
- [Converting a Dockerfile or container image](https://github.com/ascension-association/ramekin/wiki/Converting-a-Dockerfile-or-container-image)
- [Vulnerability scanning and alerts](https://github.com/ascension-association/ramekin/wiki/Vulnerability-scanning-and-alerts)

**Project**
- [Contributing](https://github.com/ascension-association/ramekin/wiki/Contributing)
