# Hoserva — Virtual Machines

---

## 1. Decision

**Build VM management, on libvirt/KVM, deliberately narrow. Not a Proxmox clone.**

### Why build it

VMs are a capability Unraid users actively rely on — a Windows gaming VM with GPU passthrough, a pfSense router VM, an isolated VM for something that doesn't trust containerisation. Leaving this out permanently would mean those users simply can't migrate, which cuts against doc 00's target user and the entire migration story (doc 05).

### Why libvirt/KVM

**This is the same orchestrate-don't-reimplement principle as D1, applied to virtualization.** libvirt/QEMU/KVM is the engine Unraid's own VM Manager runs on top of. Two consequences follow directly:

1. **Nothing here is novel systems work.** Hoserva generates libvirt domain XML and drives `libvirtd` — exactly the same relationship it has to `mergerfs`/`snapraid` (D1) and to the Docker Engine (doc 04). No hypervisor code, no device emulation, no live-migration protocol implementation.
2. **Migration is structurally different from the container case.** Docker Engine has no concept of an Unraid XML template — hence the converter in doc 04 §5. libvirt domain XML *is* what Unraid's VM Manager already produces and consumes. An exported Unraid VM's XML is not a foreign format to translate; it is close to directly loadable. Doc 05 gains a second, easier import path this way. See §5.

### Why narrow

