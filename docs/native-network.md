# nyxd Native Network Plugin

## Why no external CNI binaries

The standard `containernetworking/plugins` approach requires external binaries
(`bridge`, `host-local`, `loopback`, `portmap`) installed at `/opt/cni/bin/`.
These binaries:

- Expand the attack surface (additional binaries on disk, each with their own CVE history)
- Add a deployment dependency (must be installed at the right version on every node)
- Introduce exec latency on every container start/stop
- Have a track record of CVEs


we can do better: implement all of this directly in Go using raw netlink syscalls.

## What the native plugin does

```
internal/network/native/
├── bridge.go    Core: bridge, veth, IPAM, netns, port mappings, nftables
└── manager.go   Drop-in Manager — same interface as the exec-based CNI manager
```

### On container start (Setup)

```
1. ensureBridge()       → creates nyxbr0 (10.88.0.1/16), brings it up
                           idempotent — sync.Once, safe to call from N goroutines
2. globalIPAM.allocate() → atomically assigns next free IP from 10.88.0.0/16
                            file-backed: /var/lib/nyxd/ipam/<hex-ip> → containerID
                            crash-safe: files survive daemon restart
3. createVethPair()     → RTM_NEWLINK with IFLA_INFO_KIND="veth"
                            creates vethXXXXh (host) + vethXXXXp (peer)
4. attachVethToBridge() → RTM_SETLINK IFLA_MASTER=nyxbr0_index, brings host end up
5. moveVethToNetNS()    → RTM_NEWLINK IFLA_NET_NS_FD=<netns_fd>
                            renames peer to "eth0" inside the netns
                            enters netns via setns(2), assigns IP + default route
6. setLoUp()            → enters netns, RTM_NEWLINK IFF_UP on lo
7. addPortMappings()    → nft DNAT rules in table ip nyxd-nat
```

### On container stop (Teardown)

```
1. removePortMappings() → delete nft rules for this containerID
2. deleteLink()         → RTM_DELLINK on host veth (peer disappears with netns)
3. globalIPAM.release() → os.Remove of the IPAM lease file
```

## IPAM design

The IPAM is intentionally simple and crash-safe:

```
/var/lib/nyxd/ipam/
├── 0a580002    → "web-container"      (10.88.0.2)
├── 0a580003    → "mqtt-broker"        (10.88.0.3)
└── 0a580004    → "edge-agent"         (10.88.0.4)
```

- Each file is named by the hex-encoded 32-bit IP address
- File content is the container ID
- Allocation: scan for lowest unused address, write atomically
- Release: find file with matching container ID content, remove
- Concurrent-safe via `sync.Mutex`
- Survives daemon restart — leases persist on disk

## Port mappings

Port mappings use nftables via the `nft` binary (one-time setup of table/chains,
then one `nft add rule` per mapping). This is the only remaining binary dependency,
and `nft` has zero CVEs in its history.

The nftables table created:

```nftables
table ip nyxd-nat {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    # per-container DNAT rules added here:
    # tcp dport 8080 dnat to 10.88.0.3:80
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    oifname "nyxbr0" masquerade   # outbound container traffic
  }
}
```

## Switching from exec CNI to native

In `cmd/nyxd/main.go`, change:

```go
// Before (exec-based CNI)
import "github.com/zrougamed/nyxd/internal/network"
net := network.NewManager("nyx", cfg.CNIConfDir, cfg.CNIBinDir)

// After (native, zero external binaries)
import "github.com/zrougamed/nyxd/internal/network/native"
net := native.NewManager(logger)
```

The supervisor calls `net.Setup()` and `net.Teardown()` — same signatures.

## Kernel modules required by the native plugin

```
bridge          # nyxbr0 bridge interface type
veth            # virtual ethernet pairs
br_netfilter    # iptables/nftables sees traffic crossing the bridge
nf_nat          # NAT connection tracking
nf_conntrack    # stateful packet tracking
nft_masq        # nftables masquerade target
nft_nat         # nftables NAT
nft_chain_nat   # nftables NAT chain hooks
```

See `docs/kernel-requirements.md` for the full module list and sysctl configuration.
