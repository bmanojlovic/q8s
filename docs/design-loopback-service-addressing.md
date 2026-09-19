# Design proposal: per-service loopback addressing (`10.a.b.c ⇄ 127.a.b.c`)

Status: **proposal / not implemented.** Captures the design discussed for
tic-3124 (Traefik ingress being inert + Service `clusterIP` absent). Written
to record the idea and its boundaries before committing to code.

## The problem this replaces

Today q8s addresses Deployment-replica backends with a **shared-loopback,
allocated-port** scheme (`internal/store/store.go`, `AllocateReplicaPort`):

- Every replica publishes on `127.0.0.1:<allocated-port>`, where the port is
  drawn from a finite pool (`ReplicaPortMin..ReplicaPortMax` = 20000–32767).
- Traefik reaches each backend at `http://127.0.0.1:<allocated-port>`.
- `spec.clusterIP` is not allocated at all — after tic-e014 it is reported as
  `None` (a DNS-alias service, no virtual IP).

Consequences of sharing one loopback IP (`127.0.0.1`) across everything:

- The **port** is the only thing distinguishing backends, so it is scarce
  (~12k), must be allocated, persisted across restarts, and freed on
  scale-down. That is real bookkeeping (`ReplicaPorts` map in the store).
- The Traefik backend URL is a meaningless `127.0.0.1:20077` — it does not
  match the service's real port and is not self-documenting.
- A Service has no address of its own; it is only a DNS alias + port map.

## The key insight: a socket bind is `(IP, port)`, not just a port

The whole `127.0.0.0/8` is loopback on Linux, and **every address in it is
bindable rootless** (no privilege, no LAN exposure). A published port binds a
`(host-IP, host-port)` tuple, so:

    127.11.1.1:5432   and   127.11.1.2:5432

are **completely distinct sockets** — same port, different loopback IP, no
collision. The port is not the scarce resource; the `(IP, port)` pair is, and
`127/8` gives ~16 million IPs each with a full 65 535-port space.

This means we can stop allocating scarce shared-loopback ports and instead
give **each service (and each replica) its own loopback IP on the service's
real port**:

- `pgvector`     → advertise `clusterIP 10.11.1.1`, bind `127.11.1.1:5432`
- `mozak-brain`  → advertise `clusterIP 10.11.1.2`, bind `127.11.1.2:5432`

Both live on `:5432` simultaneously, zero conflict.

## The mapping: `10.a.b.c ⇄ 127.a.b.c` (identity on the host part)

Advertise a real-looking ClusterIP from a documented range (`10.a.b.c`) and
map it 1:1 onto host loopback by swapping only the first octet:
`10.a.b.c → 127.a.b.c`, **port preserved**. So `10.11.1.1:5432` corresponds to
`127.11.1.1:5432` on the host.

The mapping is a pure function of the ClusterIP — so the ClusterIP itself is
the allocation key. There is **no allocator state to persist**: derive the
loopback bind address from the service's ClusterIP every time. (A ClusterIP
allocator is still needed to hand out unique `10.a.b.c` values and free them on
delete, but that replaces the port allocator rather than adding to it, and the
address space is effectively unbounded.)

Podman publish syntax is `-p HOSTIP:HOSTPORT:CONTAINERPORT`, i.e. quadlet
`PublishPort=127.11.1.1:5432:5432`. (Note: `-p` takes exactly one host IP;
`10.11.1.1` is not part of the publish spec — it is only what q8s *advertises*
as `spec.clusterIP` and what it translates from.)

## What this wins

- **Retire `AllocateReplicaPort` and the 20000–32767 pool.** No port
  allocation, no `ReplicaPorts` persistence, no free-on-scale-down bookkeeping.
  The IP is the unique key; the port is just the container's real port.
- **Readable, self-documenting backends.** Traefik server URLs become
  `http://127.11.1.1:5432` — the service's real port, per-service IP.
- **`spec.clusterIP` becomes a real, stable, non-fabricated value** derived
  from the service, satisfying `kubectl` and k8s-aware tooling honestly
  (supersedes the tic-e014 `None` stopgap for non-headless services).
- **Replica fan-out is cleaner.** Each replica gets its own loopback IP (a
  sub-range per service), all on the real container port — the Endpoints /
  kube-proxy fan-out becomes "N IPs, one per pod, same port" instead of "N
  allocated ports."
