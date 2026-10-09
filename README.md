<p align="center"><img src="assets/hopport-logo.png" width="128" alt="HopPort logo"></p>

# HopPort

**A local HTTP and SOCKS5 proxy manager with one exit port per upstream and an optional first relay.**

HopPort lets you save authenticated HTTP proxies, assign each one a distinct localhost port, test the complete route, and optionally reach an upstream proxy through a SOCKS5 or HTTP relay such as Hiddify. Multiple exit ports can run at the same time. The web interface stays on your computer.

> HopPort is an application-layer proxy, not an IP-layer VPN. It does not create a TUN interface or carry UDP traffic.

## Get started

1. Download the archive for your system from [Releases](https://github.com/mostafa-binesh/HopVPN/releases).
2. Extract it and start HopPort:

   | System | Command |
   | --- | --- |
   | Linux or macOS | `./run.sh` |
   | Windows | `run.cmd` |

3. Open **http://127.0.0.1:18080**.
4. Add an upstream proxy, choose a free **local exit port**, and click **Test exit**.
5. In another app, select proxy type **HTTP** or **SOCKS5**, host `127.0.0.1`, and the exit port you chose.

The launchers keep your saved proxies in `proxies.json` beside the executable. You can also run the binary directly and pass `-config /path/to/proxies.json`; without `-config`, it uses your operating system's user config directory.

## A proxy with its own local port

For example, enter:

| Field | Example |
| --- | --- |
| Name | `Work proxy` |
| Proxy URL | `http://alice:secret@proxy.example:10003` |
| Local exit port | `12338` |
| First relay | Leave empty |

Then configure another app with either `http://127.0.0.1:12338` or `socks5://127.0.0.1:12338`. Both local protocols use the same upstream for that port. You can add more upstreams with other free exit ports; they remain active together.

Quick terminal checks:

```sh
curl --proxy http://127.0.0.1:12338 https://api.ipify.org
curl --socks5-hostname 127.0.0.1:12338 https://api.ipify.org
```

## Add a first relay

The **Connect through another relay** field is optional. Enter `localhost:10808` to use a local SOCKS5 listener; a bare `host:port` is interpreted as SOCKS5. Explicit `socks5://host:port` and `http://host:port` URLs are also accepted. Both types support `username:password@` credentials.

```text
Your app → HopPort local exit → optional first relay → upstream HTTP proxy → destination
```

HopPort sends the connection to the **upstream proxy** through the first relay. The upstream proxy then handles the destination. If you configure a first relay and it fails, HopPort fails the request. It does **not** fall back to connecting to the upstream directly.

Each saved proxy has its own relay setting. Use **Save relay** or **Remove relay** on its card to change it. New connections use the new setting; existing tunnels finish on their original route.

### Testing the route

**Listening** means HopPort has opened the local exit port. It does not prove the upstream works. **Test exit** requests `https://api.ipify.org` through the local port and reports the exit IP and elapsed time. If the request fails, HopPort also probes the upstream route separately. A configured first relay remains in use for that separate probe.

If a direct upstream works but the chain through `localhost:10808` fails, inspect the first relay's route to that upstream. A working `10808` connection to other sites does not by itself prove it can reach a particular upstream proxy.

## Build from source

HopPort uses only the Go standard library. Go 1.22 or newer is required.

```sh
git clone https://github.com/mostafa-binesh/HopVPN.git
cd HopVPN
go test ./...
go build -o hopport .
./run.sh
```

On Windows, build with `go build -o hopport.exe .` and use `run.cmd`.

### Options

| Flag | Purpose | Default |
| --- | --- | --- |
| `-ui-port` | Local web interface port | `18080` |
| `-config` | Saved proxy file | OS user config directory when running the binary directly |
| `-version` | Print the version and exit | — |

For example, `./run.sh -ui-port 18081` moves only the web interface. Every proxy's exit port is configured in the web page.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Exit port is occupied | Choose another port. HopPort will reject a port already bound by another process. |
| Port says **Listening**, but requests fail | Click **Test exit**. The upstream or first relay may be unavailable even though HopPort opened the local port. |
| Another app cannot connect | Use `127.0.0.1`, the saved exit port, and proxy type HTTP or SOCKS5. Do not select an HTTPS-proxy protocol for the local HTTP listener. |
| First relay works for other sites, but this proxy chain fails | Test whether the first relay can reach the upstream proxy's host and port. HopPort will not bypass the first relay. |
| Web interface port is occupied | Start with `./run.sh -ui-port 18081` or another free port. |
| Lost the upstream password | Re-add that proxy with the correct URL. HopPort does not show saved credentials in the web page. |

## Security and limits

- The web interface and exit ports bind to `127.0.0.1` only. They have no local authentication, so other processes on your computer can use them.
- `proxies.json` contains upstream and first-relay credentials in plaintext. HopPort writes it with mode `0600` on Unix-like systems. Keep the file private and out of Git. The repository ignores it.
- HopPort does not inspect HTTPS content or install a certificate authority. It tunnels HTTPS with CONNECT.
- Upstream proxies must use the `http://` proxy protocol. First relays may use SOCKS5 or HTTP. Local SOCKS5 supports TCP CONNECT without client authentication; UDP ASSOCIATE and BIND are not supported.
- The **Test exit** action contacts `api.ipify.org`. Normal proxy traffic goes to destinations chosen by the client app.

## License

See [LICENSE](LICENSE).