A full hypervisor platform (live migration between hosts, clustering, a VM image marketplace, nested hypervisors) is its own product, and Hoserva is explicitly single-node (doc 00 §4's "Clustering, multi-node — wrong audience" applies here without change). The dividing line mirrors D6's for containers: **Hoserva owns the path from "I want a Windows VM with my GPU" to "the VM is running and I can see its screen."** Everything a data-centre hypervisor manager does beyond that — host clustering, storage-tiered VM migration between physical hosts, a curated VM image gallery — is out of scope.

### In scope (Phase 3.5, doc 07 §1)

- VM lifecycle: create, start, stop (graceful ACPI and force), restart, delete, clone
- CPU/RAM allocation, CPU pinning
- Virtual disks (qcow2) on the pool or cache, ISO-based installation, an existing qcow2/raw image as a starting disk
- Bridged networking, giving a VM a real LAN-visible address (§4)
- **PCI and USB device passthrough**, including GPU passthrough, gated behind an explicit compatibility check (§3)
- Browser-based console (VNC/SPICE via an embedded viewer) — no separate protocol exposed
- Unraid VM (libvirt domain XML) import as part of migration tooling (§5, doc 05)
- Autostart on boot, per VM

### Out of scope (v1 of this feature)

| Excluded | Reason |
|---|---|
| Live migration between hosts, clustering | Single-node product (doc 00 §4) |
| A curated VM/OS image gallery | User supplies their own ISO or disk image; no marketplace, no image trust problem to solve |
| Mediated/vGPU device slicing (SR-IOV) | Full PCI passthrough only for the first cut; vGPU needs vendor-specific driver stacks Hoserva can't validate generically |
| Nested virtualization support for guest VMs | Guests run whatever they run; Hoserva doesn't manage what's inside them |

---

## 2. Where this sits architecturally

Same pattern as every other subsystem in doc 01 §4: SQLite is the source of truth (D4), libvirt domain XML is a **generated artifact**, never hand-edited by Hoserva after creation. A new package, `internal/vm/`, sits behind a provider-style interface:

```go
// package vm
type Engine interface {
    Define(ctx context.Context, spec DomainSpec) error   // writes domain XML, defines with libvirt
    Start(ctx context.Context, name string) error
    Shutdown(ctx context.Context, name string, force bool) error
    Delete(ctx context.Context, name string, keepDisks bool) error
    List(ctx context.Context) ([]Domain, error)
    IOMMUGroups(ctx context.Context) ([]IOMMUGroup, error)  // §3
    ConsoleSocket(ctx context.Context, name string) (string, error)
}
```

Connects to `libvirtd` over its local Unix socket via `go-libvirt` (pure Go, no cgo — consistent with the SQLite driver choice, Q6, and D3's single-static-binary goal; `libvirt-go`'s cgo bindings are not used). A `FakeEngine` gives L1 the same scriptable-simulator treatment as `disk.Provider` and `parity.Engine` (doc 06 §2): fake IOMMU groups, fake domain state transitions, a fake console socket — so VM lifecycle logic is testable without KVM at all.

### Storage placement

Virtual disks live under a dedicated share, `/mnt/user/domains/<vm-name>/`, the same path Unraid itself uses — chosen for the same reason as D10: a migrated VM's domain XML disk paths need no rewriting. Cache-then-move and array-only placement (Q12) both apply, but **a VM's vdisk is never moved by the mover while the VM is running** — the mover skips any path a running domain holds open, the same "don't move what's open" discipline doc 09 already requires elsewhere. Moving a VM's storage tier is a stop-VM, relocate, restart operation, not a live one.

### Job system

A new **VM** job class (doc 01 §4): start, stop, create, delete, snapshot, clone, migration-import. Mutually exclusive with other VM jobs on the *same* VM, independent of the Parity/Array-write/Topology/Service classes — ordinary VM lifecycle doesn't touch pool topology. The one exception: relocating a VM's disk between cache and array is an **Array-write**-class job like any other relocation (doc 09), and requires the VM to already be stopped.

### CLI (parity with the UI, D5)

```
hoserva vm list
hoserva vm create --name winvm --vcpus 4 --mem 8G --disk 100G --iso win11.iso --network vmbr0
hoserva vm start|stop|restart|rm <name>
hoserva vm console <name>                          # opens the browser console (§4)
hoserva vm passthrough list                        # IOMMU groups and current assignments (§3)
hoserva vm passthrough check                        # pre-flight ACS/group isolation report (§3)
hoserva vm passthrough assign <name> --pci 01:00.0
hoserva migrate vm-scan                             # Unraid domain XML inventory from the adopted pool (§5, doc 05)
hoserva migrate vm-import <domain-name>
```

---

## 3. PCI and USB passthrough

The single hardest part of this feature, and the one most likely to produce a broken host if implemented carelessly.

### IOMMU groups and VFIO binding

- IOMMU groups are **enumerated and shown read-only** (`hoserva vm passthrough list`) — this needs no device changes and is safe to run any time.
- A device is only unbound from its host driver and bound to `vfio-pci` when the user **explicitly assigns it to a VM**. Binding is applied at boot — an IOMMU kernel parameter where the platform needs one (`intel_iommu=on`) and a generated `vfio-pci` device list loaded before the host driver claims the device, both regenerated from SQLite like every other managed file (D4) — and **requires a reboot**. Unraid binds devices the same way, statically at boot, not live, clearly flagged as such in the UI. There is no live, in-session device unbind — that class of operation is exactly the one most likely to wedge a host that depends on the device it just lost (its own boot storage controller, its only NIC).
- **A device the host itself needs is never offered for passthrough**: the boot device's controller, and (unless a headless iGPU or second GPU is present) the console GPU, are excluded from the assignable list by construction, not by a warning the user can click past.
- `hoserva vm passthrough check` is a pre-flight report: IOMMU group membership, ACS (Access Control Services) isolation per group, and a plain-language verdict — "this device shares a group with your boot controller, passthrough would also detach that" — run **before** the user commits to a reboot-requiring change, not discovered after.

### GPU passthrough specifically

Offered, but explicitly flagged **best-effort**: single-GPU passthrough (the common budget case, where the host has only one GPU and must release it entirely to the VM and reacquire it after) is a well-known source of platform-specific breakage across the whole KVM ecosystem, not something Hoserva's abstraction can paper over. The passthrough-check report calls this configuration out by name when detected, with a link to what it means and its risks, rather than presenting it as equivalent to a dual-GPU setup.

### Testing implication

Passthrough behaviour is IOMMU-topology- and BIOS-dependent in ways no VM or loop device can simulate. `internal/vm`'s domain-XML generation and job orchestration are L1/L2-testable against the fake engine; **actual passthrough — device binding, VFIO, GPU reacquisition — is `needs-hardware`, L4-only**, and results vary per motherboard. This is stated plainly in user-facing docs: passthrough compatibility is reported per-system, not guaranteed by the product.

---

## 4. Networking and console

### Networking

VMs default to a bridged interface, `vmbr0`, that Hoserva creates over the host's physical NIC — a VM gets a real LAN-visible address via DHCP, appearing as its own device on the network rather than NAT'd behind the host. This is the model Unraid uses and what homelab users expect (a VM running a router or a game server needs to *be* a network host, not be port-forwarded to). An isolated/NAT network is offered as the alternative for VMs that shouldn't be LAN-visible.

This is a separate network layer from container networks (doc 04, Q37): containers get `bridge`/`host`/an existing custom network, VMs get their own bridge interface over a physical NIC. Both share the same narrowing discipline — no general-purpose network-topology editor, just the handful of options that cover the real use cases.

### Console

libvirt's VNC/SPICE graphics device is **never exposed as a raw network port** — that would be a second, typically-unauthenticated protocol reachable on the LAN, directly against doc 01 §7's small-attack-surface posture. Instead the API proxies the console over the existing authenticated TCP/TLS connection (doc 01 §5) via a WebSocket, rendered by an embedded noVNC client in the web UI. Same session auth as everything else; no separate credential, no separate port.

---

## 5. Unraid VM migration

### The finding that changes the shape of this work

Doc 04 needed a converter because Docker has no concept of Unraid's XML template format. **VMs don't have that problem.** Unraid's VM Manager is libvirt underneath; its domain definitions (`qemu/<name>.xml`) are already libvirt domain XML — the same format Hoserva itself generates. This is closer to doc 05's XFS-array-adoption problem (mount and use what's already there) than to doc 04's format-translation problem.

### What still needs handling

A handful of fields diverge and need remapping rather than a wholesale rewrite:

| Field | Handling |
|---|---|
| Disk paths (`/mnt/user/domains/...`) | Unchanged in the common case — D10-style path compatibility means the domains share lives at the identical path on both platforms |
| Network bridge name (Unraid's `br0` vs. Hoserva's `vmbr0`) | Rewritten to the Hoserva bridge; flagged if the source used a non-default bridge Hoserva can't infer |
| PCI/USB passthrough device addresses | **Never trusted from the source XML as-is** — re-validated against Hoserva's own IOMMU group scan (§3) on the target hardware, since passthrough addresses are hardware-specific and the target box is not guaranteed to be the same machine |
| OVMF/BIOS firmware paths | Rewritten to the Debian package's firmware path if the referenced OVMF build differs |
| Unraid-specific XML comments/metadata | Dropped |

### Import sequence

1. **Where the definitions are.** They are not in the Flash Backup: Unraid keeps its libvirt configuration in a loopback image, by default `/mnt/user/system/libvirt/libvirt.img` — on the array. Once the data disks are adopted (doc 05 step 14), `hoserva migrate vm-scan` attaches that image **read-only** and reads every domain from it, reporting name, disk paths and sizes, and any passthrough devices referenced. A non-default image location is read from the Flash Backup's VM settings; an image that can't be found is reported, not guessed at (spike S11 confirms the layout per Unraid version, Q24)
2. **Vdisks are adopted, not copied.** They live in `/mnt/user/domains` on data disks that were adopted in place, so they are already at the path the domain XML names. They are covered by the same checksum verification as all other adopted data (doc 05 step 16). Copying happens only if the user later relocates a VM's disk (Q51), and then with the usual relocation guarantees (Q14)
3. The rewritten domain XML is shown side by side with the source before import runs, mirroring doc 04 §5's "generated Compose is always reviewable" rule
4. Nothing autostarts on import; the user reviews and starts each VM explicitly

---

## 6. Security

Extends doc 01 §7's threat model rather than introducing a new one:

- **`libvirtd` runs with elevated privilege** (device access, `vfio-pci` binding) — this doesn't change the trust boundary, since the API already runs as root for the same class of reason (disk partitioning). No new privilege escalation surface is introduced beyond what the daemon already has.
- **No VM image gallery** removes the "malicious pre-built VM image" risk that a curated-source approach would otherwise need to manage (the equivalent of the CA feed moderation problem in doc 04 §4, sidestepped entirely rather than solved).
- **Console access requires the same session auth as the rest of the UI** (§4) — no separate credential to leak.
- **Passthrough attach/detach is audit-logged** (doc 01 §7), with actor and timestamp, the same as any other destructive-adjacent configuration change.
- **A device the host depends on is structurally unassignable** (§3) — this is a safety property, not just a UX nicety, since a bricked boot controller on a box someone can't reach in person is a real support scenario.

---

## 7. Packaging and dependency sourcing (Q58)

Unlike Docker (D8: external prerequisite, not shipped by the `.deb`, because the container ecosystem moves faster than Debian stable and a bundled version would conflict with what users already have), `libvirtd` and QEMU/KVM are treated like mergerfs and SnapRAID (D1, Q7): **depend on Debian 13's own `libvirt-daemon-system` and `qemu-system-x86` packages directly.** These are stable, Debian-maintained, slow-moving packages with no equivalent version-churn problem, and pulling them in as a hard dependency keeps the "one `.deb` install, everything works" promise (D9) intact for this feature the way it already does for Samba and NFS.

---

## 8. Testing

Extends doc 06's four-layer pyramid:

- **L1** — `vm.Engine`'s fake: domain-XML generation is pure (state in, XML out) and gets the same golden-file treatment as SnapRAID/Samba config (doc 06 §2); job orchestration, IOMMU-group-based passthrough eligibility logic, and the Unraid domain-XML remapping (§5) are all testable against the fake with no KVM present.
- **L2/L3** — the loop-device lab has no VM concept; **L3's own test VM already runs on libvirt/QEMU** (doc 06 §4) to test *Hoserva itself*. Testing Hoserva's *own* VM-management feature end to end therefore means **nested virtualization**: KVM running inside the L3 test VM, for a domain that Hoserva-under-test creates. Whether hosted CI runners support nested KVM (`/dev/kvm` was already confirmed present for L3 itself in spike S9, doc 08) is a **new spike, tracked in doc 07 §1 as a Phase 3.5 prerequisite** — if nested KVM isn't available on hosted runners, this suite runs on the self-hosted nightly runner only, same posture as the rest of L3 (doc 06 §7).
- **L4** — PCI/USB/GPU passthrough is real-hardware-only (§3), `needs-hardware`, and joins the existing spindown/SMART/thermal set of things only the physical test box proves.

---

## Open questions

Tracked in doc 13 under **Virtual machines** (Q51–Q58): VM disk placement and format, the threshold guard's relationship to large VM disk images, VFIO binding mechanics, VM networking, Unraid migration mechanics, the VM job class, console access, and dependency sourcing.