- **Rootless, no privilege, no extra network layer** for the host-originated
  path.
- **Fast path (rootless), not just clean.** In **rootless** mode a published
  loopback port (`127.a.b.c:port`) is a kernel-local loopback socket, avoiding
  pasta's userspace per-packet round-trip — so for the host→backend direction
  (Traefik, host tooling) the `127.x` path is effectively free where routing
  through pasta is a real (if usually small) cost. This performance argument is
  **rootless-specific**: in **rootful** mode container networking is real
  veth + bridge in the host netns (kernel-forwarded, comparable speed), so
  there is no pasta shuttle to avoid and the loopback scheme buys
  cleanliness/addressing, not speed. Net: the loopback scheme is primarily
  *the rootless answer* (perf + privilege-free + clean all align); rootful
  gets the cleanliness but doesn't need it for speed, and has a better native
  option anyway (nft DNAT, see scope 2).

## The boundary: host-side vs pod-netns (where pasta re-enters)

This elegance is **entirely on the host side**. Two directions must be kept
distinct:

- **Host → backend** (Traefik ingress backends, host tooling, anything q8s
  itself drives): a plain published port on `127.a.b.c:port`. **No pasta hop,
  no root.** This is the same forwarding podman already does for
  `127.0.0.1:<port>` today — the proposal only makes it per-IP and
  port-preserving. This path is fully solved by the scheme above.

- **Pod → pod via ClusterIP** (`10.a.b.c:port` dialed from *inside* a
  container netns): **not** solved by `127/8` alone. A container's `127.a.b.c`
  is that container's *own* loopback namespace, not the host's — host and
  container loopback share the same `127/8` numbers but are different
  namespaces. For a pod to reach the host's `127.a.b.c`, something must
  translate `10.a.b.c → host 127.a.b.c`:
    - rootless: pasta `--map-guest-addr` (so pasta re-enters here — it is not
      avoided for this direction), or the existing aardvark DNS alias for
      name-based resolution;
    - rootful: an nft/iptables DNAT rule (a real "fake kube-proxy" controller
      reacting to endpoints).

So `127/8` is the right primitive for host-originated traffic, but assuming the
same address works transparently from inside a container is the trap: that hop
still needs a translation, which is the heavier networking layer.

## Traefik as the unprivileged kube-proxy (both modes)

The intended model for load-balanced Services: **Traefik is the fake
kube-proxy, in both rootless and rootful, doing round-robin across replicas —
with no root and no iptables/nft at all.** This supersedes any nft-DNAT
framing below.

The reasoning: q8s already runs Traefik for Ingress, it already load-balances
across a servers list, and it already consumes a file-provider config q8s
writes. A ClusterIP Service therefore does not need a kernel VIP — it needs a
Traefik listener per service that round-robins to the backing replicas.

**But L7 and L4 are different beasts, and this is where the idea splits — do
not treat them as one capability:**

- **HTTP (L7) services — Traefik's home turf.** Host/path routing, TLS
  termination, protocol-aware retries and health, real HTTP load balancing,
  and many services multiplexable on one entrypoint via `Host()` rules. This
  is genuinely strong and is essentially what Ingress already does.
- **Plaintext TCP (L4) services — the hard case (and the live one: pgvector /
  Postgres).** Traefik has TCP routers, but they route by **SNI**, which only
  exists for TLS. A plaintext protocol (Postgres, Redis, plain MySQL) has no
  SNI, so services **cannot** be multiplexed on a shared port — each needs its
  **own dedicated entrypoint** (`127.a.b.c:port`). And a TCP router is a dumb
  byte pipe: connection-level round-robin only, none of the L7 niceties, no
  protocol health. For a single-backend service this is barely more than
  binding the pod's port directly (scope 1).
- **UDP services — weakest.** No connection concept, crude session handling,
  weak health-checking; may not be worth claiming Traefik covers at all.

Note the irony: a *real* kube-proxy is an **L4** device (iptables/IPVS DNAT at
the connection level, protocol-blind, conntrack-tracked). Traefik shines at
L7 — the layer kube-proxy doesn't even operate at — and is weakest exactly at
the raw-L4 job kube-proxy is built for. So "Traefik as kube-proxy" is *true
for HTTP, qualified for TCP, shaky for UDP*.

