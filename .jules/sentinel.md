## 2026-01-29 - Unbounded HTTP Server Timeouts
**Vulnerability:** The API server used `http.ListenAndServe` directly, which sets no timeouts for `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`. This exposes the service to Slowloris and resource exhaustion attacks where a client connects but sends data very slowly or never completes the request.
**Learning:** Go's default `http.Server` zero-values for timeouts are "no timeout", which is insecure for public-facing or robust services. Developers often copy `http.ListenAndServe` from basic tutorials.
**Prevention:** Always instantiate `http.Server` explicitly with `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` set to reasonable values (e.g., 5-10s for APIs).
