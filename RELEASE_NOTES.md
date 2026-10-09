# HopPort v0.1.0

The first public release of HopPort, a local HTTP and SOCKS5 proxy manager.

## Highlights

- Save multiple authenticated HTTP upstream proxies, each with its own localhost exit port.
- Use each exit port as an HTTP or SOCKS5 proxy in other apps.
- Optionally connect to an upstream through a SOCKS5 or HTTP first relay, with no direct fallback if that relay fails.
- Test each exit through the app and see its public IP and response time.
- Manage proxy settings in a local web interface; the configuration stays on your computer.

## Downloads

Choose the archive matching your operating system and CPU. Extract it, run `run.sh` on Linux or macOS or `run.cmd` on Windows, then open `http://127.0.0.1:18080`.

The archives do not contain your `proxies.json`. Keep that file private: it stores proxy credentials in plaintext.

See [README.md](https://github.com/mostafa-binesh/HopVPN#readme) for setup, examples, limits, and troubleshooting. Use `SHA256SUMS` to verify downloaded archives.