Concretely, where it does apply:

- Each Service (port) gets its own listener at `127.a.b.c:port` — that address
  is now **Traefik's entrypoint**, not a pod's published port. q8s writes a
  Traefik `tcp`/`udp` (or `http`) router + a servers list of the backing
  replicas into the file-provider config.
- Traefik round-robins across the replicas — so an **HTTP** ClusterIP becomes
  a real load-balanced VIP-equivalent, removing the current single-backing-pod
  limitation. For **plaintext TCP** it is connection-level RR only (dumb byte
  pipe, one entrypoint per service); for **UDP** it is shaky. So the
  capability gain is real but **L7-weighted** — strongest for HTTP, marginal
  for raw L4.
- **One mechanism for both modes.** No "rootful does nft, rootless does
  map-guest-addr" fork for the proxy itself — Traefik behaves identically.
  This is the clean answer to the rootful/rootless "dual thing".
- **No iptables/nft, no root**, in either mode — the whole point.

Consequence: Traefik shifts from "optional, admin-supplied, Ingress-only" to
**a component q8s requires and configures as its service proxy** (bundled/
installed in rootful; documented-and-configured in rootless). That also
answers tic-3124's "no Traefik installed" half — Traefik stops being an
afterthought and becomes part of the model.

Edges to nail down before this is fully baked (not objections — real work):

1. **Per-service L4 entrypoints.** Traefik entrypoints are normally *static*
   config; adding one per service dynamically means either a Traefik reload
   of static config, or leaning on its dynamic TCP-router support. Raw TCP
   with no SNI (e.g. Postgres) can't be multiplexed on one port by hostname,
   so each service-port genuinely needs its own `127.a.b.c:port` entrypoint —
   confirm Traefik can gain L4 entrypoints without a full restart, or accept a
   reload on service create/delete.
2. **Pod → ClusterIP reachability.** Traefik binding `127.a.b.c` on the *host*
   serves host-originated and Ingress traffic for free, but a pod dialing
   `10.a.b.c` from inside its netns still needs the `10→127` guest-addr map
   (rootless) to reach the host listener. Same boundary as the loopback
   scheme — host-side is free, pod-side needs the one translation hop. (DNS
   via aardvark can also point a service name at the reachable address.)

## Two independently shippable scopes

1. **Host-facing service addresses** — advertise `10.a.b.c`, bind
   `127.a.b.c:realport`, retire the replica-port allocator, point Traefik and
   host tooling at the per-service loopback IP. Rootless, no pasta hop, no
   root. Low-risk, high-cleanup; achievable with plain `PublishPort` + a
   ClusterIP allocator. **This is the recommended first step.**

2. **Load-balanced ClusterIP via Traefik** — Traefik as the userspace
   kube-proxy (above): a per-service `127.a.b.c:port` entrypoint round-robining
   to the replicas, no root/nft in either mode. The `127.x` binding from
   scope 1 becomes Traefik's listener address. **Split by protocol, not
   uniform:** strong for HTTP (L7 — routing, TLS, health, multiplexable),
   qualified for plaintext TCP (L4 — dedicated entrypoint per service, dumb
   byte-pipe RR, the pgvector/Postgres case), shaky for UDP. Pod-originated
   ClusterIP still needs the `10→127` guest-addr map (rootless);
   host/Ingress-originated is free. This stays unprivileged and mode-agnostic
   — it does not require the nft-DNAT / host-netns machinery a real (L4)
   kube-proxy uses, at the cost of being an L7 proxy doing an L4 job for raw
   TCP/UDP.

## Relationship to existing behavior / migration notes

- Supersedes the `AllocateReplicaPort` mechanism (`internal/store/store.go`)
  and the `ReplicaPorts` persisted map for scope 1.
- Supersedes tic-e014's `clusterIP: None` default for non-headless services
  (headless `None` stays a legal explicit choice).
- NodePort is unaffected — it stays a real `0.0.0.0:nodePort` LAN listener
  (`applyNodePorts`, `withNodePortPublish`); this proposal is about the
  internal/ingress backend addressing, not LAN exposure.
- Traefik-in-rootful-install (the other half of tic-3124) is a separate
  decision from this addressing scheme and can be decided independently.
