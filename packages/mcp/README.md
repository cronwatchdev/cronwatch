# @cronwatch/mcp

An MCP server for apps that use [`@cronwatch/sdk`](https://www.npmjs.com/package/@cronwatch/sdk). It talks to the JSON API the app mounts with `cw.routes()`, so Claude Code, Cursor, or any MCP client can list jobs, read a failing run's error and output, run a check, and silence a job during a fix. Every port serves the same API at the same paths, so it works against those apps too: the Ruby gem's `Cronwatch::Web`, the Python package's `cw.routes()` (or `cronwatch.django`), the PHP package's `$cw->routes()` (and its Laravel, Symfony, WordPress, Drupal, and Craft CMS integrations), the Go module's `cw.Routes()`, the Rust crate's `cw.routes(options)`, nested in axum or served as a tower service, the Elixir package's `Cronwatch.Web`, forwarded to from a Phoenix router, the Java library's `cw.routes()`, served by its Spring Boot starter, a servlet filter, or the JDK's own server, and the .NET package's `cw.Routes()`, mapped with `app.MapCronwatch()` in ASP.NET Core. It is tested against each language's dashboard.

```bash
claude mcp add cronwatch \
  -e CRONWATCH_URL=https://app.example.com/cronwatch \
  -e CRONWATCH_TOKEN=your-token \
  -- npx -y @cronwatch/mcp
```

Any stdio MCP client:

```json
{
  "mcpServers": {
    "cronwatch": {
      "command": "npx",
      "args": ["-y", "@cronwatch/mcp"],
      "env": { "CRONWATCH_URL": "https://app.example.com/cronwatch", "CRONWATCH_TOKEN": "your-token" }
    }
  }
}
```

## Tools

| Tool | |
|---|---|
| `list_jobs` | every job with health, schedule, last run, and next due |
| `get_job` | one job with recent runs: errors, output tails, metrics |
| `run_check` | look for missed and stuck runs now and send due alerts |
| `silence_job`, `unsilence_job` | pause and resume alerts for a job |
| `forget_job` | remove a job that no longer exists in the code |
| `get_setup_guide` | the code to add CronWatch to a job |

Give the token through `CRONWATCH_TOKEN`, as above. The `--url` and `--token` flags still work, but a flag is visible to anyone who can list processes (`ps`), so keep the token in the environment. Node 22 or newer.

Use an `https` URL. The token goes with every request, so the server refuses to start with a plain `http` URL unless its host is this machine (`localhost`, `127.0.0.1`, or `[::1]`). For a private network you trust, pass `--allow-http` or set `CRONWATCH_ALLOW_HTTP=1`, and it starts with a warning.

`list_jobs`, `get_job`, and `get_setup_guide` are marked read-only; `silence_job` and `forget_job` are marked destructive, so clients that ask before destructive tools will ask. `run_check` is marked open-world, since the alerts it sends leave the app. A run's error and output are written by the job, and an error's text by the dashboard, so tool results fence them as untrusted data for the model to read, not follow. Results are kept short: a line past 500 characters and a result past 40,000 are cut with a note saying so, `get_job` leaves out the older runs that do not fit, and an answer from the dashboard over 5 MB is not read. Job names are checked against the SDK's rule before any request.

The token grants everything the dashboard can do. Docs: [cronwatch.dev/docs/mcp](https://cronwatch.dev/docs/mcp/). MIT.
