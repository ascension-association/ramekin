# Alternatives to Ramekin

Ramekin covers several layers that are often separate tools:

- **The device's OS:** [gokrazy](https://gokrazy.org) plus one agent, `ramekind`. It has no shell, SSH or package manager.
- **The app format:** one [Apptainer](https://apptainer.org) SIF per device.
- **An updater:** for both the app and the OS. A new version must pass a health check before the device keeps it, and the device rolls back on its own if it doesn't.
- **A release pipeline:** signed and encrypted releases, blocked when they have known vulnerabilities above your threshold, and stored in a GitHub repository instead of on a server you run.
- **Fleet tools:** device claiming, status reports and a local web dashboard.

This page describes tools that solve some of the same problems, so you can choose the right one, or combine one with Ramekin. Most of these projects change quickly, so check their own documentation for current features and pricing.

## At a glance

| | Device OS | App format | OS updates | App updates | Fleet server | Shell on the device |
|---|---|---|---|---|---|---|
| **Ramekin** | gokrazy | One Apptainer SIF | A/B, health-checked | Health-checked, automatic rollback | None (GitHub) | No (and optional in the app's container) |
| **Mender** | Your Yocto or Debian build | Files, packages, containers | A/B | Update modules | Mender server or hosted | Yes |
| **balena** | balenaOS | Docker containers | A/B | Container images, deltas | balenaCloud or openBalena | Yes |
| **Torizon** | Torizon OS | Docker containers | OSTree | Container images | Torizon Cloud | Yes |
| **Ubuntu Core** | Ubuntu Core | Snaps | Transactional | Snaps | Snap Store | Optional |
| **Talos + Omni** | Talos Linux | Kubernetes workloads | A/B | Kubernetes | Omni | No |
| **RAUC / SWUpdate** | Your own build | Your choice | A/B or single copy | Your choice | Optional (such as hawkBit) | Usually |

## Updaters

These install a new OS image on a device safely. You build the OS and, in most cases, run the server yourself.

- **[Mender](https://mender.io):** the most complete open-source OTA system: A/B root file system updates, app updates through "update modules", and a fleet server you host or Mender hosts. It integrates with Yocto and Debian and offers a remote terminal. Choose it if you already have your own Linux build and want a full fleet server. Ramekin, in comparison, needs no server and includes the OS.
- **[RAUC](https://rauc.io):** a mature updater for industrial devices. It installs signed bundles into A/B slots and integrates with common bootloaders. It's only the updater: you build the OS (usually with Yocto or Buildroot) and pick a server such as [Eclipse hawkBit](https://eclipse.dev/hawkbit/).
- **[SWUpdate](https://sbabic.github.io/swupdate/):** an updater framework similar to RAUC, using signed `.swu` images that can be A/B or single-copy, also often paired with hawkBit.
- **[Rugix](https://rugix.org):** an image builder (Rugix Bakery) and an A/B updater (Rugix Ctrl) from one project, focused on boards such as the Raspberry Pi. Of the updaters here, it's the closest to Ramekin's "everything in one place" approach. It's built on a conventional distribution, which includes a shell.
- **[fwup](https://github.com/fwup-home/fwup)** with **[Nerves](https://nerves-project.org)** and **[NervesHub](https://nerves-hub.org)**: fwup writes A/B firmware images, and NervesHub manages fleets. Nerves runs an Elixir app on a minimal Linux with no conventional userland, which is the same idea as gokrazy, for Elixir instead of Go.

## How the OS is built and updated

- **[gokrazy](https://gokrazy.org):** the base Ramekin builds on. It's a Linux appliance OS made of the kernel and Go programs, with A/B root partitions. On its own, gokrazy runs Go programs. Ramekin adds:
  - apps in any language, in containers;
  - signed and encrypted releases with vulnerability gating;
  - health checks with automatic rollback;
  - device identity and claiming;
  - official firmware images that anyone can flash.
- **[Yocto](https://www.yoctoproject.org)** and **[Buildroot](https://buildroot.org):** build systems for a custom embedded Linux. They're powerful, but you maintain a distribution. Most of the updaters above expect an OS built with one of them. Ramekin's OS layer is small enough that it needs neither. A board without a gokrazy kernel may still need its board support package, which is often provided for Yocto.
- **[OSTree](https://ostreedev.github.io/ostree/):** versioned file-system trees, like Git for the OS, with atomic and deduplicated upgrades. Fedora CoreOS and Silverblue, Torizon and others use it, and [bootc](https://containers.github.io/bootable/) builds on it to make OS images from containers. It downloads only what changed, while Ramekin writes whole partitions and whole SIFs. The trade-off is more moving parts on the device.
- **[ABRoot](https://github.com/Vanilla-OS/ABRoot)** and **[Vib](https://github.com/Vanilla-OS/Vib):** from Vanilla OS. ABRoot does A/B root updates built from OCI images, and Vib builds those images from recipes. They're designed for desktops, but the approach of A/B updates from container images is the same as Ramekin's.
- **[boot2container](https://gitlab.freedesktop.org/gfx-ci/boot2container):** a small initramfs that boots straight into containers named on the kernel command line, used for CI test machines. It shares Ramekin's idea of booting directly into the workload, with no update, signing or fleet features.
- **[Kairos](https://kairos.io):** an immutable Linux for edge Kubernetes. It does A/B upgrades from container images in a registry.

## Platforms: OS plus fleet management

- **[balena](https://www.balena.io)**, with balenaOS and balenaCloud:
  - **How it works:** a Yocto-based OS with a Docker-compatible engine, plus a cloud dashboard with a VPN to each device, a web terminal and delta updates.
  - **Where it's ahead:** it's the most polished of these platforms, supports many boards and runs several containers per device. [openBalena](https://github.com/balena-io/open-balena) is a self-hosted server with fewer features.
  - **Compared with Ramekin:** balena relies on its cloud, and its devices have a shell.
- **[Torizon](https://www.torizon.io)**, from Toradex: Torizon OS (Yocto with Docker and OSTree) plus Torizon Cloud, whose updates use [Uptane](https://uptane.org), a stronger security framework for updates than Ramekin's signed releases. It's centred on Toradex hardware.
- **[FoundriesFactory](https://foundries.io):** a Yocto-based Linux microPlatform with OSTree and Docker Compose apps, Uptane-secured updates and a hosted factory that builds your images.
- **[Ubuntu Core](https://ubuntu.com/core):** Ubuntu for devices, with transactional updates and every component packaged as a snap. It has a large ecosystem and long-term support from Canonical.
- **[Talos Linux](https://www.talos.dev)** with **[Sidero Omni](https://www.siderolabs.com/omni/):** Talos is the closest to Ramekin in design: it's immutable, managed only through an API, and has no shell or SSH. Omni manages Talos clusters. Both are designed for running Kubernetes, not single-app appliances.
- **[Pantavisor](https://pantavisor.io):** a container-based framework for embedded Linux, in which the whole system, the OS included, is a set of containers updated together.
- **[AWS IoT Greengrass](https://aws.amazon.com/greengrass/)** and **[Azure IoT Edge](https://learn.microsoft.com/azure/iot-edge/):** cloud vendors' edge runtimes, which deploy components or containers from their cloud onto your Linux device. They're a good fit if you're already committed to that cloud.

## Device management on your existing OS

- **[JFrog Connect](https://jfrog.com/connect/):** an agent you install on an existing Linux device. It provides updates of files, packages and containers with rollback scripts, a remote terminal, monitoring and a hosted dashboard. JFrog's [Artifactory](https://jfrog.com/artifactory/) and Xray store and scan the artifacts, the role GitHub Releases and the vulnerability scan play in Ramekin.
- **[Memfault](https://memfault.com):** device observability (crash reports, metrics and logs) with OTA updates, for microcontrollers, Android and Linux. Its monitoring goes much deeper than Ramekin's status reports and dashboard, and it could run alongside Ramekin.

## Choosing

**Ramekin fits best when:**
- each device runs one app, in any language;
- you want almost nothing on the device for an attacker to use;
- updates must check the app's health and roll back on their own;
- you don't want to run or pay for a fleet server.

**Another tool fits better when you need:**
- **more boards:** balena, Mender, Ubuntu Core and the Yocto-based stacks support far more;
- **several containers per device:** balena, Torizon or FoundriesFactory;
- **smaller downloads:** OSTree-based systems, balena and Mender offer delta updates;
- **stronger update security:** Torizon and FoundriesFactory use Uptane, and some boards also support secure boot;
- **remote access and a hosted fleet server:** balena, Mender, JFrog Connect or Omni;
- **deep monitoring:** Memfault;
- **to keep a distribution you can't replace:** JFrog Connect, Mender, RAUC or SWUpdate.
